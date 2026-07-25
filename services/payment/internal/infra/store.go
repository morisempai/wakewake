// Package infra implements the ports internal/domain declares, using pgx directly.
//
// No repository abstraction and no ORM (service-template, ADR-0003): the payment store is thin, and
// hiding driver errors would hide the two signals — SQLSTATE 23505 from the idempotency key and
// from the booking_id unique constraint — that tell a replay and a second payment apart from a
// fresh request.
package infra

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/morisempai/wakewake/shared/platform/idempotency"
	"github.com/morisempai/wakewake/shared/platform/outbox"
	"github.com/morisempai/wakewake/shared/platform/pgerr"
	"github.com/morisempai/wakewake/shared/platform/pgxx"

	"github.com/morisempai/wakewake/services/payment/internal/domain"
)

// aggregateType labels outbox rows so an operator can ask "what happened to payment X" without
// parsing every payload.
const aggregateType = "payment"

// bookingIDUnique is the constraint that guards one-payment-per-booking. Postgres names a UNIQUE
// column constraint <table>_<column>_key; matching on the name rather than the class distinguishes
// this 23505 from an idempotency-key replay, which means something entirely different.
const bookingIDUnique = "payment_booking_id_unique"

// Store is the Postgres implementation of domain.Store.
type Store struct {
	pool *pgxpool.Pool

	// kick wakes the outbox relay after a transaction that staged events commits, cutting publish
	// latency from a poll interval to milliseconds. Purely an optimisation.
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

// columns is the projection every payment read uses. status is cast to text because the Go side
// models it as string constants.
const columns = `
  id, booking_id, status::text, amount_minor, currency, provider,
  provider_payment_id, failure_code, created_at, updated_at`

const (
	selectByIDSQL = `SELECT` + columns + ` FROM payment WHERE id = $1`

	selectByBookingSQL = `SELECT` + columns + ` FROM payment WHERE booking_id = $1`

	// Locks the row for the webhook's read-modify-write. Without FOR UPDATE two concurrent
	// deliveries of different Stripe events for one intent could both read pending and both emit.
	selectByProviderForUpdateSQL = `SELECT` + columns + ` FROM payment WHERE provider_payment_id = $1 FOR UPDATE`

	insertPaymentSQL = `
INSERT INTO payment (id, booking_id, status, amount_minor, currency, provider, provider_payment_id, failure_code)
VALUES ($1, $2, $3::payment_status, $4, $5, $6, $7, $8)
RETURNING` + columns

	updatePaymentSQL = `
UPDATE payment
   SET status       = $2::payment_status,
       failure_code = $3,
       updated_at   = $4
 WHERE id = $1
RETURNING` + columns

	insertStripeEventSQL = `
INSERT INTO stripe_event (event_id, type)
VALUES ($1, $2)
ON CONFLICT (event_id) DO NOTHING`
)

// BookingContext returns payment's projection of a held booking.
func (s *Store) BookingContext(ctx context.Context, bookingID string) (domain.BookingContext, error) {
	var b domain.BookingContext
	err := s.pool.QueryRow(ctx,
		`SELECT booking_id, customer_id, amount_minor, currency, hold_expires_at
		   FROM booking_context WHERE booking_id = $1`, bookingID).
		Scan(&b.BookingID, &b.CustomerID, &b.AmountMinor, &b.Currency, &b.HoldExpiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.BookingContext{}, domain.ErrBookingNotFound
		}
		return domain.BookingContext{}, fmt.Errorf("payment: loading booking context %s: %w", bookingID, err)
	}
	return b, nil
}

// PaymentByBooking returns the payment for a booking if one exists.
func (s *Store) PaymentByBooking(ctx context.Context, bookingID string) (domain.Payment, bool, error) {
	p, err := scanPayment(s.pool.QueryRow(ctx, selectByBookingSQL, bookingID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Payment{}, false, nil
		}
		return domain.Payment{}, false, fmt.Errorf("payment: loading payment for booking %s: %w", bookingID, err)
	}
	return p, true, nil
}

