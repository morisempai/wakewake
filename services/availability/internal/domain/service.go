package domain

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ReserveCommand is a validated-at-the-edge reserve request.
//
// The Idempotency-Key travels with it rather than being handled in the HTTP layer because the
// key must be claimed in the SAME transaction as the insert. Claiming it in a handler, before
// the store's transaction opens, reintroduces exactly the window it exists to close.
type ReserveCommand struct {
	ResourceID string
	BookingID  string

	// StartsAt is inclusive, EndsAt exclusive. UTC.
	StartsAt time.Time
	EndsAt   time.Time

	Idempotency Claim
}

// Service is the reservation use-case layer: it decides, the store persists.
//
// It holds no state beyond its dependencies, so it is safe for concurrent use by every HTTP
// handler, the consumer, and the sweeper at once.
type Service struct {
	store   Store
	now     Clock
	newID   IDGen
	holdTTL time.Duration
}

// NewService wires the service. The clock and id generator are parameters rather than package
// calls so tests are deterministic (testing-standards).
func NewService(store Store, now Clock, newID IDGen, holdTTL time.Duration) *Service {
	return &Service{store: store, now: now, newID: newID, holdTTL: holdTTL}
}

// Reserve holds a window, or reports why it could not.
//
// The whole path is: validate, mint an id, one call to the store. There is no availability check
// — deliberately. Any "is this free?" query before the insert would be the SELECT-then-INSERT
// race that ADR-0003 exists to eliminate, and it would pass its own tests while failing under
// the concurrency it claims to handle. The database decides, and a lost race arrives as
// ErrSlotUnavailable.
func (s *Service) Reserve(ctx context.Context, cmd ReserveCommand) (Reservation, error) {
	id, err := s.newID()
	if err != nil {
		return Reservation{}, fmt.Errorf("availability: generating reservation id: %w", err)
	}

	r, err := NewReservation(id, cmd.ResourceID, cmd.BookingID, cmd.StartsAt, cmd.EndsAt)
	if err != nil {
		return Reservation{}, err
	}

	return s.store.Insert(ctx, cmd.Idempotency, r, s.holdTTL)
}

// Get returns one reservation, or ErrNotFound.
func (s *Service) Get(ctx context.Context, id string) (Reservation, error) {
	return s.store.ByID(ctx, id)
}

// Confirm promotes a held reservation on the payment-success path. Idempotent per the contract.
func (s *Service) Confirm(ctx context.Context, id string) (Reservation, error) {
	r, _, err := s.store.Apply(ctx, id, func(current Reservation) (Reservation, []Emission, error) {
		return current.Confirm(s.now())
	})
	return r, err
}

// Release frees a window — the compensation path, and the sweeper's effect. Idempotent per the
// contract, and a second release emits nothing.
func (s *Service) Release(ctx context.Context, id string, reason ReleaseReason) (Reservation, error) {
	// Checked before the transaction opens: an unknown reason can never succeed, and holding a
	// row lock while establishing that wastes it.
	if !reason.Valid() {
		return Reservation{}, ErrInvalidReleaseReason
	}

	r, _, err := s.store.Apply(ctx, id, func(current Reservation) (Reservation, []Emission, error) {
		return current.Release(reason, s.now())
	})
	return r, err
}

// Busy returns the live windows overlapping [from, to) for a resource.
//
// Advisory by contract: "A free window here can be taken before you reserve it — the reserve
// call is the only authority."
func (s *Service) Busy(ctx context.Context, resourceID string, from, to time.Time) ([]Window, error) {
	if !to.After(from) {
		return nil, ErrInvalidQueryWindow
	}
	return s.store.Busy(ctx, resourceID, from.UTC(), to.UTC())
}

// Sweep releases holds whose TTL has run out, returning how many it actually released.
//
// Two passes rather than one bulk UPDATE: the scan finds candidates, and each one is then
// re-checked under its own row lock by ReleaseIfExpired. A single `UPDATE ... WHERE status =
// 'held' AND expires_at <= now()` would be cheaper but would decide in SQL what the domain is
// supposed to decide, and it would need the ReservationReleased payload for every affected row
// assembled from the same statement.
//
// A failure on one reservation is recorded and the sweep continues. Aborting on the first one
// would leave every hold behind it locked until a human noticed — inventory leaking away
// quietly, which is the failure mode ADR-0011 exists to prevent, arriving by another route.
func (s *Service) Sweep(ctx context.Context, limit int) (int, error) {
	// The database's clock, not this process's. expires_at is written by the database and the
	// scan below filters on it in SQL, so re-checking due-ness against time.Now() here would be
	// comparing two clocks that drift apart — see Store.Now.
	asOf, err := s.store.Now(ctx)
	if err != nil {
		return 0, fmt.Errorf("availability: reading the database clock: %w", err)
	}

	ids, err := s.store.ExpiredHolds(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("availability: scanning for expired holds: %w", err)
	}

	released := 0
	var failures []error

	for _, id := range ids {
		// Stop promptly on shutdown; the remaining holds are still due on the next pass.
		if ctx.Err() != nil {
			failures = append(failures, ctx.Err())
			break
		}

		_, changed, err := s.store.Apply(ctx, id, func(current Reservation) (Reservation, []Emission, error) {
			return current.ReleaseIfExpired(asOf)
		})
		if err != nil {
			// ErrNotFound is not a failure: rows are only deleted by retention, and a
			// concurrent sweeper replica taking the same candidate is normal.
			if !errors.Is(err, ErrNotFound) {
				failures = append(failures, fmt.Errorf("availability: sweeping %s: %w", id, err))
			}
			continue
		}
		if changed {
			released++
		}
	}

	return released, errors.Join(failures...)
}

// CompensateCancelledBooking is the transition the BookingCancelled consumer applies.
//
// It lives here rather than in internal/events because *which* release reason a cancelled
// booking implies is a domain decision, and the events layer is meant to validate and delegate,
// not to choose. Exposed as a Transition so the consumer can run it inside the transaction
// inbox.Process already opened — the handler's writes and the dedupe row must commit together.
func (s *Service) CompensateCancelledBooking() Transition {
	return func(current Reservation) (Reservation, []Emission, error) {
		return current.Release(ReasonBookingCancelled, s.now())
	}
}
