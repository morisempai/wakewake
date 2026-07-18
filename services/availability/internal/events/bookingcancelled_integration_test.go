//go:build integration

package events

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/morisempai/wakewake/shared/contracts/events"
	"github.com/morisempai/wakewake/shared/platform/broker"
	"github.com/morisempai/wakewake/shared/platform/consumer"
	"github.com/morisempai/wakewake/shared/platform/inbox"
	"github.com/morisempai/wakewake/shared/testkit/amqptest"
	"github.com/morisempai/wakewake/shared/testkit/eventtest"
	"github.com/morisempai/wakewake/shared/testkit/pgtest"

	"github.com/morisempai/wakewake/services/availability/internal/domain"
	"github.com/morisempai/wakewake/services/availability/internal/infra"
)

const (
	consumerName = "availability"
	holdTTL      = 15 * time.Minute
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func newID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("generating uuid: %v", err)
	}
	return id.String()
}

// fixture spins up a real Postgres with this service's migrations and returns the wired handler.
func fixture(t *testing.T) (*BookingCancelledHandler, *infra.Store, *pgxpool.Pool) {
	t.Helper()

	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolving migrations: %v", err)
	}
	pool := pgtest.Postgres(t, dir)

	store := infra.NewStore(pool, nil)
	svc := domain.NewService(store, time.Now, func() (string, error) { return newID(t), nil }, holdTTL)

	return NewBookingCancelledHandler(store, svc, discardLogger()), store, pool
}

// heldReservation inserts a live hold to compensate against.
func heldReservation(t *testing.T, store *infra.Store) domain.Reservation {
	t.Helper()

	start := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Hour)
	r, err := domain.NewReservation(newID(t), newID(t), newID(t), start, start.Add(time.Hour))
	if err != nil {
		t.Fatalf("building reservation: %v", err)
	}

	stored, err := store.Insert(context.Background(),
		domain.Claim{Key: "idem-" + newID(t), Fingerprint: "fp"}, r, holdTTL)
	if err != nil {
		t.Fatalf("inserting reservation: %v", err)
	}
	return stored
}

func cancelledEnvelope(t *testing.T, bookingID string, reservationID *string) []byte {
	t.Helper()
	return eventtest.Envelope(t, events.BookingCancelled, events.BookingCancelledPayload{
		BookingID:      bookingID,
		CustomerID:     newID(t),
		ResourceID:     newID(t),
		ReservationID:  reservationID,
		PreviousStatus: events.BookingStatusHeld,
		Reason:         events.CancelPaymentFailed,
	}, "corr-"+newID(t))
}

func countRows(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}

// process runs the handler exactly as the consumer does: inside the transaction that also
// records the event as processed.
func process(t *testing.T, pool *pgxpool.Pool, h *BookingCancelledHandler, raw []byte) (bool, error) {
	t.Helper()

	env, err := events.Parse(raw)
	if err != nil {
		t.Fatalf("parsing envelope: %v", err)
	}
	return inbox.Process(context.Background(), pool, consumerName, env, h.Handle)
}

