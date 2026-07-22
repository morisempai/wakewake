package domain

import (
	"testing"
	"time"

	"github.com/morisempai/wakewake/shared/contracts/events"
)

const (
	tCustomerID = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c0001"
	tProductID  = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c0002"
	tResourceID = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c0003"
	tBookingID  = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c0004"
	tResID      = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c0005"
	tPaymentID  = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c0006"
)

var (
	tNow    = time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	tStart  = tNow.Add(24 * time.Hour)
	tEnd    = tStart.Add(time.Hour)
	tExpiry = tNow.Add(15 * time.Minute)
)

func testProduct() Product {
	return Product{ResourceID: tResourceID, Capacity: 4, PriceMinor: 12000, Currency: "EUR"}
}

func testReservation() Reservation {
	return Reservation{ID: tResID, BookingID: tBookingID, ExpiresAt: tExpiry}
}

func heldBooking(t *testing.T) Booking {
	t.Helper()
	b, err := NewBooking(tBookingID, tCustomerID, tProductID, testProduct(), tStart, tEnd, 2, testReservation(), tNow)
	if err != nil {
		t.Fatalf("NewBooking: %v", err)
	}
	return b
}

func TestNewBookingBuildsAHeldBookingFromTheProductAndReservation(t *testing.T) {
	t.Parallel()

	b := heldBooking(t)

	if b.Status != StatusHeld {
		t.Errorf("status = %q, want held", b.Status)
	}
	if b.ResourceID != tResourceID {
		t.Errorf("resource_id = %q, want the product's %q", b.ResourceID, tResourceID)
	}
	if b.ReservationID == nil || *b.ReservationID != tResID {
		t.Errorf("reservation_id = %v, want %q", b.ReservationID, tResID)
	}
	if b.HoldExpiresAt == nil || !b.HoldExpiresAt.Equal(tExpiry) {
		t.Errorf("hold_expires_at = %v, want the reservation's %v", b.HoldExpiresAt, tExpiry)
	}
	if b.TotalMinor != 12000 || b.Currency != "EUR" {
		t.Errorf("price = %d %s, want 12000 EUR", b.TotalMinor, b.Currency)
	}
}

func TestNewBookingRejectsBadWindowsAndOversizedParties(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		start, end time.Time
		party      int
		want       error
	}{
		{"ends before starts", tStart, tStart.Add(-time.Hour), 2, ErrInvalidTimeWindow},
		{"equal bounds", tStart, tStart, 2, ErrInvalidTimeWindow},
		{"window in the past", tNow.Add(-2 * time.Hour), tNow.Add(-time.Hour), 2, ErrInvalidTimeWindow},
		{"party exceeds capacity", tStart, tEnd, 5, ErrPartyExceedsCapacity},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewBooking(tBookingID, tCustomerID, tProductID, testProduct(), tc.start, tc.end, tc.party, testReservation(), tNow)
			if err != tc.want {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestHeldEmissionCarriesTheContractPayload(t *testing.T) {
	t.Parallel()

	e := heldBooking(t).HeldEmission()
	if e.Event != events.BookingHeld {
		t.Fatalf("event = %q, want BookingHeld", e.Event)
	}
	p, ok := e.Payload.(events.BookingHeldPayload)
	if !ok {
		t.Fatalf("payload type = %T", e.Payload)
	}
	if p.BookingID != tBookingID || p.ReservationID != tResID || p.ResourceID != tResourceID {
		t.Errorf("payload identifiers wrong: %+v", p)
	}
	if !p.HoldExpiresAt.Equal(tExpiry) {
		t.Errorf("hold_expires_at = %v, want %v", p.HoldExpiresAt, tExpiry)
	}
	if p.TotalMinor != 12000 || p.Currency != "EUR" {
		t.Errorf("price wrong: %+v", p)
	}
}

func TestConfirmPromotesAHeldBookingAndEmitsBookingConfirmed(t *testing.T) {
	t.Parallel()

	next, emissions, err := heldBooking(t).Confirm(tPaymentID, tNow)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if next.Status != StatusConfirmed {
		t.Errorf("status = %q, want confirmed", next.Status)
	}
	if next.HoldExpiresAt != nil {
		t.Errorf("hold_expires_at = %v, want nil once confirmed", *next.HoldExpiresAt)
	}
	if len(emissions) != 1 || emissions[0].Event != events.BookingConfirmed {
		t.Fatalf("emissions = %+v, want one BookingConfirmed", emissions)
	}
	p := emissions[0].Payload.(events.BookingConfirmedPayload)
	if p.PaymentID != tPaymentID {
		t.Errorf("payment_id = %q, want %q", p.PaymentID, tPaymentID)
	}
}

func TestConfirmIsIdempotentAndRefusesACancelledBooking(t *testing.T) {
	t.Parallel()

	confirmed, _, _ := heldBooking(t).Confirm(tPaymentID, tNow)
	if _, emissions, err := confirmed.Confirm(tPaymentID, tNow); err != nil || len(emissions) != 0 {
		t.Errorf("re-confirm: err=%v emissions=%d, want no-op", err, len(emissions))
	}

	cancelled, _, _ := heldBooking(t).Cancel(ReasonHoldExpired, nil, tNow)
	if _, _, err := cancelled.Confirm(tPaymentID, tNow); err != ErrReservationReleased {
		t.Errorf("confirm of cancelled: err = %v, want ErrReservationReleased", err)
	}
}

func TestCancelMovesToCancelledAndRecordsThePreviousStatus(t *testing.T) {
	t.Parallel()

	next, emissions, err := heldBooking(t).Cancel(ReasonPaymentFailed, nil, tNow)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if next.Status != StatusCancelled {
		t.Errorf("status = %q, want cancelled", next.Status)
	}
	if next.CancelReason == nil || *next.CancelReason != ReasonPaymentFailed {
		t.Errorf("cancel_reason = %v, want payment_failed", next.CancelReason)
	}
	p := emissions[0].Payload.(events.BookingCancelledPayload)
	if p.PreviousStatus != events.BookingStatusHeld {
		t.Errorf("previous_status = %q, want held", p.PreviousStatus)
	}
	if p.Reason != events.CancelPaymentFailed {
		t.Errorf("reason = %q, want payment_failed", p.Reason)
	}

	// From confirmed the previous_status must be confirmed.
	confirmed, _, _ := heldBooking(t).Confirm(tPaymentID, tNow)
	_, ems, _ := confirmed.Cancel(ReasonCustomerCancelled, nil, tNow)
	if ems[0].Payload.(events.BookingCancelledPayload).PreviousStatus != events.BookingStatusConfirmed {
		t.Error("previous_status from a confirmed booking must be confirmed")
	}
}

func TestCancellingAnAlreadyCancelledBookingEmitsNothing(t *testing.T) {
	t.Parallel()

	once, _, _ := heldBooking(t).Cancel(ReasonPaymentFailed, nil, tNow)
	next, emissions, err := once.Cancel(ReasonCustomerCancelled, nil, tNow)
	if err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	if len(emissions) != 0 {
		t.Errorf("second cancel emitted %d events, want 0 — the fact happened once", len(emissions))
	}
	if next.CancelReason == nil || *next.CancelReason != ReasonPaymentFailed {
		t.Errorf("cancel_reason = %v, want the original payment_failed — a re-cancel must not rewrite it", next.CancelReason)
	}
}
