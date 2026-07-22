// Package infra implements the ports internal/domain declares, using pgx directly and the
// generated typed clients for Availability and Catalog.
//
// No repository abstraction and no ORM (service-template, ADR-0003): the booking store is thin,
// and hiding driver errors would hide the one signal — SQLSTATE 23505 from the idempotency key —
// that tells a replay apart from a fresh request.
package infra

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/morisempai/wakewake/shared/platform/idempotency"
	"github.com/morisempai/wakewake/shared/platform/outbox"
	"github.com/morisempai/wakewake/shared/platform/pgxx"

	"github.com/morisempai/wakewake/services/booking/internal/domain"
)

// aggregateType labels outbox rows so an operator can ask "what happened to booking X" without
// parsing every payload.
const aggregateType = "booking"

// Store is the Postgres implementation of domain.Store.
type Store struct {
	pool *pgxpool.Pool

	// kick wakes the outbox relay after a transaction that staged events commits, cutting publish
	// latency from a poll interval to milliseconds. Purely an optimisation — if it is never called
	// the relay still picks the rows up on its next tick.
	kick func()
}

// NewStore wires the store. kick may be nil, in which case staged events wait for the relay's next
// poll.
func NewStore(pool *pgxpool.Pool, kick func()) *Store {
	if kick == nil {
		kick = func() {}
	}
	return &Store{pool: pool, kick: kick}
}

var _ domain.Store = (*Store)(nil)

// columns is the projection every read uses. The enum columns are cast to text because the Go side
// models them as string constants.
const columns = `
  id, customer_id, product_id, resource_id, reservation_id,
  starts_at, ends_at, party_size, status::text, hold_expires_at,
  total_minor, currency, cancel_reason::text, cancellation_reason,
  created_at, updated_at`

const insertSQL = `
INSERT INTO booking (id, customer_id, product_id, resource_id, reservation_id,
                     starts_at, ends_at, party_size, status, hold_expires_at,
                     total_minor, currency)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'held', $9, $10, $11)
RETURNING` + columns

const selectSQL = `SELECT` + columns + ` FROM booking WHERE id = $1`

// selectForUpdateSQL locks the row for the read-modify-write transitions (confirm, cancel, expire).
// Without FOR UPDATE two concurrent cancels would both read `held`, both emit BookingCancelled, and
// Availability would be asked to release twice.
const selectForUpdateSQL = selectSQL + ` FOR UPDATE`

const updateSQL = `
UPDATE booking
   SET status              = $2::booking_status,
       reservation_id      = $3,
       hold_expires_at     = $4,
       cancel_reason       = $5::booking_cancel_reason,
       cancellation_reason = $6,
       updated_at          = $7
 WHERE id = $1
RETURNING` + columns

// Insert claims the idempotency key, inserts the held booking, and stages BookingHeld — one
// transaction.
//
// The key is claimed FIRST, so two concurrent requests carrying the same key serialise on
// idempotency_key's primary key rather than each inserting a hold. On a replay with an identical
// body the original booking is returned with no new row and no second event.
func (s *Store) Insert(ctx context.Context, claim domain.Claim, b domain.Booking) (domain.Booking, error) {
	var (
		stored   domain.Booking
		replayed bool
	)

	err := pgxx.WithinTx(ctx, s.pool, func(tx pgx.Tx) error {
		existingID, wasReplay, err := idempotency.Claim(ctx, tx, claim.Key, claim.Fingerprint, b.ID)
		if err != nil {
			if errors.Is(err, idempotency.ErrKeyReuse) {
				return domain.ErrIdempotencyKeyReuse
			}
			return fmt.Errorf("booking: claiming idempotency key: %w", err)
		}

		if wasReplay {
			replayed = true
			stored, err = scanOne(tx.QueryRow(ctx, selectSQL, existingID))
			if err != nil {
				return fmt.Errorf("booking: loading replayed booking %s: %w", existingID, err)
			}
			return nil
		}

		stored, err = scanOne(tx.QueryRow(ctx, insertSQL,
			b.ID, b.CustomerID, b.ProductID, b.ResourceID, b.ReservationID,
			b.StartsAt, b.EndsAt, b.PartySize, b.HoldExpiresAt, b.TotalMinor, b.Currency))
		if err != nil {
			return fmt.Errorf("booking: inserting booking: %w", err)
		}

		// Staged in the SAME transaction as the row it describes (ADR-0002, AC1).
		if err := stage(ctx, tx, b.HeldEmission()); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return domain.Booking{}, err
	}

	if !replayed {
		s.kick()
	}
	return stored, nil
}

// ByID loads one booking.
func (s *Store) ByID(ctx context.Context, id string) (domain.Booking, error) {
	b, err := scanOne(s.pool.QueryRow(ctx, selectSQL, id))
	if err != nil {
		return domain.Booking{}, translateRead(id, err)
	}
	return b, nil
}

// FindIdempotent returns the booking id and stored fingerprint a key maps to.
func (s *Store) FindIdempotent(ctx context.Context, key string) (string, string, bool, error) {
	var bookingID, fingerprint string
	err := s.pool.QueryRow(ctx,
		`SELECT resource_id, request_fingerprint FROM idempotency_key WHERE key = $1`, key).
		Scan(&bookingID, &fingerprint)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("booking: looking up idempotency key: %w", err)
	}
	return bookingID, fingerprint, true, nil
}