// ByID loads one payment.
func (s *Store) ByID(ctx context.Context, id string) (domain.Payment, error) {
	p, err := scanPayment(s.pool.QueryRow(ctx, selectByIDSQL, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Payment{}, domain.ErrPaymentNotFound
		}
		return domain.Payment{}, fmt.Errorf("payment: loading payment %s: %w", id, err)
	}
	return p, nil
}

// FindIdempotent returns the payment id and stored fingerprint a key maps to.
func (s *Store) FindIdempotent(ctx context.Context, key string) (string, string, bool, error) {
	var paymentID, fingerprint string
	err := s.pool.QueryRow(ctx,
		`SELECT resource_id, request_fingerprint FROM idempotency_key WHERE key = $1`, key).
		Scan(&paymentID, &fingerprint)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("payment: looking up idempotency key: %w", err)
	}
	return paymentID, fingerprint, true, nil
}

// InsertPayment claims the idempotency key and inserts the pending payment — one transaction.
//
// The key is claimed FIRST, so two concurrent requests carrying the same key serialise on
// idempotency_key's primary key. On a replay the original payment is returned with no new row; a
// booking that already has a payment surfaces the unique-constraint violation as
// ErrPaymentAlreadyExists.
func (s *Store) InsertPayment(ctx context.Context, claim domain.Claim, p domain.Payment) (domain.Payment, error) {
	var stored domain.Payment

	err := pgxx.WithinTx(ctx, s.pool, func(tx pgx.Tx) error {
		existingID, wasReplay, err := idempotency.Claim(ctx, tx, claim.Key, claim.Fingerprint, p.ID)
		if err != nil {
			if errors.Is(err, idempotency.ErrKeyReuse) {
				return domain.ErrIdempotencyKeyReuse
			}
			return fmt.Errorf("payment: claiming idempotency key: %w", err)
		}

		if wasReplay {
			stored, err = scanPayment(tx.QueryRow(ctx, selectByIDSQL, existingID))
			if err != nil {
				return fmt.Errorf("payment: loading replayed payment %s: %w", existingID, err)
			}
			return nil
		}

		stored, err = scanPayment(tx.QueryRow(ctx, insertPaymentSQL,
			p.ID, p.BookingID, string(p.Status), p.AmountMinor, p.Currency, p.Provider,
			p.ProviderPaymentID, failureCodeArg(p)))
		if err != nil {
			if pgerr.Is(err, pgerr.UniqueViolation) && pgerr.ConstraintName(err) == bookingIDUnique {
				return domain.ErrPaymentAlreadyExists
			}
			return fmt.Errorf("payment: inserting payment: %w", err)
		}
		return nil
	})
	if err != nil {
		return domain.Payment{}, err
	}
	return stored, nil
}

