package domain

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// CreateCommand is a validated-at-the-edge create-hold request. The customer id comes from the JWT
// claims, never from a request field (booking.yaml).
type CreateCommand struct {
	CustomerID string
	ProductID  string

	// StartsAt is inclusive, EndsAt exclusive. UTC.
	StartsAt time.Time
	EndsAt   time.Time

	PartySize int

	Idempotency Claim
}

// Service is the booking use-case layer and the saga orchestrator: it decides, the store persists,
// and the Catalog/Reservations ports reach the other services.
//
// It holds no state beyond its dependencies, so it is safe for concurrent use by every HTTP
// handler and every consumer at once.
type Service struct {
	store        Store
	reservations Reservations
	catalog      Catalog
	now          Clock
	newID        IDGen
	holdTTL      time.Duration
}

// NewService wires the service. The clock and id generator are parameters rather than package calls
// so tests are deterministic (testing-standards).
func NewService(store Store, reservations Reservations, catalog Catalog, now Clock, newID IDGen, holdTTL time.Duration) *Service {
	return &Service{store: store, reservations: reservations, catalog: catalog, now: now, newID: newID, holdTTL: holdTTL}
}

// CreateBooking is the saga's first step: resolve the product, reserve the window through
// Availability, and write the held booking plus BookingHeld in one transaction.
//
// The order matters. Catalog is consulted first so an unknown product or an oversized party is
// rejected before anything is reserved. The idempotency key is then checked so a retried request
// returns its original booking WITHOUT reserving a second time. Only a genuinely new request mints
// an id and calls Availability; the hold and its outbox row are written atomically last.
func (s *Service) CreateBooking(ctx context.Context, cmd CreateCommand) (Booking, error) {
	if cmd.CustomerID == "" || cmd.ProductID == "" {
		return Booking{}, ErrMissingIdentifier
	}

	product, err := s.catalog.Product(ctx, cmd.ProductID)
	if err != nil {
		return Booking{}, err
	}

	// Domain validation before any reservation, so a request the domain forbids never holds a slot.
	if err := validateCreate(cmd, product, s.now()); err != nil {
		return Booking{}, err
	}

	// Replay short-circuit: a retried request with the same key returns its original booking and
	// never calls Availability again. A different body under the same key is a reuse conflict.
	if bookingID, fingerprint, found, err := s.store.FindIdempotent(ctx, cmd.Idempotency.Key); err != nil {
		return Booking{}, err
	} else if found {
		if fingerprint != cmd.Idempotency.Fingerprint {
			return Booking{}, ErrIdempotencyKeyReuse
		}
		return s.store.ByID(ctx, bookingID)
	}

	candidateID, err := s.newID()
	if err != nil {
		return Booking{}, fmt.Errorf("booking: generating booking id: %w", err)
	}

	// The booking's own key travels to Availability, so a retried create-hold does not reserve
	// twice — Availability replays the same reservation for the same key.
	reservation, err := s.reservations.Hold(ctx, HoldRequest{
		ResourceID:     product.ResourceID,
		BookingID:      candidateID,
		StartsAt:       cmd.StartsAt,
		EndsAt:         cmd.EndsAt,
		IdempotencyKey: cmd.Idempotency.Key,
	})
	if err != nil {
		return Booking{}, err
	}

	// Adopt the booking id Availability recorded the reservation against — see Reservation.BookingID.
	booking, err := NewBooking(reservation.BookingID, cmd.CustomerID, cmd.ProductID, product,
		cmd.StartsAt, cmd.EndsAt, cmd.PartySize, reservation, s.now())
	if err != nil {
		return Booking{}, err
	}

	return s.store.Insert(ctx, cmd.Idempotency, booking)
}

// Get returns one booking the caller owns, or ErrNotFound / ErrForbidden.
func (s *Service) Get(ctx context.Context, id, customerID string) (Booking, error) {
	b, err := s.store.ByID(ctx, id)
	if err != nil {
		return Booking{}, err
	}
	if b.CustomerID != customerID {
		// 403 rather than 404: the id exists, it simply is not the caller's. The contract
		// distinguishes them, and leaking "no such booking" for another customer's id would be a
		// different, misleading answer.
		return Booking{}, ErrForbidden
	}
	return b, nil
}

// List returns a page of the caller's own bookings.
func (s *Service) List(ctx context.Context, q ListQuery) (ListPage, error) {
	return s.store.List(ctx, q)
}

