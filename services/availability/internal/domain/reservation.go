// Package domain holds the reservation lifecycle: what a reservation is, which state changes are
// legal, and which facts each one makes true.
//
// It imports nothing but the standard library and shared/contracts/events (pure types). No pgx,
// no amqp, no otel — CI enforces this (ADR-0009). The reason is not purity for its own sake: the
// no-double-booking invariant lives in the database, and the transitions here are the part that
// can be reasoned about and tested without one. Everything that needs a driver is a narrow
// interface declared in ports.go and implemented in internal/infra.
//
// Time is always a parameter, never read from the clock. A domain that calls time.Now() cannot
// be tested for "expired" without waiting, and the TTL sweep is exactly the behaviour that
// matters most here.
package domain

import (
	"time"

	"github.com/morisempai/wakewake/shared/contracts/events"
)

// Status is the reservation lifecycle state, matching the ReservationStatus enum in
// contracts/openapi/availability.yaml.
//
// `held` and `confirmed` are the LIVE states and block overlapping reservations; `released` is
// terminal history and does not (ADR-0011). Adding a status means deciding which side it falls
// on, in both this file and migrations/0001_reservation.sql.
type Status string

const (
	// StatusHeld — window is locked, TTL running, awaiting payment.
	StatusHeld Status = "held"
	// StatusConfirmed — payment succeeded; permanent.
	StatusConfirmed Status = "confirmed"
	// StatusReleased — freed by compensation or TTL expiry; terminal.
	StatusReleased Status = "released"
)

// Live reports whether this status blocks an overlapping reservation. It is the Go-side
// statement of the exclusion constraint's `WHERE (status <> 'released')` predicate.
func (s Status) Live() bool { return s != StatusReleased }

// ReleaseReason is why a reservation was freed, matching the ReleaseReason enum in both the
// OpenAPI and AsyncAPI contracts.
type ReleaseReason string

const (
	ReasonPaymentFailed    ReleaseReason = "payment_failed"
	ReasonBookingCancelled ReleaseReason = "booking_cancelled"
	ReasonHoldExpired      ReleaseReason = "hold_expired"
)

// Valid reports whether the reason is one the contracts define.
//
// Checked rather than assumed because the value arrives from a request body and from an event
// payload, and an unrecognised one would otherwise reach the `release_reason` enum column as a
// SQLSTATE 22P02 — a 500 for what is a client error.
func (r ReleaseReason) Valid() bool {
	switch r {
	case ReasonPaymentFailed, ReasonBookingCancelled, ReasonHoldExpired:
		return true
	default:
		return false
	}
}

// Window is a half-open [StartsAt, EndsAt) interval in UTC — the same bounds as the `during`
// tstzrange, so back-to-back windows do not overlap.
type Window struct {
	StartsAt time.Time
	EndsAt   time.Time
}

// Emission is one event a transition made true, to be staged in the outbox inside the same
// transaction as the state change (ADR-0002).
//
// Domain returns these rather than publishing: publishing needs a broker, and a domain that can
// reach a broker can publish a fact whose transaction later rolled back.
type Emission struct {
	// Event is the AsyncAPI event name, from shared/contracts/events.
	Event string
	// AggregateID is what the event is about — always the reservation id here.
	AggregateID string
	// Payload is the typed struct from shared/contracts/events.
	Payload any
}