// Apply runs a domain transition against the locked row and persists the result with its emissions,
// in one transaction. Used by the HTTP cancel path.
func (s *Store) Apply(ctx context.Context, id string, fn domain.Transition) (domain.Booking, bool, error) {
	var (
		result  domain.Booking
		changed bool
	)

	err := pgxx.WithinTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		result, changed, err = applyTx(ctx, tx, id, fn)
		return err
	})
	if err != nil {
		return domain.Booking{}, false, err
	}

	if changed {
		s.kick()
	}
	return result, changed, nil
}

// ApplyTx is Apply against a transaction the caller already owns.
//
// It exists for the event consumers: inbox.Process opens one transaction containing both the dedupe
// row and the handler's writes, and the handler MUST use it. The confirm consumer's transition also
// calls Availability while this row is locked, so the reservation is confirmed and the booking
// promoted inside the same transaction that records the event as processed.
func (s *Store) ApplyTx(ctx context.Context, tx pgx.Tx, id string, fn domain.Transition) (domain.Booking, bool, error) {
	return applyTx(ctx, tx, id, fn)
}

// Kick wakes the outbox relay. The consumers call it after inbox.Process commits, since the events
// staged inside that transaction are not this store's to kick for.
func (s *Store) Kick() { s.kick() }

func applyTx(ctx context.Context, tx pgx.Tx, id string, fn domain.Transition) (domain.Booking, bool, error) {
	current, err := scanOne(tx.QueryRow(ctx, selectForUpdateSQL, id))
	if err != nil {
		return domain.Booking{}, false, translateRead(id, err)
	}

	next, emissions, err := fn(current)
	if err != nil {
		return domain.Booking{}, false, err
	}

	if !stateChanged(current, next) && len(emissions) == 0 {
		// An idempotent no-op: confirming an already-confirmed booking, or a redelivery that finds
		// nothing left to do. Writing anyway would move updated_at and make the audit trail claim
		// something happened.
		return current, false, nil
	}

	updated, err := scanOne(tx.QueryRow(ctx, updateSQL,
		next.ID, string(next.Status), next.ReservationID, next.HoldExpiresAt,
		cancelReasonArg(next), next.CancellationReason, next.UpdatedAt))
	if err != nil {
		return domain.Booking{}, false, fmt.Errorf("booking: updating booking %s: %w", id, err)
	}

	for _, e := range emissions {
		if err := stage(ctx, tx, e); err != nil {
			return domain.Booking{}, false, err
		}
	}
	return updated, true, nil
}

