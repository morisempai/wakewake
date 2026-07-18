// Package infra implements the ports internal/domain declares, using pgx directly.
//
// No repository abstraction and no ORM (service-template, ADR-0003): the no-double-booking
// invariant IS a Postgres exclusion constraint, and the only signal that it fired is SQLSTATE
// 23P01 coming back from an INSERT. Any layer that normalises driver errors hides the exact
// moment the architecture's central claim does its job.
package infra

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/morisempai/wakewake/shared/platform/idempotency"
	"github.com/morisempai/wakewake/shared/platform/outbox"
	"github.com/morisempai/wakewake/shared/platform/pgerr"
	"github.com/morisempai/wakewake/shared/platform/pgxx"

	"github.com/morisempai/wakewake/services/availability/internal/domain"
)

// aggregateType labels outbox rows so an operator can ask "what happened to reservation X"
// without parsing every payload.
const aggregateType = "reservation"

// Store is the Postgres implementation of domain.Store.
type Store struct {
	pool *pgxpool.Pool

	// kick wakes the outbox relay after a transaction that staged events commits, cutting
	// publish latency from a poll interval to milliseconds. Purely an optimisation — if it is
	// never called the relay still picks the rows up on its next tick.
	kick func()
}

// NewStore wires the store. kick may be nil, in which case staged events wait for the relay's
// next poll.
func NewStore(pool *pgxpool.Pool, kick func()) *Store {
	if kick == nil {
		kick = func() {}
	}
	return &Store{pool: pool, kick: kick}
}

var _ domain.Store = (*Store)(nil)

// columns is the projection every read uses. The enum columns are cast to text because the Go
// side models them as string constants; registering the enum OIDs with pgx would buy nothing
// and would make the mapping invisible.
const columns = `
  id, resource_id, booking_id,
  lower(during) AS starts_at, upper(during) AS ends_at,
  status::text, expires_at, released_reason::text,
  created_at, updated_at`

// insertSQL is the ENTIRE reserve path: one statement, no preceding read.
//
// The absence of a "is this window free?" SELECT is the point (ADR-0003, AC1). Overlaps are
// rejected by reservation_no_overlap, which surfaces as SQLSTATE 23P01 and is translated below
// into domain.ErrSlotUnavailable. Checking first would race, and the race is the bug.
//
// expires_at is computed from the DATABASE clock, not Go's. The sweeper's due-check is
// `expires_at <= now()` against that same clock; deriving one from the application clock and the
// other from the database's means holds expire early or late by however far the two have
// drifted, and drift is invisible until a customer loses a slot they had paid for.
const insertSQL = `
INSERT INTO reservation (id, resource_id, booking_id, during, status, expires_at)
VALUES ($1, $2, $3, tstzrange($4, $5, '[)'), 'held', now() + make_interval(secs => $6::double precision))
RETURNING` + columns

const selectSQL = `SELECT` + columns + ` FROM reservation WHERE id = $1`

// selectForUpdateSQL locks the row for the read-modify-write transitions (confirm, release,
// sweep). Without FOR UPDATE two concurrent releases would both read `held`, both emit
// ReservationReleased, and Booking would cancel the booking twice.
const selectForUpdateSQL = selectSQL + ` FOR UPDATE`

const updateSQL = `
UPDATE reservation
   SET status          = $2::reservation_status,
       expires_at      = $3,
       released_reason = $4::release_reason,
       updated_at      = $5
 WHERE id = $1
RETURNING` + columns

// busySQL lists live windows overlapping the query range.
//
// `status <> 'released'` is not merely a filter: it is the same predicate as the partial
// exclusion constraint, and ADR-0011 records that the planner will only use the partial gist
// index for queries that repeat it. Dropping it would still be correct and would quietly become
// a sequential scan.
const busySQL = `
SELECT lower(during), upper(during)
  FROM reservation
 WHERE resource_id = $1
   AND status <> 'released'
   AND during && tstzrange($2, $3, '[)')
 ORDER BY lower(during)`

// expiredHoldsSQL is the sweeper's scan, served by reservation_due_for_sweep_idx.
//
// It only proposes candidates; each is re-checked under its row lock, because a hold can be
// confirmed between this statement and the update.
const expiredHoldsSQL = `
SELECT id
  FROM reservation
 WHERE status = 'held' AND expires_at <= now()
 ORDER BY expires_at
 LIMIT $1`

