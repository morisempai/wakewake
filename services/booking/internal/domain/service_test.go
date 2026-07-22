package domain

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// fakes — the ports, backed by plain structs. No database, no HTTP.
// ---------------------------------------------------------------------------

type fakeStore struct {
	byID       map[string]Booking
	idem       map[string]idemRow // key -> {bookingID, fingerprint}
	inserted   *Booking
	insertErr  error
	applyErr   error
	lastFn     Transition
	applyCount int
}

type idemRow struct {
	bookingID   string
	fingerprint string
}

func newFakeStore() *fakeStore {
	return &fakeStore{byID: map[string]Booking{}, idem: map[string]idemRow{}}
}

func (f *fakeStore) Insert(_ context.Context, claim Claim, b Booking) (Booking, error) {
	if f.insertErr != nil {
		return Booking{}, f.insertErr
	}
	if row, ok := f.idem[claim.Key]; ok {
		if row.fingerprint != claim.Fingerprint {
			return Booking{}, ErrIdempotencyKeyReuse
		}
		return f.byID[row.bookingID], nil
	}
	f.idem[claim.Key] = idemRow{bookingID: b.ID, fingerprint: claim.Fingerprint}
	f.byID[b.ID] = b
	cp := b
	f.inserted = &cp
	return b, nil
}

func (f *fakeStore) ByID(_ context.Context, id string) (Booking, error) {
	b, ok := f.byID[id]
	if !ok {
		return Booking{}, ErrNotFound
	}
	return b, nil
}

func (f *fakeStore) List(_ context.Context, _ ListQuery) (ListPage, error) { return ListPage{}, nil }

func (f *fakeStore) Apply(_ context.Context, id string, fn Transition) (Booking, bool, error) {
	f.applyCount++
	f.lastFn = fn
	if f.applyErr != nil {
		return Booking{}, false, f.applyErr
	}
	current, ok := f.byID[id]
	if !ok {
		return Booking{}, false, ErrNotFound
	}
	next, emissions, err := fn(current)
	if err != nil {
		return Booking{}, false, err
	}
	f.byID[id] = next
	return next, len(emissions) > 0, nil
}

func (f *fakeStore) FindIdempotent(_ context.Context, key string) (string, string, bool, error) {
	row, ok := f.idem[key]
	if !ok {
		return "", "", false, nil
	}
	return row.bookingID, row.fingerprint, true, nil
}

type fakeReservations struct {
	hold       Reservation
	holdErr    error
	holdCalls  int
	confirmErr error
	confirmN   int
}

func (f *fakeReservations) Hold(_ context.Context, req HoldRequest) (Reservation, error) {
	f.holdCalls++
	if f.holdErr != nil {
		return Reservation{}, f.holdErr
	}
	r := f.hold
	if r.BookingID == "" {
		r.BookingID = req.BookingID // default: adopt the id we were given
	}
	return r, nil
}

func (f *fakeReservations) Confirm(_ context.Context, _ string) error {
	f.confirmN++
	return f.confirmErr
}

type fakeCatalog struct {
	product Product
	err     error
}

func (f *fakeCatalog) Product(_ context.Context, _ string) (Product, error) {
	if f.err != nil {
		return Product{}, f.err
	}
	return f.product, nil
}

func newService(store Store, res Reservations, cat Catalog) *Service {
	return NewService(store, res, cat, func() time.Time { return tNow },
		func() (string, error) { return tBookingID, nil }, 15*time.Minute)
}

func createCmd(party int, fingerprint string) CreateCommand {
	return CreateCommand{
		CustomerID:  tCustomerID,
		ProductID:   tProductID,
		StartsAt:    tStart,
		EndsAt:      tEnd,
		PartySize:   party,
		Idempotency: Claim{Key: "idem-key-123456", Fingerprint: fingerprint},
	}
}

// ---------------------------------------------------------------------------
// CreateBooking — the saga's first step
// ---------------------------------------------------------------------------

