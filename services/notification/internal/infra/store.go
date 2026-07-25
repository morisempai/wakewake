// Package infra holds this service's outward-facing plumbing: the sent_notification store, the
// SMTP mailer, and the DEV recipient resolver. Each implements a narrow port declared by an inner
// layer (domain or events), so the domain stays driver-free and the handler stays testable.
package infra

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/morisempai/wakewake/services/notification/internal/domain"
)

// Store persists the record of confirmation emails into sent_notification.
//
// It is intentionally stateless: every write happens inside the transaction that
// shared/platform/inbox opened for the handler, so the method takes a pgx.Tx rather than reaching
// for a pool of its own. Reaching for a pool here would reopen the crash windows the inbox package
// doc describes — the sent_notification row and the processed_event dedupe row MUST commit
// together.
type Store struct{}

// NewStore constructs the store.
func NewStore() *Store { return &Store{} }

const recordSentSQL = `
INSERT INTO sent_notification
  (event_id, booking_id, customer_id, channel, recipient_redacted, subject, correlation_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (event_id) DO NOTHING`

// RecordSent writes one audit/idempotency row inside tx. It returns true if the row was inserted
// and false if event_id was already present — the second case is a duplicate the caller must NOT
// re-send for. The inbox's processed_event row normally catches a redelivery first, so a false
// here is defense in depth rather than the primary dedupe.
func (s *Store) RecordSent(ctx context.Context, tx pgx.Tx, r domain.SentRecord) (bool, error) {
	tag, err := tx.Exec(ctx, recordSentSQL,
		r.EventID, r.BookingID, r.CustomerID, r.Channel, r.RecipientRedacted, r.Subject, r.CorrelationID)
	if err != nil {
		return false, fmt.Errorf("infra: recording sent notification for event %s: %w", r.EventID, err)
	}
	return tag.RowsAffected() == 1, nil
}
