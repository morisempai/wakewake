//go:build integration

package sweeper

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
	"github.com/morisempai/wakewake/shared/platform/outbox"
	"github.com/morisempai/wakewake/shared/testkit/amqptest"
	"github.com/morisempai/wakewake/shared/testkit/eventtest"
	"github.com/morisempai/wakewake/shared/testkit/pgtest"

	"github.com/morisempai/wakewake/services/availability/internal/domain"
	"github.com/morisempai/wakewake/services/availability/internal/infra"
)

const holdTTL = 15 * time.Minute

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func newID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("generating uuid: %v", err)
	}
	return id.String()
}

func fixture(t *testing.T) (*domain.Service, *infra.Store, *pgxpool.Pool) {
	t.Helper()

	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolving migrations: %v", err)
	}
	pool := pgtest.Postgres(t, dir)

	store := infra.NewStore(pool, nil)
	svc := domain.NewService(store, time.Now, func() (string, error) { return newID(t), nil }, holdTTL)
	return svc, store, pool
}

// hold inserts a reservation whose TTL is `ttl` from the DATABASE clock. A negative ttl produces
// a hold that is already past due at commit — which is how these tests avoid sleeping for
// fifteen minutes to observe an expiry.
func hold(t *testing.T, store *infra.Store, offsetHours int, ttl time.Duration) domain.Reservation {
	t.Helper()

	start := time.Now().UTC().Add(time.Duration(offsetHours) * time.Hour).Truncate(time.Hour)
	r, err := domain.NewReservation(newID(t), newID(t), newID(t), start, start.Add(time.Hour))
	if err != nil {
		t.Fatalf("building reservation: %v", err)
	}

	stored, err := store.Insert(context.Background(),
		domain.Claim{Key: "idem-" + newID(t), Fingerprint: "fp"}, r, ttl)
	if err != nil {
		t.Fatalf("inserting reservation: %v", err)
	}
	return stored
}

