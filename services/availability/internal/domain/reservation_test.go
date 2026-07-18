package domain

import (
	"errors"
	"testing"
	"time"

	"github.com/morisempai/wakewake/shared/contracts/events"
)

// Fixed instants. Time is never read from the clock inside domain code, so every test states the
// instant it means — a suite that calls time.Now() cannot distinguish "expired" from "flaky".
var (
	windowStart = time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	windowEnd   = time.Date(2026, 7, 20, 11, 0, 0, 0, time.UTC)
	createdAt   = time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
)

const (
	resID  = "01912d5a-7f3e-7c1a-9b2e-000000000001"
	bookID = "01912d5a-7f3e-7c1a-9b2e-000000000002"
	resvID = "01912d5a-7f3e-7c1a-9b2e-000000000003"
)

// heldReservation is a persisted `held` row as the store would hand it back: expires_at set,
// created/updated stamped by the database.
func heldReservation(t *testing.T, expiresAt time.Time) Reservation {
	t.Helper()
	exp := expiresAt
	return Reservation{
		ID:         resvID,
		ResourceID: resID,
		BookingID:  bookID,
		StartsAt:   windowStart,
		EndsAt:     windowEnd,
		Status:     StatusHeld,
		ExpiresAt:  &exp,
		CreatedAt:  createdAt,
		UpdatedAt:  createdAt,
	}
}

// AC6: `ends_at <= starts_at` is a domain rule violation, which the contract renders as
// 422 invalid_time_window. Equal bounds matter as much as inverted ones: an equal-bounds '[)'
// range normalises to `empty` in Postgres and would overlap nothing, slipping past the
// exclusion constraint entirely (ADR-0011).
func TestRejectsWindowWhoseEndIsNotAfterItsStart_Issue5_AC6(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		startsAt time.Time
		endsAt   time.Time
	}{
		{"end equals start", windowStart, windowStart},
		{"end before start", windowEnd, windowStart},
		{"end one nanosecond before start", windowStart.Add(time.Nanosecond), windowStart},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewReservation(resvID, resID, bookID, tc.startsAt, tc.endsAt)

			if !errors.Is(err, ErrInvalidTimeWindow) {
				t.Errorf("NewReservation(%s, %s) error = %v, want ErrInvalidTimeWindow",
					tc.startsAt, tc.endsAt, err)
			}
		})
	}
}

func TestAcceptsWindowWhoseEndIsAfterItsStart_Issue5_AC6(t *testing.T) {
	t.Parallel()

	r, err := NewReservation(resvID, resID, bookID, windowStart, windowEnd)

	if err != nil {
		t.Fatalf("NewReservation returned %v, want nil", err)
	}
	if r.ID != resvID || r.ResourceID != resID || r.BookingID != bookID {
		t.Errorf("identifiers not carried through: %+v", r)
	}
	if !r.StartsAt.Equal(windowStart) || !r.EndsAt.Equal(windowEnd) {
		t.Errorf("window = [%s, %s), want [%s, %s)", r.StartsAt, r.EndsAt, windowStart, windowEnd)
	}
}

// A new reservation is born `held` — the contract's createReservation says "Created in `held`
// state with `expires_at` set from BOOKING_HOLD_TTL_SECONDS".
func TestNewReservationIsHeld(t *testing.T) {
	t.Parallel()

	r, err := NewReservation(resvID, resID, bookID, windowStart, windowEnd)
	if err != nil {
		t.Fatalf("NewReservation returned %v", err)
	}

	if r.Status != StatusHeld {
		t.Errorf("status = %q, want %q", r.Status, StatusHeld)
	}
	if r.ReleasedReason != nil {
		t.Errorf("released_reason = %v, want nil on a fresh hold", *r.ReleasedReason)
	}
}

func TestRejectsMissingIdentifiers(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name                  string
		id, resource, booking string
	}{
		{"no reservation id", "", resID, bookID},
		{"no resource id", resvID, "", bookID},
		{"no booking id", resvID, resID, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewReservation(tc.id, tc.resource, tc.booking, windowStart, windowEnd)

			if !errors.Is(err, ErrMissingIdentifier) {
				t.Errorf("error = %v, want ErrMissingIdentifier", err)
			}
		})
	}
}

