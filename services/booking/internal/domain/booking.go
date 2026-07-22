// Package domain holds the booking lifecycle: what a booking is, which state changes are legal,
// and which facts each one makes true.
//
// It imports nothing but the standard library and shared/contracts/events (pure types). No pgx,
// no amqp, no otel, no HTTP client — CI enforces this (ADR-0009). The saga's orchestration lives
// in service.go over narrow interfaces (ports.go); everything that needs a driver or a socket is
// implemented in internal/infra. The reason is not purity for its own sake: the state machine is
// the part that can be reasoned about and tested without a database or a live Availability.
//
// Time is always a parameter, never read from the clock. A domain that calls time.Now() cannot be
// tested for "held in the past" or "expired" without waiting.
package domain

import (
	"time"

	"github.com/morisempai/wakewake/shared/contracts/events"
)

// Status is the booking lifecycle state, matching the BookingStatus enum in
// contracts/openapi/booking.yaml.
type Status string

const (
	// StatusHeld — slot reserved, TTL running, awaiting payment.
	StatusHeld Status = "held"
	// StatusConfirmed — payment succeeded; the booking is real.
	StatusConfirmed Status = "confirmed"
	// StatusCancelled — terminal; slot released.
	StatusCancelled Status = "cancelled"
)

// CancelReason is why a booking reached the cancelled state, matching BookingCancelled.reason in
// the AsyncAPI contract.
type CancelReason string

const (
	ReasonPaymentFailed     CancelReason = "payment_failed"
	ReasonHoldExpired       CancelReason = "hold_expired"
	ReasonCustomerCancelled CancelReason = "customer_cancelled"
	ReasonOperatorCancelled CancelReason = "operator_cancelled"
)

// Product is the subset of a catalog product booking needs to hold a slot: the resource to
// reserve, the capacity to validate party_size against, and the price to charge. Catalog owns the
// product -> resource mapping and the pricing; booking reads it through the Catalog port and never
// touches catalog's tables (hard rule #6).
type Product struct {
	ResourceID string
	Capacity   int
	PriceMinor int64
	Currency   string
}

// Reservation is the subset of an Availability reservation booking keeps: its id, the booking id
// Availability recorded it against, and the instant its hold expires. All three come from
// Availability's response.
//
// BookingID is load-bearing for idempotency. Availability dedupes createReservation on the
// Idempotency-Key, so N concurrent create-hold requests carrying the same key all receive the SAME
// reservation — carrying the booking id the FIRST of them sent. Booking adopts that id as the
// booking's own, so every concurrent caller converges on one booking whose id matches the id
// Availability holds the reservation against. Minting a fresh booking id per caller instead would
// leave Availability's reservation pointing at a booking id that does not exist, and the eventual
// BookingCancelled would dead-letter at Availability's booking-id check.
//
// expires_at is computed by Availability's database clock, so booking's hold_expires_at agrees with
// it by construction rather than by two clocks happening to line up.
type Reservation struct {
	ID        string
	BookingID string
	ExpiresAt time.Time
}

// Emission is one event a transition made true, to be staged in the outbox inside the same
// transaction as the state change (ADR-0002).
//
// Domain returns these rather than publishing: publishing needs a broker, and a domain that can
// reach a broker can publish a fact whose transaction later rolled back.
type Emission struct {
	Event       string
	AggregateID string
	Payload     any
}