// Cancel moves a booking the caller owns to the cancelled terminal state and stages
// BookingCancelled — Availability frees the slot by consuming it. Idempotent: cancelling an
// already-cancelled booking returns it unchanged.
func (s *Service) Cancel(ctx context.Context, id, customerID string, note *string) (Booking, error) {
	// Ownership is checked before the transition opens: an unauthorised cancel must not take a row
	// lock, and the check needs the row anyway.
	current, err := s.store.ByID(ctx, id)
	if err != nil {
		return Booking{}, err
	}
	if current.CustomerID != customerID {
		return Booking{}, ErrForbidden
	}

	b, _, err := s.store.Apply(ctx, id, func(c Booking) (Booking, []Emission, error) {
		return c.Cancel(ReasonCustomerCancelled, note, s.now())
	})
	return b, err
}

// ConfirmOnPayment is the transition the PaymentSucceeded consumer applies inside the dedupe
// transaction. It calls Availability's confirm synchronously while the booking row is locked, then
// promotes the booking and stages BookingConfirmed.
//
// If Availability reports the reservation was already released — its TTL swept the hold before
// payment landed — the booking cannot be confirmed, so it is cancelled with reason hold_expired
// instead. That a paid-for booking then reads as cancelled is a real "paid but lost the slot"
// situation needing a refund; refunds are out of scope for this slice (see booking.yaml), so this
// reaches the honest terminal state and states BookingCancelled rather than resurrecting a hold
// that no longer exists.
func (s *Service) ConfirmOnPayment(ctx context.Context, paymentID string) Transition {
	return func(current Booking) (Booking, []Emission, error) {
		switch current.Status {
		case StatusConfirmed:
			return current, nil, nil
		case StatusCancelled:
			// Payment landed for a booking already cancelled. Nothing to confirm; accept so the
			// dedupe row commits and the event is not retried forever. The handler logs it.
			return current, nil, nil
		}

		if current.ReservationID == nil {
			return Booking{}, nil, fmt.Errorf("booking: held booking %s has no reservation to confirm", current.ID)
		}

		if err := s.reservations.Confirm(ctx, *current.ReservationID); err != nil {
			if errors.Is(err, ErrReservationReleased) {
				return current.Cancel(ReasonHoldExpired, nil, s.now())
			}
			// ErrReservationNotFound or a transient failure: roll back and let redelivery retry.
			// Confirm is idempotent, so retrying after a partial success is safe.
			return Booking{}, nil, err
		}

		return current.Confirm(paymentID, s.now())
	}
}

// CompensateOnPaymentFailure is the transition the PaymentFailed consumer applies. It cancels the
// held booking (reason payment_failed) and stages BookingCancelled; Availability self-releases by
// consuming that event, so this does NOT call Availability's release endpoint.
func (s *Service) CompensateOnPaymentFailure() Transition {
	return func(current Booking) (Booking, []Emission, error) {
		switch current.Status {
		case StatusCancelled:
			return current, nil, nil
		case StatusConfirmed:
			// A payment failure for a booking already confirmed is contradictory — a confirmed
			// booking's payment succeeded. Refuse to unwind it; accept and let the handler log.
			return current, nil, nil
		}
		return current.Cancel(ReasonPaymentFailed, nil, s.now())
	}
}

// ExpireHold is the transition the ReservationReleased(hold_expired) consumer applies: the TTL
// sweeper freed the window, so the abandoned booking is cancelled. Only a still-held booking is
// affected — a confirmed booking's reservation is never swept, and an already-cancelled one is a
// no-op.
func (s *Service) ExpireHold() Transition {
	return func(current Booking) (Booking, []Emission, error) {
		if current.Status != StatusHeld {
			return current, nil, nil
		}
		return current.Cancel(ReasonHoldExpired, nil, s.now())
	}
}

// validateCreate applies the domain rules that produce a 422 before anything is reserved.
func validateCreate(cmd CreateCommand, product Product, now time.Time) error {
	if !cmd.EndsAt.After(cmd.StartsAt) || !cmd.StartsAt.After(now.Add(-time.Second)) {
		return ErrInvalidTimeWindow
	}
	if cmd.PartySize < 1 {
		return ErrInvalidTimeWindow
	}
	if cmd.PartySize > product.Capacity {
		return ErrPartyExceedsCapacity
	}
	return nil
}
