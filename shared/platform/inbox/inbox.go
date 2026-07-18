// Package inbox implements consumer-side deduplication.
//
// The AsyncAPI contract states delivery is at-least-once and consumers MUST be idempotent, keyed
// on the envelope id. This package is how that requirement is met correctly, and "correctly" is
// narrower than it first appears.
//
// The tempting implementation — check a seen-set, run the handler, mark seen — is wrong in a way
// that its own tests will not catch. It has two failure windows:
//
//   - Mark seen BEFORE the handler and crash in between: the event is marked processed but its
//     work never happened. Redelivery is suppressed, so the effect is lost permanently. For a
//     BookingConfirmed that means a customer who paid never gets their confirmation, and nothing
//     anywhere records that it is owed.
//   - Mark seen AFTER the handler, in a separate transaction, and crash in between: the work is
//     committed but not recorded, so redelivery runs it twice. For a payment that is a double
//     charge.
//
// There is no ordering of two separate transactions that closes both windows. The only correct
// answer is ONE transaction containing both the dedupe row and the handler's writes, which is
// why Handler receives a pgx.Tx rather than opening its own.
package inbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/morisempai/wakewake/shared/contracts/events"
)

// uniqueViolation is SQLSTATE 23505 — a duplicate dedupe row, i.e. a redelivery.
const uniqueViolation = "23505"

// Handler processes one event inside the transaction that also records it as processed.
//
// Every database write the handler performs MUST use this tx. Reaching for a pool instead
// silently reopens the failure windows described in the package doc.
type Handler func(ctx context.Context, tx pgx.Tx, e events.Envelope) error

const claimSQL = `
INSERT INTO processed_event (consumer, event_id, event, processed_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (consumer, event_id) DO NOTHING`

// Process runs h for the envelope exactly once per consumer, or reports that it was already
// handled.
//
// The returned bool is false when the event was a duplicate and h was not run. Callers should
// still ACK in that case: the message has been dealt with, just not now.
func Process(ctx context.Context, pool *pgxpool.Pool, consumer string, e events.Envelope, h Handler) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("inbox: begin: %w", err)
	}
	// Rollback is a no-op after a successful commit. Deliberately ignoring its error: a rollback
	// failure on an already-committed or already-failed tx tells us nothing actionable, and
	// letting it mask the real error below would be worse.
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, claimSQL, consumer, e.ID, e.Event)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			// Two deliveries racing. The other one holds the row; this is a duplicate.
			return false, nil
		}
		return false, fmt.Errorf("inbox: claiming %s %s: %w", e.Event, e.ID, err)
	}
	if tag.RowsAffected() == 0 {
		// ON CONFLICT DO NOTHING matched an existing row: already processed.
		return false, nil
	}

	if err := h(ctx, tx, e); err != nil {
		// Rolling back discards the dedupe row too, so a retry will genuinely re-run the
		// handler. That is the point: a failed handler must not leave the event marked done.
		return false, fmt.Errorf("inbox: handling %s %s: %w", e.Event, e.ID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("inbox: commit for %s %s: %w", e.Event, e.ID, err)
	}
	return true, nil
}