// Booking is one held, confirmed, or cancelled booking.
//
// Transitions are value methods returning a NEW Booking, so a rejected transition cannot leave a
// half-mutated entity behind for a caller that ignored the error.
type Booking struct {
	ID         string
	CustomerID string
	ProductID  string
	ResourceID string

	// ReservationID is the Availability reservation backing this booking. A pointer because the
	// contract types it `oneOf: [Uuid, null]`; kept populated through cancellation so compensation
	// can still name the reservation to free.
	ReservationID *string

	StartsAt  time.Time
	EndsAt    time.Time
	PartySize int

	Status Status

	// HoldExpiresAt is non-nil exactly when Status is StatusHeld (the schema enforces the
	// biconditional). It is the instant Availability's sweeper releases the hold unless payment
	// confirms it first.
	HoldExpiresAt *time.Time

	TotalMinor int64
	Currency   string

	// CancelReason is non-nil exactly when Status is StatusCancelled. CancellationReason is the
	// optional free-text note a customer may attach when cancelling.
	CancelReason       *CancelReason
	CancellationReason *string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewBooking validates a create-hold request against the resolved product and the reservation
// Availability already created, and returns the held booking to insert.
//
// party_size, capacity and the window are the domain rules; product identity, pricing and the
// reservation come from the Catalog and Availability ports the service called first. The
// reservation's expiry becomes this booking's hold_expires_at, so the two agree on the deadline.
func NewBooking(id, customerID, productID string, product Product, startsAt, endsAt time.Time, partySize int, reservation Reservation, now time.Time) (Booking, error) {
	if id == "" || customerID == "" || productID == "" {
		return Booking{}, ErrMissingIdentifier
	}
	// Strictly after, and not already in the past — the two window rules the contract renders as
	// 422 invalid_time_window.
	if !endsAt.After(startsAt) || !startsAt.After(now.Add(-time.Second)) {
		return Booking{}, ErrInvalidTimeWindow
	}
	if partySize < 1 {
		return Booking{}, ErrInvalidTimeWindow
	}
	if partySize > product.Capacity {
		return Booking{}, ErrPartyExceedsCapacity
	}

	reservationID := reservation.ID
	expires := reservation.ExpiresAt.UTC()
	return Booking{
		ID:            id,
		CustomerID:    customerID,
		ProductID:     productID,
		ResourceID:    product.ResourceID,
		ReservationID: &reservationID,
		StartsAt:      startsAt.UTC(),
		EndsAt:        endsAt.UTC(),
		PartySize:     partySize,
		Status:        StatusHeld,
		HoldExpiresAt: &expires,
		TotalMinor:    product.PriceMinor,
		Currency:      product.Currency,
		CreatedAt:     now.UTC(),
		UpdatedAt:     now.UTC(),
	}, nil
}

// HeldEmission is the BookingHeld fact for a freshly created hold. Payment consumes it to prepare
// a payment context.
func (b Booking) HeldEmission() Emission {
	var expires time.Time
	if b.HoldExpiresAt != nil {
		expires = *b.HoldExpiresAt
	}
	return Emission{
		Event:       events.BookingHeld,
		AggregateID: b.ID,
		Payload: events.BookingHeldPayload{
			BookingID:     b.ID,
			CustomerID:    b.CustomerID,
			ProductID:     b.ProductID,
			ResourceID:    b.ResourceID,
			ReservationID: deref(b.ReservationID),
			StartsAt:      b.StartsAt,
			EndsAt:        b.EndsAt,
			HoldExpiresAt: expires,
			TotalMinor:    b.TotalMinor,
			Currency:      b.Currency,
		},
	}
}

// Confirm promotes a held booking on the payment-success path and states BookingConfirmed.
//
// Confirming an already-confirmed booking is a no-op returning no error and no second emission —
// the contract makes the confirm path idempotent because delivery is at-least-once. Confirming a
// cancelled booking is refused: cancelled is terminal, and a payment that lands for a booking that
// was already cancelled (its hold swept, or a prior failure) needs reconciliation, not a silent
// resurrection.
func (b Booking) Confirm(paymentID string, now time.Time) (Booking, []Emission, error) {
	switch b.Status {
	case StatusCancelled:
		return b, nil, ErrReservationReleased
	case StatusConfirmed:
		return b, nil, nil
	}

	next := b
	next.Status = StatusConfirmed
	// Cleared: a confirmed booking has no hold to expire, and the schema's held-iff-expiry CHECK
	// refuses the row otherwise.
	next.HoldExpiresAt = nil
	next.UpdatedAt = now.UTC()

	return next, []Emission{{
		Event:       events.BookingConfirmed,
		AggregateID: b.ID,
		Payload: events.BookingConfirmedPayload{
			BookingID:     b.ID,
			CustomerID:    b.CustomerID,
			ProductID:     b.ProductID,
			ResourceID:    b.ResourceID,
			ReservationID: deref(b.ReservationID),
			PaymentID:     paymentID,
			StartsAt:      b.StartsAt,
			EndsAt:        b.EndsAt,
			TotalMinor:    b.TotalMinor,
			Currency:      b.Currency,
		},
	}}, nil
}

// Cancel moves a held or confirmed booking to the cancelled terminal state and states
// BookingCancelled — the fact Availability consumes to release the reservation.
//
// Cancelling an already-cancelled booking is a no-op returning no error and, importantly, NO
// second emission: BookingCancelled happened once, and a duplicate would ask Availability to
// release a window that may by then belong to someone else. The original reason is kept.
func (b Booking) Cancel(reason CancelReason, note *string, now time.Time) (Booking, []Emission, error) {
	if !reason.valid() {
		return b, nil, ErrInvalidTimeWindow
	}
	if b.Status == StatusCancelled {
		return b, nil, nil
	}

	previous := b.Status

	next := b
	next.Status = StatusCancelled
	next.HoldExpiresAt = nil
	r := reason
	next.CancelReason = &r
	if note != nil {
		next.CancellationReason = note
	}
	next.UpdatedAt = now.UTC()

	return next, []Emission{{
		Event:       events.BookingCancelled,
		AggregateID: b.ID,
		Payload: events.BookingCancelledPayload{
			BookingID:      b.ID,
			CustomerID:     b.CustomerID,
			ResourceID:     b.ResourceID,
			ReservationID:  b.ReservationID,
			PreviousStatus: events.BookingStatus(previous),
			Reason:         events.CancelReason(reason),
		},
	}}, nil
}

func (r CancelReason) valid() bool {
	switch r {
	case ReasonPaymentFailed, ReasonHoldExpired, ReasonCustomerCancelled, ReasonOperatorCancelled:
		return true
	default:
		return false
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