// The compensation path: a cancelled booking frees its window and states the fact.
func TestBookingCancelledReleasesTheReservation(t *testing.T) {
	t.Parallel()

	h, store, pool := fixture(t)
	held := heldReservation(t, store)

	processed, err := process(t, pool, h, cancelledEnvelope(t, held.BookingID, &held.ID))
	if err != nil {
		t.Fatalf("handling: %v", err)
	}
	if !processed {
		t.Fatal("the first delivery was reported as a duplicate")
	}

	got, err := store.ByID(context.Background(), held.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if got.Status != domain.StatusReleased {
		t.Errorf("status = %q, want %q", got.Status, domain.StatusReleased)
	}
	if got.ReleasedReason == nil || *got.ReleasedReason != domain.ReasonBookingCancelled {
		t.Errorf("released_reason = %v, want %q", got.ReleasedReason, domain.ReasonBookingCancelled)
	}

	// One created + one released.
	if n := countRows(t, pool, "outbox"); n != 2 {
		t.Errorf("outbox holds %d rows, want 2", n)
	}
}

// Delivery is at-least-once, so duplicates are guaranteed. The second must change nothing and
// must NOT stage a second ReservationReleased — Booking cancels the abandoned booking on that
// event, so a duplicate is a second cancellation.
func TestDuplicateDeliveryReleasesOnlyOnce(t *testing.T) {
	t.Parallel()

	h, store, pool := fixture(t)
	held := heldReservation(t, store)
	raw := cancelledEnvelope(t, held.BookingID, &held.ID)

	if _, err := process(t, pool, h, raw); err != nil {
		t.Fatalf("first delivery: %v", err)
	}

	processed, err := process(t, pool, h, raw)
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if processed {
		t.Error("the redelivery ran the handler again; it must be suppressed by the envelope id")
	}

	if n := countRows(t, pool, "outbox"); n != 2 {
		t.Errorf("outbox holds %d rows after a redelivery, want 2", n)
	}
	if n := countRows(t, pool, "processed_event"); n != 1 {
		t.Errorf("processed_event holds %d rows, want 1", n)
	}
}

// Forward compatibility: an unknown field must be ignored, or every additive producer change
// becomes a coordinated deploy across services.
func TestUnknownPayloadFieldsAreIgnored(t *testing.T) {
	t.Parallel()

	h, store, pool := fixture(t)
	held := heldReservation(t, store)

	raw := eventtest.WithUnknownField(t,
		cancelledEnvelope(t, held.BookingID, &held.ID),
		"refund_reference", "re_1234567890")

	if _, err := process(t, pool, h, raw); err != nil {
		t.Fatalf("an unknown payload field was rejected: %v", err)
	}

	got, err := store.ByID(context.Background(), held.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if got.Status != domain.StatusReleased {
		t.Errorf("status = %q, want %q — the event should have been handled normally",
			got.Status, domain.StatusReleased)
	}
}

// `reservation_id: null` means the hold was already released. Nothing is owed, and the event is
// accepted rather than retried forever.
func TestACancelledBookingWithNoReservationIsAccepted(t *testing.T) {
	t.Parallel()

	h, _, pool := fixture(t)

	processed, err := process(t, pool, h, cancelledEnvelope(t, newID(t), nil))
	if err != nil {
		t.Fatalf("handling a null reservation_id returned %v, want nil", err)
	}
	if !processed {
		t.Error("the event was not processed")
	}
	if n := countRows(t, pool, "outbox"); n != 0 {
		t.Errorf("outbox holds %d rows, want 0 — nothing was released", n)
	}
}

// A reservation that no longer exists is accepted, not dead-lettered: retrying cannot conjure
// the row, and the compensation's goal — no window held for this booking — already holds.
func TestAnUnknownReservationIsAccepted(t *testing.T) {
	t.Parallel()

	h, _, pool := fixture(t)
	missing := newID(t)

	if _, err := process(t, pool, h, cancelledEnvelope(t, newID(t), &missing)); err != nil {
		t.Errorf("handling an unknown reservation returned %v, want nil", err)
	}
}

// Releasing a reservation that belongs to a DIFFERENT booking would free a live window on behalf
// of an unrelated cancellation. The handler must refuse, and the transaction must roll back so
// the release never lands.
func TestAReservationBelongingToAnotherBookingIsNotReleased(t *testing.T) {
	t.Parallel()

	h, store, pool := fixture(t)
	held := heldReservation(t, store)

	_, err := process(t, pool, h, cancelledEnvelope(t, newID(t), &held.ID))

	if err == nil {
		t.Fatal("the handler released a reservation belonging to another booking")
	}

	got, reloadErr := store.ByID(context.Background(), held.ID)
	if reloadErr != nil {
		t.Fatalf("reloading: %v", reloadErr)
	}
	if got.Status != domain.StatusHeld {
		t.Errorf("status = %q, want %q — the rejected handler's transaction must have rolled back",
			got.Status, domain.StatusHeld)
	}
	if n := countRows(t, pool, "processed_event"); n != 0 {
		t.Errorf("processed_event holds %d rows, want 0 — a failed handler must not mark the event done", n)
	}
}

// Dependency-down: a handler that keeps failing must exhaust its retries and dead-letter rather
// than requeue forever. Run against a real broker, because a fake cannot demonstrate DLQ routing.
func TestAPersistentlyFailingEventIsDeadLettered(t *testing.T) {
	t.Parallel()

	h, store, pool := fixture(t)
	held := heldReservation(t, store)
	conn := amqptest.Conn(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		_ = consumer.Run(ctx, conn, pool, discardLogger(), consumer.Options{
			Service:     consumerName,
			Events:      []string{events.BookingCancelled},
			MaxAttempts: 2,
			Backoff:     []time.Duration{10 * time.Millisecond},
		}, h.Handle)
	}()

	publisher, err := broker.NewPublisher(conn)
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	t.Cleanup(func() { _ = publisher.Close() })

	// Names the right reservation but the wrong booking, so the handler fails every time.
	raw := cancelledEnvelope(t, newID(t), &held.ID)
	env, err := events.Parse(raw)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	// Give the consumer time to declare its queue before publishing, or the message is routed
	// with mandatory=true and no binding yet.
	waitForQueue(t, conn, broker.QueueName(consumerName, events.BookingCancelled))

	if err := publisher.Publish(ctx, env, raw); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	dlq := broker.DLQName(broker.QueueName(consumerName, events.BookingCancelled))
	deadline := time.After(30 * time.Second)
	for {
		if amqptest.QueueDepth(t, conn, dlq) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("nothing reached %s; a persistently failing event must dead-letter", dlq)
		case <-time.After(200 * time.Millisecond):
		}
	}

	got, err := store.ByID(context.Background(), held.ID)
	if err != nil {
		t.Fatalf("reloading: %v", err)
	}
	if got.Status != domain.StatusHeld {
		t.Errorf("status = %q, want %q — no attempt should have committed", got.Status, domain.StatusHeld)
	}
}

// waitForQueue blocks until the consumer has declared its queue.
func waitForQueue(t *testing.T, conn *broker.Conn, queue string) {
	t.Helper()

	deadline := time.After(30 * time.Second)
	for {
		ch, err := conn.Channel()
		if err != nil {
			t.Fatalf("opening channel: %v", err)
		}
		_, declareErr := ch.QueueDeclarePassive(queue, true, false, false, false, nil)
		_ = ch.Close()
		if declareErr == nil {
			return
		}

		select {
		case <-deadline:
			t.Fatalf("queue %s was never declared", queue)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// The released event this consumer stages must match the AsyncAPI schema, validated against the
// real spec rather than a second hand-written expectation.
func TestTheStagedReleasedEventMatchesTheContract(t *testing.T) {
	t.Parallel()

	h, store, pool := fixture(t)
	held := heldReservation(t, store)

	if _, err := process(t, pool, h, cancelledEnvelope(t, held.BookingID, &held.ID)); err != nil {
		t.Fatalf("handling: %v", err)
	}

	var payload []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT payload FROM outbox WHERE event = $1`, events.ReservationReleased).Scan(&payload); err != nil {
		t.Fatalf("reading the staged event: %v", err)
	}

	eventtest.AssertValidAgainstSpec(t, events.ReservationReleased, payload)

	var decoded events.ReservationReleasedPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if decoded.Reason != events.ReleaseBookingCancelled {
		t.Errorf("reason = %q, want %q", decoded.Reason, events.ReleaseBookingCancelled)
	}
	if decoded.ReservationID != held.ID || decoded.BookingID != held.BookingID {
		t.Errorf("payload identifiers do not match the reservation: %+v", decoded)
	}
}
