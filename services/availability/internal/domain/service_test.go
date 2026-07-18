package domain

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeStore records what the service asked of it and replays canned answers. It is a fake, not a
// mock of the invariant: it cannot and does not pretend to enforce no-double-booking. That is a
// Postgres exclusion constraint and is proved in the integration suite against a real database
// (testing-standards: "ADR-0003's invariant is integration-only").
type fakeStore struct {
	inserted    []Reservation
	insertClaim Claim
	insertTTL   time.Duration
	insertErr   error
	insertBack  Reservation

	byID    map[string]Reservation
	byIDErr error

	applied    []string
	applyErr   map[string]error
	applyCalls int

	busy    []Window
	busyErr error
	busyArg struct {
		resourceID string
		from, to   time.Time
	}

	expired    []string
	expiredErr error

	// dbNow stands in for the database clock. Distinct from the injected Clock on purpose: the
	// sweeper must use THIS one, and a test that made them equal could not tell the difference.
	dbNow    time.Time
	dbNowErr error
	nowCalls int
}

func newFakeStore() *fakeStore {
	return &fakeStore{byID: map[string]Reservation{}, applyErr: map[string]error{}}
}

func (f *fakeStore) Insert(_ context.Context, claim Claim, r Reservation, ttl time.Duration) (Reservation, error) {
	f.insertClaim = claim
	f.insertTTL = ttl
	if f.insertErr != nil {
		return Reservation{}, f.insertErr
	}
	f.inserted = append(f.inserted, r)

	stored := r
	if f.insertBack.ID != "" {
		stored = f.insertBack
	} else {
		// Stand in for the database's RETURNING: expiry and stamps only exist once the row does.
		exp := createdAt.Add(ttl)
		stored.ExpiresAt = &exp
		stored.CreatedAt = createdAt
		stored.UpdatedAt = createdAt
	}
	f.byID[stored.ID] = stored
	return stored, nil
}

func (f *fakeStore) ByID(_ context.Context, id string) (Reservation, error) {
	if f.byIDErr != nil {
		return Reservation{}, f.byIDErr
	}
	r, ok := f.byID[id]
	if !ok {
		return Reservation{}, ErrNotFound
	}
	return r, nil
}

func (f *fakeStore) Apply(_ context.Context, id string, fn Transition) (Reservation, bool, error) {
	f.applyCalls++
	f.applied = append(f.applied, id)
	if err, ok := f.applyErr[id]; ok {
		return Reservation{}, false, err
	}
	current, ok := f.byID[id]
	if !ok {
		return Reservation{}, false, ErrNotFound
	}
	next, emissions, err := fn(current)
	if err != nil {
		return Reservation{}, false, err
	}
	f.byID[id] = next
	return next, len(emissions) > 0, nil
}

func (f *fakeStore) Busy(_ context.Context, resourceID string, from, to time.Time) ([]Window, error) {
	f.busyArg.resourceID, f.busyArg.from, f.busyArg.to = resourceID, from, to
	return f.busy, f.busyErr
}

func (f *fakeStore) Now(context.Context) (time.Time, error) {
	f.nowCalls++
	if f.dbNowErr != nil {
		return time.Time{}, f.dbNowErr
	}
	return f.dbNow, nil
}

func (f *fakeStore) ExpiredHolds(_ context.Context, _ int) ([]string, error) {
	return f.expired, f.expiredErr
}

// fixedClock and countingIDs keep the service deterministic. testing-standards: never call
// time.Now() or mint a UUID inside code under test.
func fixedClock(at time.Time) Clock { return func() time.Time { return at } }

func sequentialIDs(ids ...string) IDGen {
	i := 0
	return func() (string, error) {
		if i >= len(ids) {
			return "", errors.New("fake ran out of ids")
		}
		id := ids[i]
		i++
		return id, nil
	}
}

const holdTTL = 15 * time.Minute