// Insert claims the idempotency key, inserts the hold, and stages ReservationCreated — one
// transaction.
//
// The ORDER inside is load-bearing and is documented at length in migrations/0003_idempotency.sql:
// the key is claimed FIRST, so two concurrent requests carrying the same key serialise on
// idempotency_key's primary key rather than colliding on the exclusion constraint. Insert the
// reservation first and a client retrying its own request is told the slot is taken — by itself.
func (s *Store) Insert(ctx context.Context, claim domain.Claim, r domain.Reservation, holdTTL time.Duration) (domain.Reservation, error) {
	var (
		stored   domain.Reservation
		replayed bool
	)

	err := pgxx.WithinTx(ctx, s.pool, func(tx pgx.Tx) error {
		existingID, wasReplay, err := idempotency.Claim(ctx, tx, claim.Key, claim.Fingerprint, r.ID)
		if err != nil {
			if errors.Is(err, idempotency.ErrKeyReuse) {
				return domain.ErrIdempotencyKeyReuse
			}
			return fmt.Errorf("availability: claiming idempotency key: %w", err)
		}

		if wasReplay {
			// Same key, same body: return what the first call produced. No second row, and no
			// second ReservationCreated — the fact happened once (AC3).
			replayed = true
			stored, err = scanOne(tx.QueryRow(ctx, selectSQL, existingID))
			if err != nil {
				return fmt.Errorf("availability: loading replayed reservation %s: %w", existingID, err)
			}
			return nil
		}

		stored, err = scanOne(tx.QueryRow(ctx, insertSQL,
			r.ID, r.ResourceID, r.BookingID, r.StartsAt, r.EndsAt, holdTTL.Seconds()))
		if err != nil {
			return translateInsert(err)
		}

		// Staged in the SAME transaction as the row it describes (ADR-0002, AC4). The payload is
		// built from the RETURNING values, not from the request, because expires_at is required
		// by the AsyncAPI schema and only exists once the database has computed it.
		if err := stage(ctx, tx, stored.CreatedEmission()); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return domain.Reservation{}, err
	}

	if !replayed {
		s.kick()
	}
	return stored, nil
}

// ByID loads one reservation.
func (s *Store) ByID(ctx context.Context, id string) (domain.Reservation, error) {
	r, err := scanOne(s.pool.QueryRow(ctx, selectSQL, id))
	if err != nil {
		return domain.Reservation{}, translateRead(id, err)
	}
	return r, nil
}

// Apply runs a domain transition against the locked row and persists the result with its
// emissions, in one transaction.
func (s *Store) Apply(ctx context.Context, id string, fn domain.Transition) (domain.Reservation, bool, error) {
	var (
		result  domain.Reservation
		changed bool
	)

	err := pgxx.WithinTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		result, changed, err = applyTx(ctx, tx, id, fn)
		return err
	})
	if err != nil {
		return domain.Reservation{}, false, err
	}

	if changed {
		s.kick()
	}
	return result, changed, nil
}

// ApplyTx is Apply against a transaction the caller already owns.
//
// It exists for the event consumer: inbox.Process opens one transaction containing both the
// dedupe row and the handler's writes, and the handler MUST use it. Reaching for the pool
// instead reopens the crash windows the inbox package doc describes — the work committing
// without being recorded, or being recorded without happening.
func (s *Store) ApplyTx(ctx context.Context, tx pgx.Tx, id string, fn domain.Transition) (domain.Reservation, bool, error) {
	return applyTx(ctx, tx, id, fn)
}

// Kick wakes the outbox relay. The consumer calls it after inbox.Process commits, since the
// events staged inside that transaction are not this store's to kick for.
func (s *Store) Kick() { s.kick() }

func applyTx(ctx context.Context, tx pgx.Tx, id string, fn domain.Transition) (domain.Reservation, bool, error) {
	current, err := scanOne(tx.QueryRow(ctx, selectForUpdateSQL, id))
	if err != nil {
		return domain.Reservation{}, false, translateRead(id, err)
	}

	next, emissions, err := fn(current)
	if err != nil {
		return domain.Reservation{}, false, err
	}

	if !stateChanged(current, next) && len(emissions) == 0 {
		// An idempotent no-op: confirming an already-confirmed reservation, or sweeping one that
		// was confirmed between the scan and this lock. Writing anyway would move updated_at and
		// make the audit trail claim something happened.
		return current, false, nil
	}

	updated, err := scanOne(tx.QueryRow(ctx, updateSQL,
		next.ID, string(next.Status), next.ExpiresAt, releasedReasonArg(next), next.UpdatedAt))
	if err != nil {
		return domain.Reservation{}, false, fmt.Errorf("availability: updating reservation %s: %w", id, err)
	}

	for _, e := range emissions {
		if err := stage(ctx, tx, e); err != nil {
			return domain.Reservation{}, false, err
		}
	}
	return updated, true, nil
}

