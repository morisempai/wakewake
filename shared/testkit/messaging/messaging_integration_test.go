//go:build integration

// Package messaging_test exercises the outbox -> relay -> broker -> consumer -> inbox path
// against a real Postgres and a real RabbitMQ.
//
// It lives in testkit rather than platform because testkit already depends on platform; putting
// it the other way round would make the two modules mutually dependent.
//
// These are the tests that turn ADR-0002 from a description into a demonstrated property. Until
// they ran, "state and event commit atomically", "delivery is at-least-once", and "consumers
// dedupe on the envelope id" were all prose — and the dedupe argument in particular is the kind
// that is easy to get subtly, invisibly wrong.
package messaging_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/morisempai/wakewake/shared/contracts/events"
	"github.com/morisempai/wakewake/shared/platform/broker"
	"github.com/morisempai/wakewake/shared/platform/correlation"
	"github.com/morisempai/wakewake/shared/platform/inbox"
	"github.com/morisempai/wakewake/shared/platform/outbox"
	"github.com/morisempai/wakewake/shared/platform/pgxx"
	"github.com/morisempai/wakewake/shared/testkit/amqptest"
	"github.com/morisempai/wakewake/shared/testkit/pgtest"
)

const migrations = "testdata/migrations"

// db starts Postgres and applies the outbox and inbox DDL straight from the embedded constants
// in shared/platform.
//
// Applying the embedded text rather than a committed copy means these tests also prove the DDL
// that services are told to paste is valid, executable SQL. A copy in testdata/ could drift from
// the source it was copied from, and the tests would keep passing against the stale version —
// exactly the drift this repo keeps building guards against.
func db(t *testing.T) *pgxpool.Pool {
	t.Helper()

	pool := pgtest.Postgres(t, migrations)
	ctx := context.Background()
	for name, ddl := range map[string]string{
		"outbox": outbox.MigrationSQL,
		"inbox":  inbox.MigrationSQL,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("applying the embedded %s DDL: %v", name, err)
		}
	}
	return pool
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func samplePayload() events.ReservationCreatedPayload {
	return events.ReservationCreatedPayload{
		ReservationID: uuid.New().String(),
		ResourceID:    uuid.New().String(),
		BookingID:     uuid.New().String(),
		StartsAt:      time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
		EndsAt:        time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC),
		ExpiresAt:     time.Date(2026, 8, 1, 9, 15, 0, 0, time.UTC),
	}
}

// TestOutboxRowCommitsWithTheStateChange is the atomicity claim in ADR-0002: if the transaction
// rolls back, the event must vanish with it. A dual write would leave the event behind,
// announcing a reservation that does not exist.
func TestOutboxRowCommitsWithTheStateChange(t *testing.T) {
	pool := db(t)
	ctx := correlation.WithID(context.Background(), "corr-atomicity")

	sentinel := errors.New("domain rejected this")
	err := pgxx.WithinTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO thing (id, name) VALUES ($1, $2)`, uuid.New(), "doomed"); err != nil {
			return err
		}
		if _, err := outbox.Enqueue(ctx, tx, outbox.Record{
			Event:         events.ReservationCreated,
			AggregateType: "reservation",
			AggregateID:   uuid.New().String(),
			Payload:       samplePayload(),
		}); err != nil {
			return err
		}
		return sentinel // roll it all back
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the sentinel error, got %v", err)
	}

	var things, outboxRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM thing`).Scan(&things); err != nil {
		t.Fatalf("counting things: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&outboxRows); err != nil {
		t.Fatalf("counting outbox: %v", err)
	}
	if things != 0 {
		t.Errorf("%d rows survived a rolled-back transaction", things)
	}
	if outboxRows != 0 {
		t.Errorf("%d outbox rows survived a rolled-back transaction — the event would announce "+
			"a state change that never happened", outboxRows)
	}
}