func TestReserveMintsAnIDAndHandsTheStoreTheHoldTTL(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	svc := NewService(store, fixedClock(createdAt), sequentialIDs(resvID), holdTTL)

	got, err := svc.Reserve(context.Background(), ReserveCommand{
		ResourceID:  resID,
		BookingID:   bookID,
		StartsAt:    windowStart,
		EndsAt:      windowEnd,
		Idempotency: Claim{Key: "key-abcdefgh", Fingerprint: "fp"},
	})
	if err != nil {
		t.Fatalf("Reserve returned %v, want nil", err)
	}

	if len(store.inserted) != 1 {
		t.Fatalf("store saw %d insert(s), want exactly 1", len(store.inserted))
	}
	if store.inserted[0].ID != resvID {
		t.Errorf("inserted id = %q, want the minted %q", store.inserted[0].ID, resvID)
	}
	if store.inserted[0].Status != StatusHeld {
		t.Errorf("inserted status = %q, want %q", store.inserted[0].Status, StatusHeld)
	}
	if store.insertTTL != holdTTL {
		t.Errorf("hold ttl = %s, want %s", store.insertTTL, holdTTL)
	}
	if store.insertClaim.Key != "key-abcdefgh" || store.insertClaim.Fingerprint != "fp" {
		t.Errorf("idempotency claim = %+v, want the one from the command", store.insertClaim)
	}
	if got.ID != resvID {
		t.Errorf("returned id = %q, want %q", got.ID, resvID)
	}
	if got.ExpiresAt == nil {
		t.Error("returned expires_at is nil; the persisted row's expiry must be carried back")
	}
}

// AC6: an invalid window is rejected by the domain, so the store is never asked. Reaching the
// database to be told `ends_at <= starts_at` costs a round trip and returns a CHECK violation
// that has to be reverse-engineered back into a 422.
func TestReserveRejectsAnInvalidWindowWithoutTouchingTheStore_Issue5_AC6(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	svc := NewService(store, fixedClock(createdAt), sequentialIDs(resvID), holdTTL)

	_, err := svc.Reserve(context.Background(), ReserveCommand{
		ResourceID:  resID,
		BookingID:   bookID,
		StartsAt:    windowEnd,
		EndsAt:      windowStart,
		Idempotency: Claim{Key: "key-abcdefgh", Fingerprint: "fp"},
	})

	if !errors.Is(err, ErrInvalidTimeWindow) {
		t.Errorf("error = %v, want ErrInvalidTimeWindow", err)
	}
	if len(store.inserted) != 0 {
		t.Errorf("store saw %d insert(s) for an invalid window, want 0", len(store.inserted))
	}
}

// The lost race is surfaced verbatim. Swallowing it or retrying would hand the caller a slot
// somebody else holds.
func TestReservePropagatesTheLostRace(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.insertErr = ErrSlotUnavailable
	svc := NewService(store, fixedClock(createdAt), sequentialIDs(resvID), holdTTL)

	_, err := svc.Reserve(context.Background(), ReserveCommand{
		ResourceID:  resID,
		BookingID:   bookID,
		StartsAt:    windowStart,
		EndsAt:      windowEnd,
		Idempotency: Claim{Key: "key-abcdefgh", Fingerprint: "fp"},
	})

	if !errors.Is(err, ErrSlotUnavailable) {
		t.Errorf("error = %v, want ErrSlotUnavailable", err)
	}
}

func TestConfirmAppliesTheTransitionToTheStoredReservation(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.byID[resvID] = heldReservation(t, createdAt.Add(holdTTL))
	now := createdAt.Add(time.Minute)
	svc := NewService(store, fixedClock(now), sequentialIDs(), holdTTL)

	got, err := svc.Confirm(context.Background(), resvID)
	if err != nil {
		t.Fatalf("Confirm returned %v, want nil", err)
	}

	if got.Status != StatusConfirmed {
		t.Errorf("status = %q, want %q", got.Status, StatusConfirmed)
	}
	if !got.UpdatedAt.Equal(now) {
		t.Errorf("updated_at = %s, want the injected clock's %s", got.UpdatedAt, now)
	}
	if len(store.applied) != 1 || store.applied[0] != resvID {
		t.Errorf("store.Apply called with %v, want exactly [%s]", store.applied, resvID)
	}
}