// RecordOutcome applies a verified Stripe outcome: it dedupes on the Stripe event id, locks the
// payment the intent maps to, runs the transition, and stages PaymentSucceeded/PaymentFailed — all
// in ONE transaction (AC4). The webhook signature is verified by the caller before this runs.
//
// It returns Found=false when no payment matches the intent (the caller acks 200 and ignores),
// Duplicate=true when the Stripe event id was already processed (nothing changes, nothing emits),
// and Changed=true when the status actually moved.
func (s *Store) RecordOutcome(ctx context.Context, stripeEventID, stripeType, providerPaymentID string, fn domain.Transition) (domain.OutcomeResult, error) {
	var result domain.OutcomeResult

	err := pgxx.WithinTx(ctx, s.pool, func(tx pgx.Tx) error {
		current, err := scanPayment(tx.QueryRow(ctx, selectByProviderForUpdateSQL, providerPaymentID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// No payment for this intent. Nothing to record — a retry will keep not finding it,
				// which is harmless. The caller acks so Stripe stops retrying.
				return nil
			}
			return fmt.Errorf("payment: locking payment for intent %s: %w", providerPaymentID, err)
		}
		result.Found = true
		result.Payment = current

		tag, err := tx.Exec(ctx, insertStripeEventSQL, stripeEventID, stripeType)
		if err != nil {
			return fmt.Errorf("payment: recording stripe event %s: %w", stripeEventID, err)
		}
		if tag.RowsAffected() == 0 {
			// The event id was already processed: a duplicate delivery. Leave everything untouched.
			result.Duplicate = true
			return nil
		}

		next, emissions, err := fn(current)
		if err != nil {
			return err
		}
		if next.Status == current.Status && len(emissions) == 0 {
			// The payment was already terminal (a late, distinct event for a settled intent). The
			// stripe_event row is still recorded so it is not reconsidered, but no state moves.
			return nil
		}

		updated, err := scanPayment(tx.QueryRow(ctx, updatePaymentSQL,
			next.ID, string(next.Status), failureCodeArg(next), next.UpdatedAt))
		if err != nil {
			return fmt.Errorf("payment: updating payment %s: %w", next.ID, err)
		}
		result.Payment = updated
		result.Changed = true

		// Staged in the SAME transaction as the status change (ADR-0002, AC4).
		for _, e := range emissions {
			if err := stage(ctx, tx, e); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return domain.OutcomeResult{}, err
	}

	if result.Changed {
		s.kick()
	}
	return result, nil
}

// UpsertBookingContext records payment's projection of a BookingHeld. It takes the transaction the
// consumer's inbox opened, so the context write and the dedupe row commit together. A duplicate
// BookingHeld (at-least-once delivery) is an ON CONFLICT DO NOTHING no-op.
func (s *Store) UpsertBookingContext(ctx context.Context, tx pgx.Tx, b domain.BookingContext) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO booking_context (booking_id, customer_id, amount_minor, currency, hold_expires_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (booking_id) DO NOTHING`,
		b.BookingID, b.CustomerID, b.AmountMinor, b.Currency, b.HoldExpiresAt)
	if err != nil {
		return fmt.Errorf("payment: upserting booking context %s: %w", b.BookingID, err)
	}
	return nil
}

// Kick wakes the outbox relay. The consumer calls it after inbox.Process commits, since events
// staged inside that transaction are not this store's to kick for. (BookingHeld stages nothing, so
// today this is only here for symmetry with booking.)
func (s *Store) Kick() { s.kick() }

// stage writes one domain emission to the outbox inside the caller's transaction.
func stage(ctx context.Context, tx pgx.Tx, e domain.Emission) error {
	if _, err := outbox.Enqueue(ctx, tx, outbox.Record{
		Event:         e.Event,
		AggregateType: aggregateType,
		AggregateID:   e.AggregateID,
		Payload:       e.Payload,
	}); err != nil {
		return fmt.Errorf("payment: staging %s: %w", e.Event, err)
	}
	return nil
}

// failureCodeArg converts the optional failure code into something pgx can bind to a nullable text
// column. A typed nil *string is sent as NULL, which the payment_failed_iff_failure_code check
// requires for a non-failed row.
func failureCodeArg(p domain.Payment) *string {
	return p.FailureCode
}

// scanner is what pgx.Row and pgx.Rows share.
type scanner interface {
	Scan(dest ...any) error
}

func scanPayment(row scanner) (domain.Payment, error) {
	var (
		p      domain.Payment
		status string
	)
	if err := row.Scan(
		&p.ID, &p.BookingID, &status, &p.AmountMinor, &p.Currency, &p.Provider,
		&p.ProviderPaymentID, &p.FailureCode, &p.CreatedAt, &p.UpdatedAt,
	); err != nil {
		return domain.Payment{}, err
	}
	p.Status = domain.Status(status)
	return p, nil
}
