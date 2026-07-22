//go:build integration

// Integration tests for the booking store, against a real Postgres.
//
// These cannot be unit tests: AC1 is the claim that the state change and its outbox row commit
// together or not at all, and a fake backed by a map cannot demonstrate a transaction rolling back.
package infra

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/morisempai/wakewake/shared/contracts/events"
	"github.com/morisempai/wakewake/shared/platform/idempotency"
	"github.com/morisempai/wakewake/shared/platform/pgxx"
	"github.com/morisempai/wakewake/shared/testkit/eventtest"
	"github.com/morisempai/wakewake/shared/testkit/pgtest"

	"github.com/morisempai/wakewake/services/booking/internal/domain"
)

func migrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolving migrations dir: %v", err)
	}
	return dir
}

func newStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	pool := pgtest.Postgres(t, migrationsDir(t))
	return NewStore(pool, nil), pool
}

func newID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("generating uuid: %v", err)
	}
	return id.String()
}

// heldBooking builds a fresh held booking to insert, with a distinct id per call.
func heldBooking(t *testing.T) domain.Booking {
	t.Helper()
	start := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Hour)
	product := domain.Product{ResourceID: newID(t), Capacity: 8, PriceMinor: 15000, Currency: "EUR"}
	reservation := domain.Reservation{ID: newID(t), BookingID: newID(t), ExpiresAt: time.Now().UTC().Add(15 * time.Minute)}
	b, err := domain.NewBooking(newID(t), newID(t), newID(t), product, start, start.Add(time.Hour), 2, reservation, time.Now().UTC())
	if err != nil {
		t.Fatalf("building booking: %v", err)
	}
	return b
}

func claim(t *testing.T) domain.Claim {
	t.Helper()
	return domain.Claim{Key: "idem-" + newID(t), Fingerprint: "fp-" + newID(t)}
}

func countRows(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}

// ---------------------------------------------------------------------------
// AC1 — the outbox row and the state change share one transaction
// ---------------------------------------------------------------------------

