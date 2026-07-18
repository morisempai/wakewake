//go:build integration

// Integration tests for the reservation store, against a real Postgres.
//
// These cannot be unit tests and the distinction is not pedantry. The no-double-booking
// invariant IS a Postgres exclusion constraint (ADR-0003): a fake repository backed by a map
// would happily "prove" a guarantee that only the database actually provides, and the test would
// stay green through a migration that dropped the constraint entirely.
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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/morisempai/wakewake/shared/contracts/events"
	"github.com/morisempai/wakewake/shared/platform/idempotency"
	"github.com/morisempai/wakewake/shared/testkit/eventtest"
	"github.com/morisempai/wakewake/shared/testkit/pgtest"

	"github.com/morisempai/wakewake/services/availability/internal/domain"
)

const holdTTL = 15 * time.Minute

func migrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolving migrations dir: %v", err)
	}
	return dir
}

// newStore starts a Postgres with this service's real migrations applied. The migrations are the
// subject under test as much as the Go code is.
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

func window(offsetHours int) (time.Time, time.Time) {
	start := time.Now().UTC().Add(time.Duration(offsetHours) * time.Hour).Truncate(time.Hour)
	return start, start.Add(time.Hour)
}

func reservation(t *testing.T, resourceID string, start, end time.Time) domain.Reservation {
	t.Helper()
	r, err := domain.NewReservation(newID(t), resourceID, newID(t), start, end)
	if err != nil {
		t.Fatalf("building reservation: %v", err)
	}
	return r
}

func claim(t *testing.T) domain.Claim {
	t.Helper()
	return domain.Claim{Key: "idem-" + newID(t), Fingerprint: "fingerprint-" + newID(t)}
}

// countRows is used to assert that a rejected or replayed request wrote nothing.
func countRows(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}

// ---------------------------------------------------------------------------
// AC1 — the crux. This is the test the M1 pipeline-status job said did not exist.
// ---------------------------------------------------------------------------

// N concurrent reserves of overlapping windows on one resource: exactly one wins, the rest get
// ErrSlotUnavailable, and the database is what decided.
//
// The goroutines are released together by a shared start channel rather than being launched in a
// loop. Without that they serialise on goroutine startup, the first commits before the second
// begins, and the test passes without ever creating the contention it claims to test.
func TestConcurrentReservesOfOneWindowLeaveExactlyOneWinner_Issue5_AC1(t *testing.T) {
	t.Parallel()

	const concurrency = 8

	store, pool := newStore(t)
	ctx := context.Background()
	resourceID := newID(t)
	start, end := window(24)

	var (
		release = make(chan struct{})
		ready   sync.WaitGroup
		done    sync.WaitGroup
		mu      sync.Mutex
		winners []domain.Reservation
		losers  int
		others  []error
	)

	ready.Add(concurrency)
	done.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func() {
			defer done.Done()
			ready.Done()
			<-release

			got, err := store.Insert(ctx, claim(t), reservation(t, resourceID, start, end), holdTTL)

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners = append(winners, got)
			case errors.Is(err, domain.ErrSlotUnavailable):
				losers++
			default:
				others = append(others, err)
			}
		}()
	}

	ready.Wait()
	close(release)
	done.Wait()

	for _, err := range others {
		t.Errorf("unexpected error: %v", err)
	}
	if len(winners) != 1 {
		t.Fatalf("%d callers won the slot, want exactly 1", len(winners))
	}
	if losers != concurrency-1 {
		t.Errorf("%d callers got ErrSlotUnavailable, want %d", losers, concurrency-1)
	}

	// The database, not the counters above, is the authority on what happened.
	if got := countRows(t, pool, "reservation"); got != 1 {
		t.Errorf("reservation table holds %d rows, want 1", got)
	}
	if got := countRows(t, pool, "outbox"); got != 1 {
		t.Errorf("outbox holds %d rows, want 1 — a losing reserve must stage nothing", got)
	}
	if winners[0].Status != domain.StatusHeld {
		t.Errorf("winner status = %q, want %q", winners[0].Status, domain.StatusHeld)
	}
	if winners[0].ExpiresAt == nil {
		t.Error("winner has no expires_at; the sweeper would never see it")
	}
}