func TestConfirmSurfacesNotFound(t *testing.T) {
	t.Parallel()

	svc := NewService(newFakeStore(), fixedClock(createdAt), sequentialIDs(), holdTTL)

	_, err := svc.Confirm(context.Background(), "01912d5a-7f3e-7c1a-9b2e-00000000dead")

	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestReleaseRejectsAReasonOutsideTheContractEnumBeforeWriting(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.byID[resvID] = heldReservation(t, createdAt.Add(holdTTL))
	svc := NewService(store, fixedClock(createdAt), sequentialIDs(), holdTTL)

	_, err := svc.Release(context.Background(), resvID, ReleaseReason("nope"))

	if !errors.Is(err, ErrInvalidReleaseReason) {
		t.Errorf("error = %v, want ErrInvalidReleaseReason", err)
	}
	if store.applyCalls != 0 {
		t.Errorf("store.Apply called %d time(s) for an invalid reason, want 0", store.applyCalls)
	}
}

func TestBusyRejectsAQueryWindowThatDoesNotMoveForward(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	svc := NewService(store, fixedClock(createdAt), sequentialIDs(), holdTTL)

	cases := []struct {
		name     string
		from, to time.Time
	}{
		{"to equals from", windowStart, windowStart},
		{"to before from", windowEnd, windowStart},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Busy(context.Background(), resID, tc.from, tc.to)
			if !errors.Is(err, ErrInvalidQueryWindow) {
				t.Errorf("error = %v, want ErrInvalidQueryWindow", err)
			}
		})
	}
}

func TestBusyPassesTheQueryWindowThrough(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.busy = []Window{{StartsAt: windowStart, EndsAt: windowEnd}}
	svc := NewService(store, fixedClock(createdAt), sequentialIDs(), holdTTL)

	got, err := svc.Busy(context.Background(), resID, windowStart, windowEnd)
	if err != nil {
		t.Fatalf("Busy returned %v, want nil", err)
	}

	if len(got) != 1 || !got[0].StartsAt.Equal(windowStart) || !got[0].EndsAt.Equal(windowEnd) {
		t.Errorf("windows = %+v, want the store's single window", got)
	}
	if store.busyArg.resourceID != resID ||
		!store.busyArg.from.Equal(windowStart) || !store.busyArg.to.Equal(windowEnd) {
		t.Errorf("store queried with %+v, want (%s, %s, %s)",
			store.busyArg, resID, windowStart, windowEnd)
	}
}

// AC2: the sweeper releases every due hold and counts only the ones it actually changed.
func TestSweepReleasesEveryDueHoldAndCountsOnlyRealChanges_Issue5_AC2(t *testing.T) {
	t.Parallel()

	expiry := createdAt.Add(holdTTL)
	now := expiry.Add(time.Second)

	due := heldReservation(t, expiry)
	due.ID = "01912d5a-7f3e-7c1a-9b2e-0000000000a1"

	alsoDue := heldReservation(t, expiry)
	alsoDue.ID = "01912d5a-7f3e-7c1a-9b2e-0000000000a2"

	// Confirmed between the scan and the sweep — it must survive.
	rescued := heldReservation(t, expiry)
	rescued.ID = "01912d5a-7f3e-7c1a-9b2e-0000000000a3"
	rescued.Status = StatusConfirmed
	rescued.ExpiresAt = nil

	store := newFakeStore()
	store.dbNow = now
	for _, r := range []Reservation{due, alsoDue, rescued} {
		store.byID[r.ID] = r
	}
	store.expired = []string{due.ID, alsoDue.ID, rescued.ID}

	svc := NewService(store, fixedClock(now), sequentialIDs(), holdTTL)

	released, err := svc.Sweep(context.Background(), 100)
	if err != nil {
		t.Fatalf("Sweep returned %v, want nil", err)
	}

	if released != 2 {
		t.Errorf("released = %d, want 2 — the confirmed reservation must not be swept", released)
	}
	for _, id := range []string{due.ID, alsoDue.ID} {
		got := store.byID[id]
		if got.Status != StatusReleased {
			t.Errorf("%s status = %q, want %q", id, got.Status, StatusReleased)
		}
		if got.ReleasedReason == nil || *got.ReleasedReason != ReasonHoldExpired {
			t.Errorf("%s released_reason = %v, want %q", id, got.ReleasedReason, ReasonHoldExpired)
		}
	}
	if store.byID[rescued.ID].Status != StatusConfirmed {
		t.Errorf("the confirmed reservation was swept: status = %q", store.byID[rescued.ID].Status)
	}
}