// Confirming clears expires_at. The schema's `CHECK ((status = 'held') = (expires_at IS NOT
// NULL))` refuses the row otherwise, and leaving it set would let the sweeper release a
// reservation after it had been paid for.
func TestConfirmPromotesHeldAndClearsExpiry(t *testing.T) {
	t.Parallel()

	now := createdAt.Add(time.Minute)
	held := heldReservation(t, createdAt.Add(15*time.Minute))

	got, emissions, err := held.Confirm(now)

	if err != nil {
		t.Fatalf("Confirm returned %v, want nil", err)
	}
	if got.Status != StatusConfirmed {
		t.Errorf("status = %q, want %q", got.Status, StatusConfirmed)
	}
	if got.ExpiresAt != nil {
		t.Errorf("expires_at = %v, want nil once confirmed", *got.ExpiresAt)
	}
	if !got.UpdatedAt.Equal(now) {
		t.Errorf("updated_at = %s, want %s", got.UpdatedAt, now)
	}
	// The AsyncAPI contract defines no ReservationConfirmed event. Emitting one would be
	// inventing a contract.
	if len(emissions) != 0 {
		t.Errorf("Confirm emitted %d event(s), want 0 — the contract has no ReservationConfirmed",
			len(emissions))
	}
}

// "Idempotent: confirming an already-confirmed reservation returns 200."
func TestConfirmIsIdempotent(t *testing.T) {
	t.Parallel()

	confirmed := heldReservation(t, createdAt.Add(15*time.Minute))
	confirmed.Status = StatusConfirmed
	confirmed.ExpiresAt = nil

	got, emissions, err := confirmed.Confirm(createdAt.Add(time.Hour))

	if err != nil {
		t.Fatalf("Confirm on an already-confirmed reservation returned %v, want nil", err)
	}
	if got.Status != StatusConfirmed {
		t.Errorf("status = %q, want %q", got.Status, StatusConfirmed)
	}
	if len(emissions) != 0 {
		t.Errorf("emitted %d event(s), want 0", len(emissions))
	}
}

// "422 — Cannot confirm — the reservation was already released."
func TestConfirmRejectsAReleasedReservation(t *testing.T) {
	t.Parallel()

	reason := ReasonHoldExpired
	released := heldReservation(t, createdAt.Add(15*time.Minute))
	released.Status = StatusReleased
	released.ExpiresAt = nil
	released.ReleasedReason = &reason

	_, _, err := released.Confirm(createdAt.Add(time.Hour))

	if !errors.Is(err, ErrAlreadyReleased) {
		t.Errorf("error = %v, want ErrAlreadyReleased", err)
	}
}

// Releasing emits ReservationReleased. The payload's fields and their values are the contract
// (contracts/asyncapi/booking-events.yaml); a wrong resource_id here silently breaks Booking.
func TestReleaseEmitsReservationReleasedWithTheContractPayload(t *testing.T) {
	t.Parallel()

	now := createdAt.Add(time.Minute)
	held := heldReservation(t, createdAt.Add(15*time.Minute))

	got, emissions, err := held.Release(ReasonPaymentFailed, now)

	if err != nil {
		t.Fatalf("Release returned %v, want nil", err)
	}
	if got.Status != StatusReleased {
		t.Errorf("status = %q, want %q", got.Status, StatusReleased)
	}
	if got.ReleasedReason == nil || *got.ReleasedReason != ReasonPaymentFailed {
		t.Errorf("released_reason = %v, want %q", got.ReleasedReason, ReasonPaymentFailed)
	}
	if got.ExpiresAt != nil {
		t.Errorf("expires_at = %v, want nil once released", *got.ExpiresAt)
	}

	if len(emissions) != 1 {
		t.Fatalf("emitted %d event(s), want exactly 1 ReservationReleased", len(emissions))
	}
	e := emissions[0]
	if e.Event != events.ReservationReleased {
		t.Errorf("event = %q, want %q", e.Event, events.ReservationReleased)
	}
	if e.AggregateID != resvID {
		t.Errorf("aggregate_id = %q, want the reservation id %q", e.AggregateID, resvID)
	}

	payload, ok := e.Payload.(events.ReservationReleasedPayload)
	if !ok {
		t.Fatalf("payload is %T, want events.ReservationReleasedPayload", e.Payload)
	}
	if payload.ReservationID != resvID {
		t.Errorf("payload.reservation_id = %q, want %q", payload.ReservationID, resvID)
	}
	if payload.ResourceID != resID {
		t.Errorf("payload.resource_id = %q, want %q", payload.ResourceID, resID)
	}
	if payload.BookingID != bookID {
		t.Errorf("payload.booking_id = %q, want %q", payload.BookingID, bookID)
	}
	if !payload.StartsAt.Equal(windowStart) || !payload.EndsAt.Equal(windowEnd) {
		t.Errorf("payload window = [%s, %s), want [%s, %s)",
			payload.StartsAt, payload.EndsAt, windowStart, windowEnd)
	}
	if payload.Reason != events.ReleasePaymentFailed {
		t.Errorf("payload.reason = %q, want %q", payload.Reason, events.ReleasePaymentFailed)
	}
}