// List returns a page of the caller's bookings, newest first, using keyset pagination over
// (created_at, id).
func (s *Store) List(ctx context.Context, q domain.ListQuery) (domain.ListPage, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}

	var (
		conds = []string{"customer_id = $1"}
		args  = []any{q.CustomerID}
	)
	if q.Status != nil {
		args = append(args, string(*q.Status))
		conds = append(conds, fmt.Sprintf("status = $%d::booking_status", len(args)))
	}
	if q.Cursor != "" {
		createdAt, id, err := decodeCursor(q.Cursor)
		if err != nil {
			// A malformed cursor is treated as no cursor rather than an error: cursors are opaque
			// and a client should never hand-craft one, so the safe reading of a bad value is
			// "start from the top" instead of failing a list request.
			createdAt, id = time.Time{}, ""
		}
		if id != "" {
			args = append(args, createdAt, id)
			conds = append(conds, fmt.Sprintf("(created_at, id) < ($%d, $%d)", len(args)-1, len(args)))
		}
	}

	// One extra row tells us whether a next page exists without a second COUNT query.
	args = append(args, limit+1)
	sql := `SELECT` + columns + ` FROM booking WHERE ` + strings.Join(conds, " AND ") +
		fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, len(args))

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return domain.ListPage{}, fmt.Errorf("booking: listing bookings: %w", err)
	}
	defer rows.Close()

	bookings := make([]domain.Booking, 0, limit)
	for rows.Next() {
		b, err := scanRow(rows)
		if err != nil {
			return domain.ListPage{}, fmt.Errorf("booking: scanning booking row: %w", err)
		}
		bookings = append(bookings, b)
	}
	if err := rows.Err(); err != nil {
		return domain.ListPage{}, fmt.Errorf("booking: reading bookings: %w", err)
	}

	page := domain.ListPage{Bookings: bookings}
	if len(bookings) > limit {
		last := bookings[limit-1]
		page.Bookings = bookings[:limit]
		cursor := encodeCursor(last.CreatedAt, last.ID)
		page.NextCursor = &cursor
	}
	return page, nil
}

// stage writes one domain emission to the outbox inside the caller's transaction.
func stage(ctx context.Context, tx pgx.Tx, e domain.Emission) error {
	if _, err := outbox.Enqueue(ctx, tx, outbox.Record{
		Event:         e.Event,
		AggregateType: aggregateType,
		AggregateID:   e.AggregateID,
		Payload:       e.Payload,
	}); err != nil {
		return fmt.Errorf("booking: staging %s: %w", e.Event, err)
	}
	return nil
}

// stateChanged reports whether a transition actually altered the persisted state.
func stateChanged(current, next domain.Booking) bool {
	if current.Status != next.Status {
		return true
	}
	if !sameTime(current.HoldExpiresAt, next.HoldExpiresAt) {
		return true
	}
	if !sameStr(current.ReservationID, next.ReservationID) {
		return true
	}
	if !sameReason(current.CancelReason, next.CancelReason) {
		return true
	}
	return !sameStr(current.CancellationReason, next.CancellationReason)
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

func sameStr(a, b *string) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

func sameReason(a, b *domain.CancelReason) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// cancelReasonArg converts the optional reason into something pgx can bind to a nullable enum
// column. A typed nil pointer would be sent as the zero string and rejected by the enum.
func cancelReasonArg(b domain.Booking) *string {
	if b.CancelReason == nil {
		return nil
	}
	s := string(*b.CancelReason)
	return &s
}

// scanner is what pgx.Row and pgx.Rows share.
type scanner interface {
	Scan(dest ...any) error
}

func scanOne(row pgx.Row) (domain.Booking, error) { return scanRow(row) }

func scanRow(row scanner) (domain.Booking, error) {
	var (
		b            domain.Booking
		status       string
		cancelReason *string
	)
	if err := row.Scan(
		&b.ID, &b.CustomerID, &b.ProductID, &b.ResourceID, &b.ReservationID,
		&b.StartsAt, &b.EndsAt, &b.PartySize, &status, &b.HoldExpiresAt,
		&b.TotalMinor, &b.Currency, &cancelReason, &b.CancellationReason,
		&b.CreatedAt, &b.UpdatedAt,
	); err != nil {
		return domain.Booking{}, err
	}

	b.Status = domain.Status(status)
	if cancelReason != nil {
		r := domain.CancelReason(*cancelReason)
		b.CancelReason = &r
	}
	return b, nil
}

func translateRead(id string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	return fmt.Errorf("booking: loading booking %s: %w", id, err)
}

// encodeCursor / decodeCursor make an opaque keyset cursor from (created_at, id). Opaque so a
// client cannot depend on its shape, and keyset rather than offset so paging stays O(1) as the
// table grows.
func encodeCursor(createdAt time.Time, id string) string {
	raw := createdAt.UTC().Format(time.RFC3339Nano) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, "", fmt.Errorf("booking: malformed cursor")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", err
	}
	return createdAt, parts[1], nil
}