// A create stages exactly one BookingHeld in the same transaction, carrying the contract payload —
// validated against the real AsyncAPI schema, not a second hand-written expectation.
func TestInsertStagesBookingHeldInTheSameTransaction_Issue14_AC1(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	got, err := store.Insert(context.Background(), claim(t), heldBooking(t))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	if n := countRows(t, pool, "booking"); n != 1 {
		t.Fatalf("booking table holds %d rows, want 1", n)
	}

	var (
		event       string
		aggregateID string
		payload     []byte
		publishedAt *time.Time
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT event, aggregate_id, payload, published_at FROM outbox`).
		Scan(&event, &aggregateID, &payload, &publishedAt); err != nil {
		t.Fatalf("reading outbox: %v", err)
	}
	if event != events.BookingHeld {
		t.Errorf("event = %q, want BookingHeld", event)
	}
	if aggregateID != got.ID {
		t.Errorf("aggregate_id = %q, want the booking id %q", aggregateID, got.ID)
	}
	if publishedAt != nil {
		t.Error("published_at is already set; the relay must set it only after a broker confirm")
	}
	eventtest.AssertValidAgainstSpec(t, events.BookingHeld, payload)

	var decoded events.BookingHeldPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	if decoded.BookingID != got.ID || decoded.ReservationID != *got.ReservationID {
		t.Errorf("payload identifiers do not match the row: %+v", decoded)
	}
}

// The "or roll back together" half: a transaction that inserts the booking and stages the event and
// then fails must leave NOTHING behind — no booking, no outbox row, no idempotency claim. This is
// the half a happy-path test never touches, and the reason AC1 exists.
func TestARolledBackHoldLeavesNoBookingAndNoEvent_Issue14_AC1(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	ctx := context.Background()
	b := heldBooking(t)
	c := claim(t)

	sentinel := errors.New("simulated crash after staging the event")
	err := pgxx.WithinTx(ctx, pool, func(tx pgx.Tx) error {
		if _, _, err := idempotency.Claim(ctx, tx, c.Key, c.Fingerprint, b.ID); err != nil {
			return err
		}
		if _, err := scanOne(tx.QueryRow(ctx, insertSQL,
			b.ID, b.CustomerID, b.ProductID, b.ResourceID, b.ReservationID,
			b.StartsAt, b.EndsAt, b.PartySize, b.HoldExpiresAt, b.TotalMinor, b.Currency)); err != nil {
			return err
		}
		if err := stage(ctx, tx, b.HeldEmission()); err != nil {
			return err
		}
		// Everything above is durable-in-progress; now the transaction dies.
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithinTx returned %v, want the sentinel", err)
	}

	if n := countRows(t, pool, "booking"); n != 0 {
		t.Errorf("booking table holds %d rows, want 0 — the rollback did not discard the state change", n)
	}
	if n := countRows(t, pool, "outbox"); n != 0 {
		t.Errorf("outbox holds %d rows, want 0 — the event survived a rolled-back transaction", n)
	}
	if n := countRows(t, pool, "idempotency_key"); n != 0 {
		t.Errorf("idempotency_key holds %d rows, want 0 — the claim survived the rollback", n)
	}

	// And because it rolled back, the booking id is free: a genuine retry inserts cleanly.
	if _, err := store.Insert(ctx, c, b); err != nil {
		t.Errorf("retry after rollback returned %v; the rolled-back state is still present", err)
	}
}

// ---------------------------------------------------------------------------
// AC2 — idempotency
// ---------------------------------------------------------------------------

// Same key, same body: the original booking comes back, no second row, no second event.
func TestReplayingAnIdempotencyKeyReturnsTheOriginalAndWritesNothing_Issue14_AC2(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	ctx := context.Background()
	b := heldBooking(t)
	c := claim(t)

	first, err := store.Insert(ctx, c, b)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}

	second, err := store.Insert(ctx, c, b)
	if err != nil {
		t.Fatalf("replay returned %v, want the original booking", err)
	}

	if second.ID != first.ID {
		t.Errorf("replay returned id %s, want the original %s", second.ID, first.ID)
	}
	if got := countRows(t, pool, "booking"); got != 1 {
		t.Errorf("booking table holds %d rows after a replay, want 1", got)
	}
	if got := countRows(t, pool, "outbox"); got != 1 {
		t.Errorf("outbox holds %d rows after a replay, want 1 — the fact happened once", got)
	}
}

// Same key, different body: the store surfaces ErrIdempotencyKeyReuse and writes nothing new.
func TestReusingAnIdempotencyKeyWithADifferentBodyIsRejected_Issue14_AC2(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	ctx := context.Background()
	c := claim(t)

	if _, err := store.Insert(ctx, c, heldBooking(t)); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	reused := domain.Claim{Key: c.Key, Fingerprint: "a-completely-different-body"}
	_, err := store.Insert(ctx, reused, heldBooking(t))
	if !errors.Is(err, domain.ErrIdempotencyKeyReuse) {
		t.Fatalf("error = %v, want ErrIdempotencyKeyReuse", err)
	}
	if got := countRows(t, pool, "booking"); got != 1 {
		t.Errorf("booking table holds %d rows, want 1 — a rejected reuse must write nothing", got)
	}
}

// Two concurrent requests carrying the SAME key must serialise on idempotency_key's primary key and
// produce exactly one booking, not two holds.
func TestConcurrentRequestsWithOneKeyProduceOneBooking_Issue14_AC2(t *testing.T) {
	t.Parallel()

	const concurrency = 4

	store, pool := newStore(t)
	ctx := context.Background()
	c := claim(t)
	// All callers converge on the same booking id, exactly as CreateBooking does once it adopts the
	// reservation's booking id — so the idempotency table, not a per-caller id, decides the winner.
	shared := heldBooking(t)

	var (
		release = make(chan struct{})
		ready   sync.WaitGroup
		done    sync.WaitGroup
		mu      sync.Mutex
		ids     = map[string]int{}
		errs    []error
	)
	ready.Add(concurrency)
	done.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func() {
			defer done.Done()
			ready.Done()
			<-release

			got, err := store.Insert(ctx, c, shared)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			ids[got.ID]++
		}()
	}

	ready.Wait()
	close(release)
	done.Wait()

	for _, err := range errs {
		t.Errorf("same-key request failed with %v; every one should have returned the winner's booking", err)
	}
	if len(ids) != 1 {
		t.Errorf("same-key requests produced %d distinct bookings, want 1: %v", len(ids), ids)
	}
	if got := countRows(t, pool, "booking"); got != 1 {
		t.Errorf("booking table holds %d rows, want 1", got)
	}
	if got := countRows(t, pool, "outbox"); got != 1 {
		t.Errorf("outbox holds %d rows, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// Apply — the cancel path stages one event, and a second cancel stages none
// ---------------------------------------------------------------------------

func TestCancelStagesOneEventAndASecondCancelStagesNone(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	ctx := context.Background()
	held, err := store.Insert(ctx, claim(t), heldBooking(t))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	cancelOnce := func(reason domain.CancelReason) bool {
		t.Helper()
		_, changed, err := store.Apply(ctx, held.ID, func(b domain.Booking) (domain.Booking, []domain.Emission, error) {
			return b.Cancel(reason, nil, time.Now().UTC())
		})
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		return changed
	}

	if !cancelOnce(domain.ReasonCustomerCancelled) {
		t.Fatal("first cancel reported no change")
	}
	if cancelOnce(domain.ReasonOperatorCancelled) {
		t.Error("second cancel reported a change; cancelled is terminal")
	}

	// One BookingHeld from the insert, one BookingCancelled from the first cancel.
	if got := countRows(t, pool, "outbox"); got != 2 {
		t.Fatalf("outbox holds %d rows, want 2 (held + cancelled)", got)
	}

	var payload []byte
	if err := pool.QueryRow(ctx,
		`SELECT payload FROM outbox WHERE event = $1`, events.BookingCancelled).Scan(&payload); err != nil {
		t.Fatalf("reading the cancelled event: %v", err)
	}
	eventtest.AssertValidAgainstSpec(t, events.BookingCancelled, payload)

	var decoded events.BookingCancelledPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if decoded.Reason != events.CancelCustomerCancelled {
		t.Errorf("reason = %q, want the first cancel's customer_cancelled", decoded.Reason)
	}
	if decoded.PreviousStatus != events.BookingStatusHeld {
		t.Errorf("previous_status = %q, want held", decoded.PreviousStatus)
	}
}