func TestReleaseWorksFromConfirmed(t *testing.T) {
	t.Parallel()

	confirmed := heldReservation(t, createdAt.Add(15*time.Minute))
	confirmed.Status = StatusConfirmed
	confirmed.ExpiresAt = nil

	got, emissions, err := confirmed.Release(ReasonBookingCancelled, createdAt.Add(time.Hour))

	if err != nil {
		t.Fatalf("Release from confirmed returned %v, want nil", err)
	}
	if got.Status != StatusReleased {
		t.Errorf("status = %q, want %q", got.Status, StatusReleased)
	}
	if len(emissions) != 1 {
		t.Errorf("emitted %d event(s), want 1", len(emissions))
	}
}

// "Idempotent: releasing an already-released reservation returns 200." It must NOT emit a second
// ReservationReleased — the event is a fact, and the fact happened once. Booking cancels the
// abandoned booking on hold_expired, so a duplicate is a second cancellation of a live booking.
func TestReleaseIsIdempotentAndEmitsNothingTheSecondTime(t *testing.T) {
	t.Parallel()

	first := heldReservation(t, createdAt.Add(15*time.Minute))
	released, _, err := first.Release(ReasonHoldExpired, createdAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("first Release returned %v", err)
	}

	got, emissions, err := released.Release(ReasonBookingCancelled, createdAt.Add(time.Hour))

	if err != nil {
		t.Fatalf("second Release returned %v, want nil", err)
	}
	if len(emissions) != 0 {
		t.Errorf("second Release emitted %d event(s), want 0", len(emissions))
	}
	if got.ReleasedReason == nil || *got.ReleasedReason != ReasonHoldExpired {
		t.Errorf("released_reason = %v, want the original %q — a re-release must not rewrite history",
			got.ReleasedReason, ReasonHoldExpired)
	}
	if !got.UpdatedAt.Equal(released.UpdatedAt) {
		t.Errorf("updated_at moved on a no-op release: %s, want %s", got.UpdatedAt, released.UpdatedAt)
	}
}

func TestReleaseRejectsAReasonOutsideTheContractEnum(t *testing.T) {
	t.Parallel()

	held := heldReservation(t, createdAt.Add(15*time.Minute))

	_, _, err := held.Release(ReleaseReason("because_i_said_so"), createdAt.Add(time.Minute))

	if !errors.Is(err, ErrInvalidReleaseReason) {
		t.Errorf("error = %v, want ErrInvalidReleaseReason", err)
	}
}