// TestOccurredAtComesFromTheDatabase pins the decision that occurred_at is transaction time.
// All rows staged in one transaction must share it, which is only true if the database supplies
// it — separate time.Now() calls would differ by microseconds.
func TestOccurredAtComesFromTheDatabase(t *testing.T) {
	pool := db(t)
	ctx := correlation.WithID(context.Background(), "corr-clock")

	var first, second outbox.Meta
	err := pgxx.WithinTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		if first, err = outbox.Enqueue(ctx, tx, outbox.Record{
			Event: events.ReservationCreated, AggregateType: "reservation",
			AggregateID: uuid.New().String(), Payload: samplePayload(),
		}); err != nil {
			return err
		}
		// A deliberate gap: with an app clock these would differ.
		time.Sleep(10 * time.Millisecond)
		second, err = outbox.Enqueue(ctx, tx, outbox.Record{
			Event: events.ReservationReleased, AggregateType: "reservation",
			AggregateID: uuid.New().String(),
			Payload: events.ReservationReleasedPayload{
				ReservationID: uuid.New().String(), ResourceID: uuid.New().String(),
				BookingID: uuid.New().String(),
				StartsAt:  time.Now().UTC(), EndsAt: time.Now().UTC().Add(time.Hour),
				Reason: events.ReleaseHoldExpired,
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("staging events: %v", err)
	}

	if !first.OccurredAt.Equal(second.OccurredAt) {
		t.Errorf("occurred_at differs within one transaction (%s vs %s) — it is coming from the "+
			"app clock, not the transaction", first.OccurredAt, second.OccurredAt)
	}
}

// TestRelayPublishesAndMarksSent walks the full path and checks the relay only marks a row
// published after the broker has confirmed it.
func TestRelayPublishesAndMarksSent(t *testing.T) {
	pool := db(t)
	conn := amqptest.Conn(t)
	ctx, cancel := context.WithCancel(correlation.WithID(context.Background(), "corr-relay"))
	defer cancel()

	stream := amqptest.Collect(t, conn, events.ReservationCreated)

	pub, err := broker.NewPublisher(conn)
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	defer func() { _ = pub.Close() }()

	payload := samplePayload()
	var meta outbox.Meta
	if err := pgxx.WithinTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		meta, err = outbox.Enqueue(ctx, tx, outbox.Record{
			Event: events.ReservationCreated, AggregateType: "reservation",
			AggregateID: payload.ReservationID, Payload: payload,
		})
		return err
	}); err != nil {
		t.Fatalf("staging: %v", err)
	}

	relay := outbox.NewRelay(pool, pub, quietLogger(), outbox.RelayConfig{PollInterval: 200 * time.Millisecond})
	go func() { _ = relay.Run(ctx) }()
	relay.Kick()

	got := amqptest.Expect(t, stream, events.ReservationCreated, 20*time.Second)

	if got.ID != meta.ID {
		t.Errorf("published envelope id %s, want %s — the relay must republish the staged id so "+
			"consumers can dedupe across retries", got.ID, meta.ID)
	}
	if got.CorrelationID != "corr-relay" {
		t.Errorf("correlation_id = %q, want corr-relay", got.CorrelationID)
	}
	if !got.OccurredAt.Equal(meta.OccurredAt.UTC()) {
		t.Errorf("occurred_at = %s, want the staged %s", got.OccurredAt, meta.OccurredAt.UTC())
	}

	decoded, err := events.PayloadOf[events.ReservationCreatedPayload](got)
	if err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	if decoded.ReservationID != payload.ReservationID {
		t.Errorf("payload round-tripped wrong: %+v", decoded)
	}

	waitFor(t, 10*time.Second, func() bool {
		var published bool
		if err := pool.QueryRow(ctx,
			`SELECT published_at IS NOT NULL FROM outbox WHERE id = $1`, meta.ID).Scan(&published); err != nil {
			return false
		}
		return published
	}, "outbox row was never marked published")
}