// Busy returns the live windows overlapping [from, to).
func (s *Store) Busy(ctx context.Context, resourceID string, from, to time.Time) ([]domain.Window, error) {
	rows, err := s.pool.Query(ctx, busySQL, resourceID, from, to)
	if err != nil {
		return nil, fmt.Errorf("availability: querying busy windows for %s: %w", resourceID, err)
	}
	defer rows.Close()

	// Non-nil so an empty result marshals as `[]` rather than `null`. The OpenAPI schema types
	// `busy` as an array and makes it required; `null` would fail validation.
	windows := make([]domain.Window, 0)
	for rows.Next() {
		var w domain.Window
		if err := rows.Scan(&w.StartsAt, &w.EndsAt); err != nil {
			return nil, fmt.Errorf("availability: scanning busy window: %w", err)
		}
		windows = append(windows, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("availability: reading busy windows: %w", err)
	}
	return windows, nil
}

// ExpiredHolds returns candidate ids for the sweeper.
func (s *Store) ExpiredHolds(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx, expiredHoldsSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("availability: scanning expired holds: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("availability: scanning expired hold id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("availability: reading expired holds: %w", err)
	}
	return ids, nil
}

// stage writes one domain emission to the outbox inside the caller's transaction.
func stage(ctx context.Context, tx pgx.Tx, e domain.Emission) error {
	if _, err := outbox.Enqueue(ctx, tx, outbox.Record{
		Event:         e.Event,
		AggregateType: aggregateType,
		AggregateID:   e.AggregateID,
		Payload:       e.Payload,
	}); err != nil {
		return fmt.Errorf("availability: staging %s: %w", e.Event, err)
	}
	return nil
}

// stateChanged reports whether a transition actually altered the persisted state.
//
// All three columns are compared rather than just status: a future transition that only moved an
// expiry would otherwise be silently discarded as a no-op, and the bug would present as a hold
// that never expires.
func stateChanged(current, next domain.Reservation) bool {
	if current.Status != next.Status {
		return true
	}
	if !sameTime(current.ExpiresAt, next.ExpiresAt) {
		return true
	}
	return !sameReason(current.ReleasedReason, next.ReleasedReason)
}

func sameTime(a, b *time.Time) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.Equal(*b)
	}
}

func sameReason(a, b *domain.ReleaseReason) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// releasedReasonArg converts the optional reason into something pgx can bind to a nullable enum
// column. A typed nil pointer would be sent as the zero string and rejected by the enum.
func releasedReasonArg(r domain.Reservation) *string {
	if r.ReleasedReason == nil {
		return nil
	}
	s := string(*r.ReleasedReason)
	return &s
}

// scanOne reads the standard projection into a domain.Reservation.
func scanOne(row pgx.Row) (domain.Reservation, error) {
	var (
		r      domain.Reservation
		status string
		reason *string
	)
	if err := row.Scan(
		&r.ID, &r.ResourceID, &r.BookingID,
		&r.StartsAt, &r.EndsAt,
		&status, &r.ExpiresAt, &reason,
		&r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return domain.Reservation{}, err
	}

	r.Status = domain.Status(status)
	if reason != nil {
		rr := domain.ReleaseReason(*reason)
		r.ReleasedReason = &rr
	}
	return r, nil
}

// translateInsert maps driver errors on the reserve path to domain errors.
//
// The 23P01 mapping is a contract obligation, not an implementation detail (service-template):
// it is what makes `409 reservation_overlap` reachable at all. Matching on SQLSTATE rather than
// message text is deliberate — messages change with the server locale, and a mismatch would
// silently degrade the invariant into a 500 that clients retry into the same wall.
func translateInsert(err error) error {
	if pgerr.Is(err, pgerr.ExclusionViolation) {
		return domain.ErrSlotUnavailable
	}
	if pgerr.Is(err, pgerr.CheckViolation) {
		// Several CHECKs share SQLSTATE 23514 and mean different things. Only the empty-window
		// one is a client error; the others (held-iff-expiry, released-iff-reason) are internal
		// bugs and must not be dressed up as a 422 the caller can act on.
		switch pgerr.ConstraintName(err) {
		case "reservation_window_not_empty", "reservation_window_bounded":
			return domain.ErrInvalidTimeWindow
		}
	}
	return fmt.Errorf("availability: inserting reservation: %w", err)
}

func translateRead(id string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	return fmt.Errorf("availability: loading reservation %s: %w", id, err)
}