func TestCreateBookingHoldsThroughAvailabilityAndPersists(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	res := &fakeReservations{hold: Reservation{ID: tResID, BookingID: tBookingID, ExpiresAt: tExpiry}}
	cat := &fakeCatalog{product: testProduct()}
	svc := newService(store, res, cat)

	b, err := svc.CreateBooking(context.Background(), createCmd(2, "fp-a"))
	if err != nil {
		t.Fatalf("CreateBooking: %v", err)
	}
	if b.Status != StatusHeld || b.ResourceID != tResourceID {
		t.Errorf("booking = %+v, want held on the product's resource", b)
	}
	if res.holdCalls != 1 {
		t.Errorf("Availability.Hold called %d times, want 1", res.holdCalls)
	}
	if store.inserted == nil {
		t.Error("nothing was inserted")
	}
}

func TestCreateBookingRejectsAnUnknownProductBeforeReserving(t *testing.T) {
	t.Parallel()

	res := &fakeReservations{}
	svc := newService(newFakeStore(), res, &fakeCatalog{err: ErrProductNotFound})

	_, err := svc.CreateBooking(context.Background(), createCmd(2, "fp-a"))
	if !errors.Is(err, ErrProductNotFound) {
		t.Fatalf("err = %v, want ErrProductNotFound", err)
	}
	if res.holdCalls != 0 {
		t.Error("Availability was called for an unknown product; it must be rejected first")
	}
}

func TestCreateBookingRejectsAnOversizedPartyBeforeReserving(t *testing.T) {
	t.Parallel()

	res := &fakeReservations{}
	svc := newService(newFakeStore(), res, &fakeCatalog{product: testProduct()})

	_, err := svc.CreateBooking(context.Background(), createCmd(9, "fp-a"))
	if !errors.Is(err, ErrPartyExceedsCapacity) {
		t.Fatalf("err = %v, want ErrPartyExceedsCapacity", err)
	}
	if res.holdCalls != 0 {
		t.Error("Availability was called despite an oversized party; the domain must reject first")
	}
}

func TestCreateBookingMapsAnOverlapToSlotUnavailable(t *testing.T) {
	t.Parallel()

	res := &fakeReservations{holdErr: ErrSlotUnavailable}
	svc := newService(newFakeStore(), res, &fakeCatalog{product: testProduct()})

	_, err := svc.CreateBooking(context.Background(), createCmd(2, "fp-a"))
	if !errors.Is(err, ErrSlotUnavailable) {
		t.Fatalf("err = %v, want ErrSlotUnavailable", err)
	}
}

func TestCreateBookingReplayReturnsTheOriginalWithoutReserving(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	res := &fakeReservations{hold: Reservation{ID: tResID, BookingID: tBookingID, ExpiresAt: tExpiry}}
	svc := newService(store, res, &fakeCatalog{product: testProduct()})

	first, err := svc.CreateBooking(context.Background(), createCmd(2, "fp-a"))
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	second, err := svc.CreateBooking(context.Background(), createCmd(2, "fp-a"))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("replay id = %s, want the original %s", second.ID, first.ID)
	}
	if res.holdCalls != 1 {
		t.Errorf("Availability.Hold called %d times across a replay, want 1", res.holdCalls)
	}
}

func TestCreateBookingSameKeyDifferentBodyIsReuse(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	res := &fakeReservations{hold: Reservation{ID: tResID, BookingID: tBookingID, ExpiresAt: tExpiry}}
	svc := newService(store, res, &fakeCatalog{product: testProduct()})

	if _, err := svc.CreateBooking(context.Background(), createCmd(2, "fp-a")); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := svc.CreateBooking(context.Background(), createCmd(2, "fp-DIFFERENT"))
	if !errors.Is(err, ErrIdempotencyKeyReuse) {
		t.Fatalf("err = %v, want ErrIdempotencyKeyReuse", err)
	}
}

// ---------------------------------------------------------------------------
// the consumer transitions
// ---------------------------------------------------------------------------