// AC2, end to end: a hold past its expiry is released with reason hold_expired and
// ReservationReleased actually reaches the broker.
//
// The relay is run for real rather than asserting on the outbox row alone. "Published" is the
// claim AC2 makes, and an outbox row is a promise to publish, not a publication — a topology or
// confirm bug would leave the row staged and this test green if it stopped at the table.
func TestTheSweeperReleasesAnExpiredHoldAndPublishesReservationReleased_Issue5_AC2(t *testing.T) {
	t.Parallel()

	svc, store, pool := fixture(t)
	conn := amqptest.Conn(t)

	// Bound BEFORE anything is published: a collector attached afterwards would miss the event
	// and report a failure that is really a race in the test.
	stream := amqptest.Collect(t, conn, events.ReservationReleased)

	expired := hold(t, store, 24, -time.Minute)
	live := hold(t, store, 48, holdTTL)

	publisher, err := broker.NewPublisher(conn)
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	t.Cleanup(func() { _ = publisher.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	relay := outbox.NewRelay(pool, publisher, discardLogger(), outbox.RelayConfig{
		PollInterval: 100 * time.Millisecond,
	})
	go func() { _ = relay.Run(ctx) }()

	released, err := svc.Sweep(ctx, 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if released != 1 {
		t.Fatalf("sweep released %d holds, want 1", released)
	}

	// The expired hold is now terminal, with the sweeper's fixed reason.
	got, err := store.ByID(ctx, expired.ID)
	if err != nil {
		t.Fatalf("reloading the expired hold: %v", err)
	}
	if got.Status != domain.StatusReleased {
		t.Errorf("status = %q, want %q", got.Status, domain.StatusReleased)
	}
	if got.ReleasedReason == nil || *got.ReleasedReason != domain.ReasonHoldExpired {
		t.Errorf("released_reason = %v, want %q", got.ReleasedReason, domain.ReasonHoldExpired)
	}
	if got.ExpiresAt != nil {
		t.Errorf("expires_at = %v, want nil once released", *got.ExpiresAt)
	}

	// The live hold must be untouched.
	stillHeld, err := store.ByID(ctx, live.ID)
	if err != nil {
		t.Fatalf("reloading the live hold: %v", err)
	}
	if stillHeld.Status != domain.StatusHeld {
		t.Errorf("the live hold's status = %q, want %q", stillHeld.Status, domain.StatusHeld)
	}

	env := amqptest.Expect(t, stream, events.ReservationReleased, 30*time.Second)

	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("re-marshalling the envelope: %v", err)
	}
	eventtest.AssertEnvelope(t, raw, events.ReservationReleased, "")
	eventtest.AssertValidAgainstSpec(t, events.ReservationReleased, env.Payload)

	var payload events.ReservationReleasedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	if payload.Reason != events.ReleaseHoldExpired {
		t.Errorf("reason = %q, want %q — Booking reacts to exactly this value", payload.Reason, events.ReleaseHoldExpired)
	}
	if payload.ReservationID != expired.ID {
		t.Errorf("reservation_id = %q, want %q", payload.ReservationID, expired.ID)
	}
	if payload.ResourceID != expired.ResourceID || payload.BookingID != expired.BookingID {
		t.Errorf("payload identifiers do not match the swept reservation: %+v", payload)
	}
	if !payload.StartsAt.Equal(expired.StartsAt) || !payload.EndsAt.Equal(expired.EndsAt) {
		t.Errorf("payload window = [%s, %s), want [%s, %s)",
			payload.StartsAt, payload.EndsAt, expired.StartsAt, expired.EndsAt)
	}
}

// The swept window must actually be bookable again. This is ADR-0011's whole point: with the
// total constraint ADR-0003 specified, the sweeper would free a slot that could then never be
// re-booked, and the abandoned-checkout protection would become an inventory leak.
func TestASweptWindowCanBeReserved_Issue5_AC2(t *testing.T) {
	t.Parallel()

	svc, store, _ := fixture(t)
	ctx := context.Background()

	expired := hold(t, store, 24, -time.Minute)

	if _, err := svc.Sweep(ctx, 100); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	retry, err := domain.NewReservation(newID(t), expired.ResourceID, newID(t), expired.StartsAt, expired.EndsAt)
	if err != nil {
		t.Fatalf("building the retry reservation: %v", err)
	}
	if _, err := store.Insert(ctx, domain.Claim{Key: "idem-" + newID(t), Fingerprint: "fp"}, retry, holdTTL); err != nil {
		t.Fatalf("reserving the swept window returned %v; the released row is still blocking it", err)
	}
}

// Two sweeper replicas racing on one hold must produce ONE release and one no-op. Booking
// cancels the abandoned booking on hold_expired, so a duplicate would cancel twice.
func TestConcurrentSweepersReleaseAHoldOnlyOnce_Issue5_AC2(t *testing.T) {
	t.Parallel()

	svc, store, pool := fixture(t)
	ctx := context.Background()

	hold(t, store, 24, -time.Minute)

	type result struct {
		released int
		err      error
	}
	results := make(chan result, 2)
	start := make(chan struct{})

	for i := 0; i < 2; i++ {
		go func() {
			<-start
			n, err := svc.Sweep(ctx, 100)
			results <- result{released: n, err: err}
		}()
	}
	close(start)

	total := 0
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err != nil {
			t.Errorf("sweep returned %v", r.err)
		}
		total += r.released
	}

	if total != 1 {
		t.Errorf("the two sweepers released %d holds between them, want exactly 1", total)
	}

	var releasedEvents int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox WHERE event = $1`, events.ReservationReleased).Scan(&releasedEvents); err != nil {
		t.Fatalf("counting staged events: %v", err)
	}
	if releasedEvents != 1 {
		t.Errorf("%d ReservationReleased rows were staged, want 1", releasedEvents)
	}
}

// The sweeper drains a backlog rather than releasing one batch per tick. After an outage there
// may be far more due holds than a batch, and every one of those slots is unbookable until it
// is swept.
func TestASweepDrainsMoreThanOneBatch(t *testing.T) {
	t.Parallel()

	svc, store, _ := fixture(t)

	const holds = 5
	for i := 0; i < holds; i++ {
		hold(t, store, 24+i, -time.Minute)
	}

	s := New(svc, discardLogger(), time.Hour, 2) // batch smaller than the backlog
	s.sweepOnce(context.Background())

	remaining, err := store.ExpiredHolds(context.Background(), 100)
	if err != nil {
		t.Fatalf("ExpiredHolds: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("%d holds are still due after one pass; the sweep did not drain the backlog", len(remaining))
	}
}
