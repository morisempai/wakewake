package domain

import (
	"context"
	"time"
)

// The ports this domain needs, declared HERE rather than in internal/infra.
//
// The interface belongs to the consumer, not the implementation (service-template's layering
// rule). That is what keeps this package free of database drivers and HTTP clients and lets the
// saga be unit-tested with plain fakes — and CI enforces it with a guard that refuses
// driver-carrying imports anywhere under internal/domain (ADR-0009).

// Transition is a state change decided by the domain and applied by the store inside one
// transaction: the store loads and locks the row, hands it here, and persists whatever comes back
// along with the emissions.
//
// Passing a function rather than a finished Booking keeps read-modify-write decisions in the
// domain while the transaction and the row lock stay in infra. A consumer handler may close a
// cross-service call (an Availability confirm) over this function, so the call happens while the
// row is locked and inside the same transaction that records the event as processed.
type Transition func(current Booking) (next Booking, emit []Emission, err error)

// Claim is the Idempotency-Key contract for the create-hold path.
//
// The key is claimed BEFORE the booking is inserted and in the same transaction, so two concurrent
// requests carrying the same key serialise on the idempotency table's primary key rather than each
// creating a hold on the same slot.
type Claim struct {
	Key         string
	Fingerprint string
}

// ListQuery scopes and pages listBookings. CustomerID always comes from the JWT claims; the
// endpoint never accepts a customer id parameter (booking.yaml).
type ListQuery struct {
	CustomerID string
	Status     *Status
	Limit      int
	Cursor     string
}

// ListPage is one page of bookings plus the opaque cursor for the next, or nil on the last page.
type ListPage struct {
	Bookings   []Booking
	NextCursor *string
}

// Store persists bookings. Every mutating method is atomic: a state change and the events it makes
// true commit together or not at all (ADR-0002).
type Store interface {
	// Insert claims the idempotency key, inserts the held booking, and stages BookingHeld — one
	// transaction. On a replay of the same key with an identical body it returns the ORIGINAL
	// booking with no new row and no second event; on a different body it returns
	// ErrIdempotencyKeyReuse.
	Insert(ctx context.Context, claim Claim, b Booking) (Booking, error)

	// ByID loads one booking, or ErrNotFound.
	ByID(ctx context.Context, id string) (Booking, error)

	// List returns a page of the caller's bookings, newest first.
	List(ctx context.Context, q ListQuery) (ListPage, error)

	// Apply loads and locks the booking, runs the transition, and persists the result with its
	// emissions in one transaction. The bool reports whether anything actually changed, so a
	// caller can tell an idempotent no-op from real work. Used by the HTTP cancel path.
	Apply(ctx context.Context, id string, fn Transition) (Booking, bool, error)

	// FindIdempotent returns the booking id and stored request fingerprint a key already maps to,
	// for the create-hold path to short-circuit a replay before it calls Availability again.
	FindIdempotent(ctx context.Context, key string) (bookingID, fingerprint string, found bool, err error)
}

// Reservations is the Availability HTTP API, as this domain needs it. infra implements it with the
// generated typed client and maps Availability's status codes onto the domain errors above.
type Reservations interface {
	// Hold reserves the window (createReservation). It returns ErrSlotUnavailable on a
	// 409 reservation_overlap and ErrAvailabilityUnavailable when Availability is unreachable or
	// returns a transient error, in which case no hold was created and the caller may retry.
	Hold(ctx context.Context, req HoldRequest) (Reservation, error)

	// Confirm promotes the reservation (confirmReservation). It is idempotent; it returns
	// ErrReservationReleased on a 422 (the hold was already swept) and ErrReservationNotFound on a
	// 404.
	Confirm(ctx context.Context, reservationID string) error
}

// HoldRequest is a createReservation call. The IdempotencyKey is the booking's own key, so a
// retried create-hold does not reserve twice at Availability.
type HoldRequest struct {
	ResourceID     string
	BookingID      string
	StartsAt       time.Time
	EndsAt         time.Time
	IdempotencyKey string
}

// Catalog is the Catalog HTTP API, as this domain needs it: resolve a product to the resource
// Availability reserves against, its capacity, and its price. infra implements it with the
// generated typed client.
type Catalog interface {
	// Product returns the product, or ErrProductNotFound for an unknown id.
	Product(ctx context.Context, productID string) (Product, error)
}

// Clock returns the current time. Injected so tests state the instant they mean instead of
// sleeping — hold expiry and "window in the past" are the behaviours that matter most here.
type Clock func() time.Time

// IDGen mints a booking id. UUIDv7 in production, so ids sort by creation time.
type IDGen func() (string, error)
