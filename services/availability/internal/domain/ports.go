package domain

import (
	"context"
	"time"
)

// The ports this domain needs, declared HERE rather than in internal/infra.
//
// The interface belongs to the consumer, not the implementation (service-template's layering
// rule). That is what keeps this package free of pgx and lets the service be unit-tested with
// plain fakes — and CI enforces it with a guard that refuses driver-carrying imports anywhere
// under internal/domain (ADR-0009).
//
// That guard is a plain grep for the shared platform module's import path, so it matches
// comments as readily as imports. Do not spell that path out in this package, even to explain
// the rule: the build fails on the explanation.

// Transition is a state change decided by the domain and applied by the store inside one
// transaction: the store loads and locks the row, hands it here, and persists whatever comes
// back along with the emissions.
//
// Passing a function rather than a finished Reservation is what keeps read-modify-write
// decisions in the domain while the transaction and the row lock stay in infra. A store that
// decided "may this be confirmed?" itself would put a business rule in the driver layer, where
// it is untestable without a database.
type Transition func(current Reservation) (next Reservation, emit []Emission, err error)

// Claim is the Idempotency-Key contract for the reserve path.
//
// The key is claimed BEFORE the reservation is inserted and in the same transaction. That
// ordering is load-bearing, not stylistic — see migrations/0003_idempotency.sql.
type Claim struct {
	// Key is the client-supplied Idempotency-Key header.
	Key string
	// Fingerprint is a digest of the request body, so a replay with a different body can be
	// told apart from a genuine retry.
	Fingerprint string
}

// Store persists reservations. Every method is atomic: a state change and the events it makes
// true commit together or not at all (ADR-0002).
type Store interface {
	// Insert claims the idempotency key, inserts the held reservation with an expiry of
	// holdTTL from the database's clock, and stages ReservationCreated — one transaction, one
	// INSERT on the reserve path.
	//
	// It returns ErrSlotUnavailable when the exclusion constraint fires, ErrIdempotencyKeyReuse
	// when the key was used with a different body, and the ORIGINAL reservation with no new row
	// and no second event when the key is replayed with an identical body.
	//
	// There is deliberately no "is this window free?" query anywhere behind this call. Asking
	// first and inserting second races, and losing that race is the exact bug the whole design
	// exists to prevent (ADR-0003).
	Insert(ctx context.Context, claim Claim, r Reservation, holdTTL time.Duration) (Reservation, error)

	// ByID loads one reservation, or ErrNotFound.
	ByID(ctx context.Context, id string) (Reservation, error)

	// Apply loads and locks the reservation, runs the transition, and persists the result with
	// its emissions in one transaction. The bool reports whether anything actually changed, so
	// a caller can tell an idempotent no-op from real work.
	Apply(ctx context.Context, id string, fn Transition) (Reservation, bool, error)

	// Busy returns the live (held or confirmed) windows overlapping [from, to) for one resource.
	Busy(ctx context.Context, resourceID string, from, to time.Time) ([]Window, error)

	// Now returns the DATABASE's current time.
	//
	// The sweeper needs it because its two halves would otherwise consult two different clocks:
	// ExpiredHolds selects on `expires_at <= now()` in SQL, while the re-check under the row lock
	// runs in Go. expires_at itself is written from the database clock, so if the application
	// clock lags, the scan keeps proposing rows that the re-check keeps declining — a hold that
	// expires late while the sweeper spins on it doing nothing. One clock, asked once per pass.
	Now(ctx context.Context) (time.Time, error)

	// ExpiredHolds returns up to limit ids of held reservations past their expiry.
	//
	// Advisory: each one is re-checked under its row lock by ReleaseIfExpired, because a hold
	// can be confirmed between this scan and the update.
	ExpiredHolds(ctx context.Context, limit int) ([]string, error)
}

// Clock returns the current time. Injected so tests state the instant they mean instead of
// sleeping — the TTL sweep is the behaviour that matters most here and it is all about time.
type Clock func() time.Time

// IDGen mints a reservation id. UUIDv7 in production, so ids sort by creation time.
type IDGen func() (string, error)