func TestConfirmOnPaymentConfirmsWhenAvailabilityConfirms(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.byID[tBookingID] = heldBooking(t)
	res := &fakeReservations{}
	svc := newService(store, res, &fakeCatalog{})

	b, changed, err := store.Apply(context.Background(), tBookingID, svc.ConfirmOnPayment(context.Background(), tPaymentID))
	if err != nil || !changed {
		t.Fatalf("apply: err=%v changed=%v", err, changed)
	}
	if b.Status != StatusConfirmed {
		t.Errorf("status = %q, want confirmed", b.Status)
	}
	if res.confirmN != 1 {
		t.Errorf("Availability.Confirm called %d times, want 1", res.confirmN)
	}
}

func TestConfirmOnPaymentCancelsWhenTheReservationWasAlreadyReleased(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.byID[tBookingID] = heldBooking(t)
	res := &fakeReservations{confirmErr: ErrReservationReleased}
	svc := newService(store, res, &fakeCatalog{})

	b, _, err := store.Apply(context.Background(), tBookingID, svc.ConfirmOnPayment(context.Background(), tPaymentID))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if b.Status != StatusCancelled {
		t.Errorf("status = %q, want cancelled — the hold was swept before payment", b.Status)
	}
	if b.CancelReason == nil || *b.CancelReason != ReasonHoldExpired {
		t.Errorf("cancel_reason = %v, want hold_expired", b.CancelReason)
	}
}

func TestConfirmOnPaymentPropagatesATransientAvailabilityError(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.byID[tBookingID] = heldBooking(t)
	res := &fakeReservations{confirmErr: ErrAvailabilityUnavailable}
	svc := newService(store, res, &fakeCatalog{})

	_, _, err := store.Apply(context.Background(), tBookingID, svc.ConfirmOnPayment(context.Background(), tPaymentID))
	if !errors.Is(err, ErrAvailabilityUnavailable) {
		t.Fatalf("err = %v, want ErrAvailabilityUnavailable so the consumer retries", err)
	}
	if store.byID[tBookingID].Status != StatusHeld {
		t.Error("the booking changed despite a failed confirm; the transition must not have committed")
	}
}

func TestCompensateOnPaymentFailureCancelsTheHeldBooking(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	store.byID[tBookingID] = heldBooking(t)
	res := &fakeReservations{}
	svc := newService(store, res, &fakeCatalog{})

	b, _, err := store.Apply(context.Background(), tBookingID, svc.CompensateOnPaymentFailure())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if b.Status != StatusCancelled || b.CancelReason == nil || *b.CancelReason != ReasonPaymentFailed {
		t.Errorf("booking = %+v, want cancelled/payment_failed", b)
	}
	// Compensation is event-driven; it must NOT call Availability's HTTP API.
	if res.confirmN != 0 {
		t.Error("payment-failure compensation called Availability over HTTP; it must be event-driven")
	}
}

func TestExpireHoldCancelsOnlyAHeldBooking(t *testing.T) {
	t.Parallel()

	store := newFakeStore()
	svc := newService(store, &fakeReservations{}, &fakeCatalog{})

	store.byID[tBookingID] = heldBooking(t)
	b, changed, err := store.Apply(context.Background(), tBookingID, svc.ExpireHold())
	if err != nil || !changed {
		t.Fatalf("apply held: err=%v changed=%v", err, changed)
	}
	if b.Status != StatusCancelled || *b.CancelReason != ReasonHoldExpired {
		t.Errorf("booking = %+v, want cancelled/hold_expired", b)
	}

	// A confirmed booking must not be cancelled by a stray TTL release.
	confirmed, _, _ := heldBooking(t).Confirm(tPaymentID, tNow)
	store.byID[tBookingID] = confirmed
	_, changed, err = store.Apply(context.Background(), tBookingID, svc.ExpireHold())
	if err != nil {
		t.Fatalf("apply confirmed: %v", err)
	}
	if changed {
		t.Error("a confirmed booking was cancelled by a TTL release; only held bookings expire")
	}
}