// TestConsumerDedupesRedelivery is the at-least-once contract, executed. The same envelope
// delivered twice must run the handler exactly once.
func TestConsumerDedupesRedelivery(t *testing.T) {
	pool := db(t)
	ctx := correlation.WithID(context.Background(), "corr-dedupe")

	env, err := events.New(events.ReservationCreated, uuid.New().String(),
		time.Now().UTC(), "corr-dedupe", samplePayload())
	if err != nil {
		t.Fatalf("building envelope: %v", err)
	}

	var handled int
	handler := func(ctx context.Context, tx pgx.Tx, e events.Envelope) error {
		handled++
		_, err := tx.Exec(ctx, `INSERT INTO thing (id, name) VALUES ($1, $2)`, uuid.New(), e.ID)
		return err
	}

	first, err := inbox.Process(ctx, pool, "test-consumer", env, handler)
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if !first {
		t.Fatal("first delivery reported as a duplicate")
	}

	second, err := inbox.Process(ctx, pool, "test-consumer", env, handler)
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if second {
		t.Error("redelivery was processed again — dedupe on the envelope id is not working")
	}
	if handled != 1 {
		t.Errorf("handler ran %d times, want exactly 1", handled)
	}

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM thing`).Scan(&rows); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if rows != 1 {
		t.Errorf("handler wrote %d rows across two deliveries, want 1", rows)
	}
}

// TestFailedHandlerLeavesNoDedupeRow is the subtle one, and the reason inbox.Process hands the
// handler a transaction at all.
//
// If the dedupe row survived a failed handler, the event would be marked processed while its
// work never happened — and redelivery would be suppressed, so the effect is lost permanently.
// For a BookingConfirmed that is a paying customer who never receives their confirmation, with
// nothing anywhere recording that it is owed.
func TestFailedHandlerLeavesNoDedupeRow(t *testing.T) {
	pool := db(t)
	ctx := correlation.WithID(context.Background(), "corr-rollback")

	env, err := events.New(events.ReservationCreated, uuid.New().String(),
		time.Now().UTC(), "corr-rollback", samplePayload())
	if err != nil {
		t.Fatalf("building envelope: %v", err)
	}

	boom := errors.New("dependency is down")
	failing := func(ctx context.Context, tx pgx.Tx, e events.Envelope) error {
		if _, err := tx.Exec(ctx, `INSERT INTO thing (id, name) VALUES ($1, $2)`, uuid.New(), "partial"); err != nil {
			return err
		}
		return boom
	}

	if _, err := inbox.Process(ctx, pool, "test-consumer", env, failing); !errors.Is(err, boom) {
		t.Fatalf("expected the handler error, got %v", err)
	}

	var deduped int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM processed_event WHERE event_id = $1`, env.ID).Scan(&deduped); err != nil {
		t.Fatalf("counting processed_event: %v", err)
	}
	if deduped != 0 {
		t.Fatal("a failed handler left a dedupe row — the event is now marked processed, its work " +
			"never happened, and redelivery is suppressed. The effect is lost permanently.")
	}

	var partial int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM thing`).Scan(&partial); err != nil {
		t.Fatalf("counting things: %v", err)
	}
	if partial != 0 {
		t.Errorf("%d rows survived a failed handler — its writes were not in the rolled-back "+
			"transaction", partial)
	}

	// And a retry must genuinely re-run it.
	var handled int
	succeeding := func(ctx context.Context, tx pgx.Tx, e events.Envelope) error {
		handled++
		_, err := tx.Exec(ctx, `INSERT INTO thing (id, name) VALUES ($1, $2)`, uuid.New(), "retry")
		return err
	}
	processed, err := inbox.Process(ctx, pool, "test-consumer", env, succeeding)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !processed || handled != 1 {
		t.Errorf("retry after failure did not re-run the handler (processed=%v handled=%d)",
			processed, handled)
	}
}

// TestDedupeIsScopedPerConsumer guards the choice to key on (consumer, event_id).
//
// Keying on event_id alone would let whichever service processed an event first suppress it for
// every other service — a bug invisible until a second consumer is added, long after the first
// one's tests were written.
func TestDedupeIsScopedPerConsumer(t *testing.T) {
	pool := db(t)
	ctx := context.Background()

	env, err := events.New(events.BookingConfirmed, uuid.New().String(),
		time.Now().UTC(), "corr-fanout", events.BookingConfirmedPayload{
			BookingID: uuid.New().String(), CustomerID: uuid.New().String(),
			ProductID: uuid.New().String(), ResourceID: uuid.New().String(),
			ReservationID: uuid.New().String(), PaymentID: uuid.New().String(),
			StartsAt: time.Now().UTC(), EndsAt: time.Now().UTC().Add(time.Hour),
			TotalMinor: 5000, Currency: "EUR",
		})
	if err != nil {
		t.Fatalf("building envelope: %v", err)
	}

	noop := func(ctx context.Context, tx pgx.Tx, e events.Envelope) error { return nil }

	for _, consumer := range []string{"notification", "analytics"} {
		processed, err := inbox.Process(ctx, pool, consumer, env, noop)
		if err != nil {
			t.Fatalf("%s: %v", consumer, err)
		}
		if !processed {
			t.Errorf("%s saw the event as already processed — dedupe is not scoped per consumer, "+
				"so one service is swallowing events meant for another", consumer)
		}
	}
}

// TestUnknownPayloadFieldsAreIgnored is forward compatibility end-to-end: a producer adding an
// optional field must not require a coordinated deploy.
func TestUnknownPayloadFieldsAreIgnored(t *testing.T) {
	pool := db(t)
	ctx := context.Background()

	env, err := events.New(events.ReservationCreated, uuid.New().String(),
		time.Now().UTC(), "corr-fwd", samplePayload())
	if err != nil {
		t.Fatalf("building envelope: %v", err)
	}
	// Simulate a newer producer.
	env.Payload = []byte(fmt.Sprintf(`{"reservation_id":%q,"resource_id":%q,"booking_id":%q,`+
		`"starts_at":"2026-08-01T10:00:00Z","ends_at":"2026-08-01T11:00:00Z",`+
		`"expires_at":"2026-08-01T09:15:00Z","field_from_the_future":"ignore me"}`,
		uuid.New(), uuid.New(), uuid.New()))

	var decoded events.ReservationCreatedPayload
	handler := func(ctx context.Context, tx pgx.Tx, e events.Envelope) error {
		var err error
		decoded, err = events.PayloadOf[events.ReservationCreatedPayload](e)
		return err
	}

	if _, err := inbox.Process(ctx, pool, "test-consumer", env, handler); err != nil {
		t.Fatalf("an unknown payload field broke the consumer: %v", err)
	}
	if decoded.ReservationID == "" {
		t.Error("known fields did not decode alongside the unknown one")
	}
}

func waitFor(t *testing.T, within time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal(msg)
}