// The constraint is partial (ADR-0011): a released reservation must not keep its window locked.
// This is the failure that would otherwise only appear in production, as "the calendar says full
// but we have no bookings".
func TestAReleasedReservationFreesItsWindow_ADR0011(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	ctx := context.Background()
	resourceID := newID(t)
	start, end := window(48)

	first, err := store.Insert(ctx, claim(t), reservation(t, resourceID, start, end), holdTTL)
	if err != nil {
		t.Fatalf("first reserve: %v", err)
	}

	// The window is genuinely blocked while the hold is live.
	if _, err := store.Insert(ctx, claim(t), reservation(t, resourceID, start, end), holdTTL); !errors.Is(err, domain.ErrSlotUnavailable) {
		t.Fatalf("second reserve while held returned %v, want ErrSlotUnavailable", err)
	}

	if _, _, err := store.Apply(ctx, first.ID, func(r domain.Reservation) (domain.Reservation, []domain.Emission, error) {
		return r.Release(domain.ReasonHoldExpired, time.Now().UTC())
	}); err != nil {
		t.Fatalf("releasing: %v", err)
	}

	if _, err := store.Insert(ctx, claim(t), reservation(t, resourceID, start, end), holdTTL); err != nil {
		t.Fatalf("reserve after release returned %v; the released row is still poisoning its window", err)
	}
}

// Half-open '[)' bounds: 10:00–11:00 and 11:00–12:00 share an endpoint and must both be bookable.
// A closed range would reject the second and quietly lose the revenue.
func TestBackToBackWindowsDoNotOverlap(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	ctx := context.Background()
	resourceID := newID(t)
	start, mid := window(72)
	end := mid.Add(time.Hour)

	if _, err := store.Insert(ctx, claim(t), reservation(t, resourceID, start, mid), holdTTL); err != nil {
		t.Fatalf("first window: %v", err)
	}
	if _, err := store.Insert(ctx, claim(t), reservation(t, resourceID, mid, end), holdTTL); err != nil {
		t.Fatalf("back-to-back window rejected: %v", err)
	}
}

// The constraint is scoped to one resource: two rooms can be booked for the same hour.
func TestDifferentResourcesShareTheSameWindow(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	ctx := context.Background()
	start, end := window(96)

	if _, err := store.Insert(ctx, claim(t), reservation(t, newID(t), start, end), holdTTL); err != nil {
		t.Fatalf("first resource: %v", err)
	}
	if _, err := store.Insert(ctx, claim(t), reservation(t, newID(t), start, end), holdTTL); err != nil {
		t.Fatalf("second resource rejected: %v", err)
	}
}