// AC2, domain half: the sweeper's decision of what is due. The store finds candidates, but the
// re-check happens here under the row lock — a reservation confirmed between the scan and the
// update must survive.
func TestReleaseIfExpiredOnlyReleasesHoldsThatArePastTheirExpiry_Issue5_AC2(t *testing.T) {
	t.Parallel()

	expiry := createdAt.Add(15 * time.Minute)

	confirmed := heldReservation(t, expiry)
	confirmed.Status = StatusConfirmed
	confirmed.ExpiresAt = nil

	alreadyReleased := heldReservation(t, expiry)
	alreadyReleased.Status = StatusReleased
	alreadyReleased.ExpiresAt = nil
	reason := ReasonPaymentFailed
	alreadyReleased.ReleasedReason = &reason

	cases := []struct {
		name        string
		reservation Reservation
		now         time.Time
		wantRelease bool
	}{
		{"held and past expiry", heldReservation(t, expiry), expiry.Add(time.Second), true},
		{"held exactly at expiry", heldReservation(t, expiry), expiry, true},
		{"held but not yet expired", heldReservation(t, expiry), expiry.Add(-time.Second), false},
		{"confirmed between scan and sweep", confirmed, expiry.Add(time.Hour), false},
		{"already released", alreadyReleased, expiry.Add(time.Hour), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, emissions, err := tc.reservation.ReleaseIfExpired(tc.now)
			if err != nil {
				t.Fatalf("ReleaseIfExpired returned %v, want nil", err)
			}

			released := len(emissions) == 1
			if released != tc.wantRelease {
				t.Fatalf("emitted %d event(s); released = %v, want %v", len(emissions), released, tc.wantRelease)
			}
			if !tc.wantRelease {
				if got.Status != tc.reservation.Status {
					t.Errorf("status changed to %q on a reservation that was not due", got.Status)
				}
				return
			}

			if got.Status != StatusReleased {
				t.Errorf("status = %q, want %q", got.Status, StatusReleased)
			}
			if got.ReleasedReason == nil || *got.ReleasedReason != ReasonHoldExpired {
				t.Errorf("released_reason = %v, want %q — the sweeper's reason is fixed by the contract",
					got.ReleasedReason, ReasonHoldExpired)
			}
			payload, ok := emissions[0].Payload.(events.ReservationReleasedPayload)
			if !ok {
				t.Fatalf("payload is %T, want events.ReservationReleasedPayload", emissions[0].Payload)
			}
			if payload.Reason != events.ReleaseHoldExpired {
				t.Errorf("payload.reason = %q, want %q", payload.Reason, events.ReleaseHoldExpired)
			}
		})
	}
}

// The ReservationCreated payload the store stages after the insert returns. expires_at is
// required and non-nullable in the AsyncAPI schema, so it is read from the persisted row rather
// than recomputed.
func TestCreatedEmissionCarriesTheContractPayload(t *testing.T) {
	t.Parallel()

	expiry := createdAt.Add(15 * time.Minute)
	held := heldReservation(t, expiry)

	e := held.CreatedEmission()

	if e.Event != events.ReservationCreated {
		t.Errorf("event = %q, want %q", e.Event, events.ReservationCreated)
	}
	if e.AggregateID != resvID {
		t.Errorf("aggregate_id = %q, want %q", e.AggregateID, resvID)
	}
	payload, ok := e.Payload.(events.ReservationCreatedPayload)
	if !ok {
		t.Fatalf("payload is %T, want events.ReservationCreatedPayload", e.Payload)
	}
	if payload.ReservationID != resvID || payload.ResourceID != resID || payload.BookingID != bookID {
		t.Errorf("identifiers wrong: %+v", payload)
	}
	if !payload.StartsAt.Equal(windowStart) || !payload.EndsAt.Equal(windowEnd) {
		t.Errorf("window = [%s, %s), want [%s, %s)",
			payload.StartsAt, payload.EndsAt, windowStart, windowEnd)
	}
	if !payload.ExpiresAt.Equal(expiry) {
		t.Errorf("expires_at = %s, want %s", payload.ExpiresAt, expiry)
	}
}

func TestReleaseReasonValidMatchesTheContractEnum(t *testing.T) {
	t.Parallel()

	for _, reason := range []ReleaseReason{ReasonPaymentFailed, ReasonBookingCancelled, ReasonHoldExpired} {
		if !reason.Valid() {
			t.Errorf("%q is in the contract enum but Valid() said false", reason)
		}
	}
	for _, reason := range []ReleaseReason{"", "customer_cancelled", "operator_cancelled", "expired"} {
		if reason.Valid() {
			t.Errorf("%q is not in the contract enum but Valid() said true", reason)
		}
	}
}