// One bad row must not stop the sweep. A sweeper that aborts on the first failure leaves every
// hold behind it locked until a human notices — the slow inventory leak ADR-0011 is about,
// arriving by a different route.
func TestSweepKeepsGoingAfterOneReservationFails(t *testing.T) {
	t.Parallel()

	expiry := createdAt.Add(holdTTL)
	now := expiry.Add(time.Second)

	broken := heldReservation(t, expiry)
	broken.ID = "01912d5a-7f3e-7c1a-9b2e-0000000000b1"
	fine := heldReservation(t, expiry)
	fine.ID = "01912d5a-7f3e-7c1a-9b2e-0000000000b2"

	store := newFakeStore()
	store.dbNow = now
	store.byID[broken.ID] = broken
	store.byID[fine.ID] = fine
	store.expired = []string{broken.ID, fine.ID}
	store.applyErr[broken.ID] = errors.New("connection reset")

	svc := NewService(store, fixedClock(now), sequentialIDs(), holdTTL)

	released, err := svc.Sweep(context.Background(), 100)

	if err == nil {
		t.Error("Sweep returned nil error; the failure must be reported, not swallowed")
	}
	if released != 1 {
		t.Errorf("released = %d, want 1 — the healthy hold must still be swept", released)
	}
	if store.byID[fine.ID].Status != StatusReleased {
		t.Errorf("the healthy hold was skipped: status = %q", store.byID[fine.ID].Status)
	}
}

func TestReleaseAppliesTheTransitionAndReportsTheReleasedReservation(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.byID[resvID] = heldReservation(t, createdAt.Add(holdTTL))
	now := createdAt.Add(time.Minute)
	svc := NewService(store, fixedClock(now), sequentialIDs(), holdTTL)

	got, err := svc.Release(context.Background(), resvID, ReasonBookingCancelled)
	if err != nil {
		t.Fatalf("Release returned %v, want nil", err)
	}

	if got.Status != StatusReleased {
		t.Errorf("status = %q, want %q", got.Status, StatusReleased)
	}
	if got.ReleasedReason == nil || *got.ReleasedReason != ReasonBookingCancelled {
		t.Errorf("released_reason = %v, want %q", got.ReleasedReason, ReasonBookingCancelled)
	}
	if !got.UpdatedAt.Equal(now) {
		t.Errorf("updated_at = %s, want the injected clock's %s", got.UpdatedAt, now)
	}
}

// A reservation cannot be inserted without an id, so a generator failure must abort before the
// store is touched rather than inserting under a zero id.
func TestReserveAbortsWhenTheIDGeneratorFails(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	svc := NewService(store, fixedClock(createdAt), sequentialIDs(), holdTTL) // no ids available

	_, err := svc.Reserve(context.Background(), ReserveCommand{
		ResourceID:  resID,
		BookingID:   bookID,
		StartsAt:    windowStart,
		EndsAt:      windowEnd,
		Idempotency: Claim{Key: "key-abcdefgh", Fingerprint: "fp"},
	})

	if err == nil {
		t.Fatal("Reserve returned nil error when no id could be minted")
	}
	if len(store.inserted) != 0 {
		t.Errorf("store saw %d insert(s), want 0", len(store.inserted))
	}
}