// Reservation is one held, confirmed, or released window on one resource.
//
// The zero value is not meaningful; build one with NewReservation or load it from the store.
// Transitions are value methods returning a NEW Reservation, so a rejected transition cannot
// leave a half-mutated entity behind for a caller that ignored the error.
type Reservation struct {
	ID         string
	ResourceID string
	BookingID  string

	// StartsAt is inclusive, EndsAt exclusive — the '[)' bounds of the `during` range.
	StartsAt time.Time
	EndsAt   time.Time

	Status Status

	// ExpiresAt is non-nil exactly when Status is StatusHeld. The schema enforces the biconditional:
	// a held row with no expiry is invisible to the sweeper and holds its slot forever, and a
	// confirmed row with one gets swept after it was paid for.
	ExpiresAt *time.Time

	// ReleasedReason is non-nil exactly when Status is StatusReleased.
	ReleasedReason *ReleaseReason

	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewReservation validates a reserve request and returns the held reservation to insert.
//
// It deliberately does NOT set ExpiresAt. The TTL deadline is computed by the database as
// `now() + interval` in the INSERT, because the sweeper compares `expires_at <= now()` against
// that same clock — deriving the deadline from the application clock and the due-check from the
// database clock means holds expire early or late by however far the two have drifted.
func NewReservation(id, resourceID, bookingID string, startsAt, endsAt time.Time) (Reservation, error) {
	if id == "" || resourceID == "" || bookingID == "" {
		return Reservation{}, ErrMissingIdentifier
	}
	// Strictly after: equal bounds normalise to an `empty` tstzrange, which overlaps nothing and
	// would slip past the exclusion constraint entirely (ADR-0011).
	if !endsAt.After(startsAt) {
		return Reservation{}, ErrInvalidTimeWindow
	}

	return Reservation{
		ID:         id,
		ResourceID: resourceID,
		BookingID:  bookingID,
		StartsAt:   startsAt.UTC(),
		EndsAt:     endsAt.UTC(),
		Status:     StatusHeld,
	}, nil
}

// Confirm promotes a held reservation on the payment-success path.
//
// Confirming an already-confirmed reservation is a no-op returning no error — the contract makes
// it idempotent, because the caller cannot distinguish a lost response from a failed request and
// will retry. Confirming a released one is ErrAlreadyReleased: released is terminal, so it can
// never become valid by retrying, which is why the contract renders it 422 rather than 409.
//
// It emits nothing. The AsyncAPI contract defines no ReservationConfirmed event, and inventing
// one here would be defining a contract rather than implementing it.
func (r Reservation) Confirm(now time.Time) (Reservation, []Emission, error) {
	switch r.Status {
	case StatusReleased:
		return r, nil, ErrAlreadyReleased
	case StatusConfirmed:
		return r, nil, nil
	}

	next := r
	next.Status = StatusConfirmed
	// Cleared so the sweeper cannot reach a reservation that has been paid for. The schema's
	// held-iff-expiry CHECK refuses the row otherwise.
	next.ExpiresAt = nil
	next.UpdatedAt = now.UTC()
	return next, nil, nil
}

// Release frees the window and states the fact.
//
// Releasing an already-released reservation is a no-op returning no error and, importantly, NO
// second emission. ReservationReleased is a fact that happened once; Booking cancels the
// abandoned booking when it sees `hold_expired`, so a duplicate would be a second cancellation
// of what may by then be a live booking. The original reason is kept — a re-release must not
// rewrite why the window was freed.
func (r Reservation) Release(reason ReleaseReason, now time.Time) (Reservation, []Emission, error) {
	if !reason.Valid() {
		return r, nil, ErrInvalidReleaseReason
	}
	if r.Status == StatusReleased {
		return r, nil, nil
	}

	next := r
	next.Status = StatusReleased
	next.ReleasedReason = &reason
	next.ExpiresAt = nil
	next.UpdatedAt = now.UTC()

	return next, []Emission{{
		Event:       events.ReservationReleased,
		AggregateID: r.ID,
		Payload: events.ReservationReleasedPayload{
			ReservationID: r.ID,
			ResourceID:    r.ResourceID,
			BookingID:     r.BookingID,
			StartsAt:      r.StartsAt,
			EndsAt:        r.EndsAt,
			Reason:        events.ReleaseReason(reason),
		},
	}}, nil
}

// ReleaseIfExpired is the TTL sweeper's decision, made under the row lock.
//
// The sweeper scans for candidates and then re-reads each one inside a transaction; this is the
// re-check. Between the scan and the lock a reservation may have been confirmed by a successful
// payment, and sweeping it then would release a slot somebody has paid for. A no-op is returned
// rather than an error: nothing went wrong, the row simply is not due.
func (r Reservation) ReleaseIfExpired(now time.Time) (Reservation, []Emission, error) {
	if r.Status != StatusHeld || r.ExpiresAt == nil {
		return r, nil, nil
	}
	// `!Before` rather than `After`: a hold whose expiry is exactly now is due. The sweeper's
	// SQL uses `expires_at <= now()`, and a strict comparison here would leave rows the query
	// keeps returning and this check keeps declining — an endless, silent no-op loop.
	if now.Before(*r.ExpiresAt) {
		return r, nil, nil
	}
	return r.Release(ReasonHoldExpired, now)
}

// CreatedEmission is the ReservationCreated fact for a freshly inserted hold.
//
// Called after the INSERT returns, not before: the AsyncAPI schema makes `expires_at` required
// and non-nullable, and its value is the database's `now() + interval`, which only exists once
// the row does.
func (r Reservation) CreatedEmission() Emission {
	var expiresAt time.Time
	if r.ExpiresAt != nil {
		expiresAt = *r.ExpiresAt
	}
	return Emission{
		Event:       events.ReservationCreated,
		AggregateID: r.ID,
		Payload: events.ReservationCreatedPayload{
			ReservationID: r.ID,
			ResourceID:    r.ResourceID,
			BookingID:     r.BookingID,
			StartsAt:      r.StartsAt,
			EndsAt:        r.EndsAt,
			ExpiresAt:     expiresAt,
		},
	}
}