// Partial overlaps must be rejected too, not just exact repeats.
func TestPartiallyOverlappingWindowsAreRejected(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	ctx := context.Background()
	resourceID := newID(t)
	start, end := window(120)

	if _, err := store.Insert(ctx, claim(t), reservation(t, resourceID, start, end), holdTTL); err != nil {
		t.Fatalf("first window: %v", err)
	}

	cases := []struct {
		name         string
		start, end   time.Time
		wantRejected bool
	}{
		{"starts inside", start.Add(30 * time.Minute), end.Add(30 * time.Minute), true},
		{"ends inside", start.Add(-30 * time.Minute), start.Add(30 * time.Minute), true},
		{"strictly contains", start.Add(-time.Hour), end.Add(time.Hour), true},
		{"strictly contained", start.Add(15 * time.Minute), end.Add(-15 * time.Minute), true},
		{"entirely before", start.Add(-2 * time.Hour), start.Add(-time.Hour), false},
		{"entirely after", end.Add(time.Hour), end.Add(2 * time.Hour), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.Insert(ctx, claim(t), reservation(t, resourceID, tc.start, tc.end), holdTTL)
			rejected := errors.Is(err, domain.ErrSlotUnavailable)
			if rejected != tc.wantRejected {
				t.Errorf("rejected = %v (err %v), want %v", rejected, err, tc.wantRejected)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// AC3 — idempotency
// ---------------------------------------------------------------------------

// Same key, same body: the original reservation comes back, no second row, no second event.
func TestReplayingAnIdempotencyKeyReturnsTheOriginalAndWritesNothing_Issue5_AC3(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	ctx := context.Background()
	resourceID := newID(t)
	start, end := window(24)
	key := claim(t)

	first, err := store.Insert(ctx, key, reservation(t, resourceID, start, end), holdTTL)
	if err != nil {
		t.Fatalf("first reserve: %v", err)
	}

	// A retry mints a fresh reservation id, exactly as the service would — the replay must be
	// decided by the key, not by the caller happening to reuse an id.
	second, err := store.Insert(ctx, key, reservation(t, resourceID, start, end), holdTTL)
	if err != nil {
		t.Fatalf("replay returned %v, want the original reservation", err)
	}

	if second.ID != first.ID {
		t.Errorf("replay returned id %s, want the original %s", second.ID, first.ID)
	}
	if second.Status != first.Status {
		t.Errorf("replay status = %q, want %q", second.Status, first.Status)
	}
	if got := countRows(t, pool, "reservation"); got != 1 {
		t.Errorf("reservation table holds %d rows after a replay, want 1", got)
	}
	if got := countRows(t, pool, "outbox"); got != 1 {
		t.Errorf("outbox holds %d rows after a replay, want 1 — the fact happened once", got)
	}
}

// Same key, different body: 409 idempotency_key_reuse, and nothing is written.
func TestReusingAnIdempotencyKeyWithADifferentBodyIsRejected_Issue5_AC3(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	ctx := context.Background()
	resourceID := newID(t)
	start, end := window(24)

	key := claim(t)
	if _, err := store.Insert(ctx, key, reservation(t, resourceID, start, end), holdTTL); err != nil {
		t.Fatalf("first reserve: %v", err)
	}

	reused := domain.Claim{Key: key.Key, Fingerprint: "a-completely-different-body"}
	otherStart, otherEnd := window(200)

	_, err := store.Insert(ctx, reused, reservation(t, newID(t), otherStart, otherEnd), holdTTL)

	if !errors.Is(err, domain.ErrIdempotencyKeyReuse) {
		t.Fatalf("error = %v, want ErrIdempotencyKeyReuse", err)
	}
	if got := countRows(t, pool, "reservation"); got != 1 {
		t.Errorf("reservation table holds %d rows, want 1 — a rejected reuse must write nothing", got)
	}
}

// Two concurrent requests carrying the SAME key must serialise on idempotency_key's primary key,
// not collide on the exclusion constraint. If they collided there, a client retrying its own
// request would be told the slot is taken — by itself.
func TestConcurrentRequestsWithOneKeyProduceOneReservation_Issue5_AC3(t *testing.T) {
	t.Parallel()

	const concurrency = 4

	store, pool := newStore(t)
	ctx := context.Background()
	resourceID := newID(t)
	start, end := window(24)
	key := claim(t)

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

			got, err := store.Insert(ctx, key, reservation(t, resourceID, start, end), holdTTL)

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
		t.Errorf("same-key request failed with %v; every one should have returned the winner's reservation", err)
	}
	if len(ids) != 1 {
		t.Errorf("same-key requests produced %d distinct reservations, want 1: %v", len(ids), ids)
	}
	if got := countRows(t, pool, "reservation"); got != 1 {
		t.Errorf("reservation table holds %d rows, want 1", got)
	}
	if got := countRows(t, pool, "outbox"); got != 1 {
		t.Errorf("outbox holds %d rows, want 1", got)
	}
}

// The deferred foreign key is what allows the key to be claimed before the reservation exists.
// A non-deferred one would reject that insert and take the serialisation mechanism with it.
func TestTheIdempotencyKeyIsClaimedBeforeTheReservationExists(t *testing.T) {
	t.Parallel()

	_, pool := newStore(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Claiming against a reservation id that does not exist yet must succeed: the FK is only
	// checked at COMMIT.
	if _, _, err := idempotency.Claim(ctx, tx, "key-"+newID(t), "fp", newID(t)); err != nil {
		t.Fatalf("claiming before the reservation exists returned %v; is the FK still DEFERRABLE INITIALLY DEFERRED?", err)
	}
}

// ---------------------------------------------------------------------------
// AC4 — the outbox row and the state change share one transaction
// ---------------------------------------------------------------------------

// Exactly one outbox row per create, staged in the same transaction, carrying the contract
// payload — validated against the real AsyncAPI schema rather than against a second hand-written
// expectation that could share the same mistake.
func TestCreateStagesExactlyOneOutboxRowInTheSameTransaction_Issue5_AC4(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	ctx := context.Background()
	start, end := window(24)

	got, err := store.Insert(ctx, claim(t), reservation(t, newID(t), start, end), holdTTL)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	rows := outboxRows(t, pool)
	if len(rows) != 1 {
		t.Fatalf("outbox holds %d rows, want exactly 1", len(rows))
	}
	row := rows[0]

	if row.event != events.ReservationCreated {
		t.Errorf("event = %q, want %q", row.event, events.ReservationCreated)
	}
	if row.aggregateID != got.ID {
		t.Errorf("aggregate_id = %q, want the reservation id %q", row.aggregateID, got.ID)
	}
	if row.publishedAt != nil {
		t.Error("published_at is already set; the relay must set it only after a broker confirm")
	}

	eventtest.AssertValidAgainstSpec(t, events.ReservationCreated, row.payload)

	var payload events.ReservationCreatedPayload
	if err := json.Unmarshal(row.payload, &payload); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	if payload.ReservationID != got.ID || payload.ResourceID != got.ResourceID || payload.BookingID != got.BookingID {
		t.Errorf("payload identifiers do not match the row: %+v", payload)
	}
	if payload.ExpiresAt.IsZero() {
		t.Error("payload expires_at is zero; it must come from the persisted row")
	}
	if got.ExpiresAt == nil || !payload.ExpiresAt.Equal(*got.ExpiresAt) {
		t.Errorf("payload expires_at = %s, want the row's %v", payload.ExpiresAt, got.ExpiresAt)
	}
}

// A failed reserve must leave NOTHING behind — no reservation, no outbox row, no idempotency
// claim. This is the "or roll back together" half of the atomicity claim, and it is the half a
// happy-path test never touches.
func TestALostRaceStagesNoEventAndClaimsNoKey_Issue5_AC4(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	ctx := context.Background()
	resourceID := newID(t)
	start, end := window(24)

	if _, err := store.Insert(ctx, claim(t), reservation(t, resourceID, start, end), holdTTL); err != nil {
		t.Fatalf("first reserve: %v", err)
	}

	loser := claim(t)
	if _, err := store.Insert(ctx, loser, reservation(t, resourceID, start, end), holdTTL); !errors.Is(err, domain.ErrSlotUnavailable) {
		t.Fatalf("second reserve returned %v, want ErrSlotUnavailable", err)
	}

	if got := countRows(t, pool, "outbox"); got != 1 {
		t.Errorf("outbox holds %d rows, want 1 — the losing reserve must have staged nothing", got)
	}
	if got := countRows(t, pool, "idempotency_key"); got != 1 {
		t.Errorf("idempotency_key holds %d rows, want 1 — the loser's claim must have rolled back", got)
	}

	// And because it rolled back, the loser's key is still usable for a genuine retry.
	otherStart, otherEnd := window(300)
	if _, err := store.Insert(ctx, loser, reservation(t, resourceID, otherStart, otherEnd), holdTTL); err != nil {
		t.Errorf("retrying with the loser's key returned %v; the rolled-back claim is still held", err)
	}
}

// Releasing stages exactly one ReservationReleased, and releasing again stages none.
func TestReleaseStagesOneEventAndASecondReleaseStagesNone_Issue5_AC4(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	ctx := context.Background()
	start, end := window(24)

	held, err := store.Insert(ctx, claim(t), reservation(t, newID(t), start, end), holdTTL)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	releaseOnce := func(reason domain.ReleaseReason) bool {
		t.Helper()
		_, changed, err := store.Apply(ctx, held.ID, func(r domain.Reservation) (domain.Reservation, []domain.Emission, error) {
			return r.Release(reason, time.Now().UTC())
		})
		if err != nil {
			t.Fatalf("release: %v", err)
		}
		return changed
	}

	if !releaseOnce(domain.ReasonPaymentFailed) {
		t.Fatal("first release reported no change")
	}
	if releaseOnce(domain.ReasonBookingCancelled) {
		t.Error("second release reported a change; releasing is terminal")
	}

	rows := outboxRows(t, pool)
	if len(rows) != 2 {
		t.Fatalf("outbox holds %d rows, want 2 (one created, one released)", len(rows))
	}

	released := rows[1]
	if released.event != events.ReservationReleased {
		t.Fatalf("second event = %q, want %q", released.event, events.ReservationReleased)
	}
	eventtest.AssertValidAgainstSpec(t, events.ReservationReleased, released.payload)

	var payload events.ReservationReleasedPayload
	if err := json.Unmarshal(released.payload, &payload); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	if payload.Reason != events.ReleasePaymentFailed {
		t.Errorf("reason = %q, want the FIRST release's %q — a re-release must not rewrite history",
			payload.Reason, events.ReleasePaymentFailed)
	}
}

// Confirming changes state but emits nothing: the AsyncAPI contract has no ReservationConfirmed.
func TestConfirmStagesNoEvent(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	ctx := context.Background()
	start, end := window(24)

	held, err := store.Insert(ctx, claim(t), reservation(t, newID(t), start, end), holdTTL)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	confirmed, changed, err := store.Apply(ctx, held.ID, func(r domain.Reservation) (domain.Reservation, []domain.Emission, error) {
		return r.Confirm(time.Now().UTC())
	})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if !changed {
		t.Error("confirm reported no change")
	}
	if confirmed.Status != domain.StatusConfirmed {
		t.Errorf("status = %q, want %q", confirmed.Status, domain.StatusConfirmed)
	}
	// The schema's held-iff-expiry CHECK would have rejected the row if this were still set.
	if confirmed.ExpiresAt != nil {
		t.Errorf("expires_at = %v, want nil once confirmed", *confirmed.ExpiresAt)
	}
	if got := countRows(t, pool, "outbox"); got != 1 {
		t.Errorf("outbox holds %d rows, want 1 — confirming emits nothing", got)
	}
}

// ---------------------------------------------------------------------------
// AC2 — the TTL sweeper's store half
// ---------------------------------------------------------------------------

// ExpiredHolds finds due holds and nothing else. The expiry is written from the DATABASE clock,
// so a hold created with a negative TTL is already past due the moment it commits — which is how
// this test avoids sleeping for fifteen minutes.
func TestExpiredHoldsFindsOnlyDueHolds_Issue5_AC2(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	ctx := context.Background()

	dueStart, dueEnd := window(24)
	due, err := store.Insert(ctx, claim(t), reservation(t, newID(t), dueStart, dueEnd), -time.Minute)
	if err != nil {
		t.Fatalf("creating an already-expired hold: %v", err)
	}

	liveStart, liveEnd := window(48)
	live, err := store.Insert(ctx, claim(t), reservation(t, newID(t), liveStart, liveEnd), holdTTL)
	if err != nil {
		t.Fatalf("creating a live hold: %v", err)
	}

	confirmedStart, confirmedEnd := window(72)
	confirmed, err := store.Insert(ctx, claim(t), reservation(t, newID(t), confirmedStart, confirmedEnd), -time.Minute)
	if err != nil {
		t.Fatalf("creating a hold to confirm: %v", err)
	}
	if _, _, err := store.Apply(ctx, confirmed.ID, func(r domain.Reservation) (domain.Reservation, []domain.Emission, error) {
		return r.Confirm(time.Now().UTC())
	}); err != nil {
		t.Fatalf("confirming: %v", err)
	}

	ids, err := store.ExpiredHolds(ctx, 100)
	if err != nil {
		t.Fatalf("ExpiredHolds: %v", err)
	}

	found := map[string]bool{}
	for _, id := range ids {
		found[id] = true
	}
	if !found[due.ID] {
		t.Errorf("the expired hold %s was not returned", due.ID)
	}
	if found[live.ID] {
		t.Errorf("the live hold %s was returned; its TTL has not run out", live.ID)
	}
	if found[confirmed.ID] {
		t.Errorf("the confirmed reservation %s was returned; it was paid for and has no expiry", confirmed.ID)
	}
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

func TestBusyReportsLiveWindowsAndOmitsReleasedOnes(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	ctx := context.Background()
	resourceID := newID(t)

	heldStart, heldEnd := window(24)
	if _, err := store.Insert(ctx, claim(t), reservation(t, resourceID, heldStart, heldEnd), holdTTL); err != nil {
		t.Fatalf("held reserve: %v", err)
	}

	releasedStart, releasedEnd := window(26)
	released, err := store.Insert(ctx, claim(t), reservation(t, resourceID, releasedStart, releasedEnd), holdTTL)
	if err != nil {
		t.Fatalf("reserve to release: %v", err)
	}
	if _, _, err := store.Apply(ctx, released.ID, func(r domain.Reservation) (domain.Reservation, []domain.Emission, error) {
		return r.Release(domain.ReasonHoldExpired, time.Now().UTC())
	}); err != nil {
		t.Fatalf("releasing: %v", err)
	}

	// Another resource's window inside the same range must not leak into the answer.
	if _, err := store.Insert(ctx, claim(t), reservation(t, newID(t), heldStart, heldEnd), holdTTL); err != nil {
		t.Fatalf("other resource: %v", err)
	}

	windows, err := store.Busy(ctx, resourceID, heldStart.Add(-time.Hour), releasedEnd.Add(time.Hour))
	if err != nil {
		t.Fatalf("Busy: %v", err)
	}

	if len(windows) != 1 {
		t.Fatalf("Busy returned %d windows, want 1: %+v", len(windows), windows)
	}
	if !windows[0].StartsAt.Equal(heldStart) || !windows[0].EndsAt.Equal(heldEnd) {
		t.Errorf("window = [%s, %s), want [%s, %s)",
			windows[0].StartsAt, windows[0].EndsAt, heldStart, heldEnd)
	}
}

func TestBusyReturnsAnEmptySliceRatherThanNil(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	start, end := window(24)

	// Non-nil matters: the OpenAPI schema makes `busy` a required array, and a nil slice
	// marshals to `null`, which fails validation.
	windows, err := store.Busy(context.Background(), newID(t), start, end)
	if err != nil {
		t.Fatalf("Busy: %v", err)
	}
	if windows == nil {
		t.Error("Busy returned nil; an empty result must marshal as [] not null")
	}
	if len(windows) != 0 {
		t.Errorf("Busy returned %d windows for an unused resource, want 0", len(windows))
	}
}

func TestByIDReturnsErrNotFoundForAnUnknownReservation(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)

	_, err := store.ByID(context.Background(), newID(t))

	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

// The empty-range hole ADR-0011 closes. The domain rejects this first, so reaching the schema
// requires going around it — which is exactly what a future caller or migration might do.
func TestTheSchemaRejectsAnEmptyWindow(t *testing.T) {
	t.Parallel()

	_, pool := newStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(24 * time.Hour)

	_, err := pool.Exec(ctx, `
		INSERT INTO reservation (id, resource_id, booking_id, during, status, expires_at)
		VALUES ($1, $2, $3, tstzrange($4, $4, '[)'), 'held', now() + interval '15 minutes')`,
		newID(t), newID(t), newID(t), at)

	if err == nil {
		t.Fatal("the schema accepted an empty window; it would overlap nothing and slip past the exclusion constraint")
	}
}

// outboxRow is the subset of an outbox row these tests assert on.
type outboxRow struct {
	event       string
	aggregateID string
	payload     []byte
	publishedAt *time.Time
}

func outboxRows(t *testing.T, pool *pgxpool.Pool) []outboxRow {
	t.Helper()

	rows, err := pool.Query(context.Background(),
		`SELECT event, aggregate_id, payload, published_at FROM outbox ORDER BY occurred_at, id`)
	if err != nil {
		t.Fatalf("reading outbox: %v", err)
	}
	defer rows.Close()

	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.event, &r.aggregateID, &r.payload, &r.publishedAt); err != nil {
			t.Fatalf("scanning outbox row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading outbox rows: %v", err)
	}
	return out
}