func TestSweepReportsAFailedScan(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.expiredErr = errors.New("database unreachable")
	svc := NewService(store, fixedClock(createdAt), sequentialIDs(), holdTTL)

	released, err := svc.Sweep(context.Background(), 100)

	if err == nil {
		t.Error("Sweep returned nil error for a failed scan")
	}
	if released != 0 {
		t.Errorf("released = %d, want 0", released)
	}
}

// A hold that vanished between the scan and the update is not a failure: a second sweeper
// replica taking the same candidate is normal, and treating it as an error would make every
// multi-replica sweep log errors it cannot act on.
func TestSweepTreatsAVanishedHoldAsNormal(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.dbNow = createdAt
	store.expired = []string{"01912d5a-7f3e-7c1a-9b2e-0000000000c1"}
	store.applyErr["01912d5a-7f3e-7c1a-9b2e-0000000000c1"] = ErrNotFound
	svc := NewService(store, fixedClock(createdAt), sequentialIDs(), holdTTL)

	released, err := svc.Sweep(context.Background(), 100)

	if err != nil {
		t.Errorf("Sweep returned %v, want nil for a vanished hold", err)
	}
	if released != 0 {
		t.Errorf("released = %d, want 0", released)
	}
}

// Shutdown must stop the sweep promptly rather than working through the whole batch. The
// remaining holds are still due on the next pass.
func TestSweepStopsWhenTheContextIsCancelled(t *testing.T) {
	t.Parallel()

	expiry := createdAt.Add(holdTTL)
	store := newFakeStore()
	store.dbNow = expiry.Add(time.Second)
	for _, id := range []string{"01912d5a-7f3e-7c1a-9b2e-0000000000d1", "01912d5a-7f3e-7c1a-9b2e-0000000000d2"} {
		r := heldReservation(t, expiry)
		r.ID = id
		store.byID[id] = r
		store.expired = append(store.expired, id)
	}
	svc := NewService(store, fixedClock(expiry.Add(time.Second)), sequentialIDs(), holdTTL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	released, err := svc.Sweep(ctx, 100)

	if released != 0 {
		t.Errorf("released = %d, want 0 — a cancelled sweep must not keep writing", released)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if store.applyCalls != 0 {
		t.Errorf("store.Apply called %d time(s) after cancellation, want 0", store.applyCalls)
	}
}

func TestGetReturnsTheStoredReservation(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	want := heldReservation(t, createdAt.Add(holdTTL))
	store.byID[resvID] = want
	svc := NewService(store, fixedClock(createdAt), sequentialIDs(), holdTTL)

	got, err := svc.Get(context.Background(), resvID)
	if err != nil {
		t.Fatalf("Get returned %v, want nil", err)
	}
	if got.ID != want.ID || got.Status != want.Status {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// The BookingCancelled consumer runs this transition inside the transaction inbox.Process
// opened. Which reason a cancelled booking implies is a domain decision, so it is pinned here
// rather than in the consumer.
func TestCompensateCancelledBookingReleasesWithBookingCancelled(t *testing.T) {
	t.Parallel()

	now := createdAt.Add(time.Minute)
	svc := NewService(newFakeStore(), fixedClock(now), sequentialIDs(), holdTTL)

	next, emissions, err := svc.CompensateCancelledBooking()(heldReservation(t, createdAt.Add(holdTTL)))
	if err != nil {
		t.Fatalf("transition returned %v, want nil", err)
	}

	if next.Status != StatusReleased {
		t.Errorf("status = %q, want %q", next.Status, StatusReleased)
	}
	if next.ReleasedReason == nil || *next.ReleasedReason != ReasonBookingCancelled {
		t.Errorf("released_reason = %v, want %q", next.ReleasedReason, ReasonBookingCancelled)
	}
	if !next.UpdatedAt.Equal(now) {
		t.Errorf("updated_at = %s, want the injected clock's %s", next.UpdatedAt, now)
	}
	if len(emissions) != 1 {
		t.Fatalf("emitted %d event(s), want 1", len(emissions))
	}
}

// A booking cancelled after its reservation was already released must not produce a second
// ReservationReleased — Booking reacts to that event, and a duplicate is a second cancellation.
func TestCompensateCancelledBookingIsANoOpOnAnAlreadyReleasedReservation(t *testing.T) {
	t.Parallel()

	svc := NewService(newFakeStore(), fixedClock(createdAt), sequentialIDs(), holdTTL)

	reason := ReasonHoldExpired
	released := heldReservation(t, createdAt.Add(holdTTL))
	released.Status = StatusReleased
	released.ExpiresAt = nil
	released.ReleasedReason = &reason

	_, emissions, err := svc.CompensateCancelledBooking()(released)
	if err != nil {
		t.Fatalf("transition returned %v, want nil", err)
	}
	if len(emissions) != 0 {
		t.Errorf("emitted %d event(s), want 0", len(emissions))
	}
}

// The sweeper must judge due-ness by the DATABASE clock, not this process's.
//
// expires_at is written by the database and ExpiredHolds filters on it in SQL, so a lagging
// application clock would have the scan keep proposing rows that the re-check keeps declining:
// the hold expires late while the sweeper spins on it doing nothing. The two clocks are set
// deliberately apart here, straddling the expiry, so a regression to time.Now() fails.
func TestSweepJudgesExpiryByTheDatabaseClockNotTheAppClock_Issue5_AC2(t *testing.T) {
	t.Parallel()

	expiry := createdAt.Add(holdTTL)

	due := heldReservation(t, expiry)
	due.ID = "01912d5a-7f3e-7c1a-9b2e-0000000000e1"

	store := newFakeStore()
	store.byID[due.ID] = due
	store.expired = []string{due.ID}
	// The database says the hold is due...
	store.dbNow = expiry.Add(time.Second)

	// ...while this process's clock still thinks there is an hour left.
	svc := NewService(store, fixedClock(expiry.Add(-time.Hour)), sequentialIDs(), holdTTL)

	released, err := svc.Sweep(context.Background(), 100)
	if err != nil {
		t.Fatalf("Sweep returned %v, want nil", err)
	}

	if released != 1 {
		t.Errorf("released = %d, want 1 — the sweeper consulted the application clock", released)
	}
	if store.nowCalls != 1 {
		t.Errorf("the database clock was read %d time(s), want exactly 1 per pass", store.nowCalls)
	}
	if got := store.byID[due.ID]; got.Status != StatusReleased {
		t.Errorf("status = %q, want %q", got.Status, StatusReleased)
	}
}

// A sweep that cannot read the database clock must abort rather than fall back to a local one:
// falling back is precisely the two-clock comparison this design removed.
func TestSweepAbortsWhenTheDatabaseClockIsUnreadable(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.dbNowErr = errors.New("connection reset")
	store.expired = []string{"01912d5a-7f3e-7c1a-9b2e-0000000000f1"}

	svc := NewService(store, fixedClock(createdAt), sequentialIDs(), holdTTL)

	released, err := svc.Sweep(context.Background(), 100)

	if err == nil {
		t.Error("Sweep returned nil error when the database clock was unreadable")
	}
	if released != 0 {
		t.Errorf("released = %d, want 0", released)
	}
	if store.applyCalls != 0 {
		t.Errorf("store.Apply was called %d time(s) without a trustworthy clock, want 0", store.applyCalls)
	}
}
