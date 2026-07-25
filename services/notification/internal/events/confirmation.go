// Package events holds this service's consumer handler. It is deliberately thin: decode the
// payload, resolve the recipient, compose the email, record it, send it.
//
// Delivery is at-least-once and duplicates are guaranteed (ADR-0002), so the handler runs through
// shared/platform/inbox, which executes it inside the same transaction that records the event as
// processed. Unknown payload fields are ignored by construction: events.PayloadOf does not set
// DisallowUnknownFields, so a producer can add an optional field without a coordinated deploy.
package events

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/morisempai/wakewake/shared/contracts/events"

	"github.com/morisempai/wakewake/services/notification/internal/domain"
)

// Recorder writes the sent_notification row inside the handler's transaction. The interface is
// declared here, by the consumer that needs it, and implemented by infra.Store — which keeps this
// package testable with a fake and free of a real database.
type Recorder interface {
	RecordSent(ctx context.Context, tx pgx.Tx, r domain.SentRecord) (bool, error)
}

// ConfirmationHandler sends the booking-confirmation email on BookingConfirmed.
type ConfirmationHandler struct {
	recorder Recorder
	resolver domain.RecipientResolver
	mailer   domain.Mailer
	log      *slog.Logger
}

// NewConfirmationHandler wires the consumer.
func NewConfirmationHandler(recorder Recorder, resolver domain.RecipientResolver, mailer domain.Mailer, log *slog.Logger) *ConfirmationHandler {
	return &ConfirmationHandler{
		recorder: recorder,
		resolver: resolver,
		mailer:   mailer,
		log:      log.With(slog.String("consumer", "BookingConfirmed")),
	}
}

// Handle runs inside the transaction inbox.Process opened.
//
// Ordering matters. The sent_notification row is written FIRST, then the email is sent, and only
// then does Handle return so inbox.Process can commit. Sending inside the transaction means the
// one genuinely unavoidable failure window — a commit that fails after the email was accepted by
// the relay — resolves as a DUPLICATE email on redelivery, not a lost one. That is the right side
// to err on for a payment confirmation: a customer who paid must never be left without one, and a
// second copy is a minor annoyance rather than a broken promise. This is the classic
// database-plus-side-effect dual write; exactly-once across the two is impossible, so the choice
// is made and documented rather than hidden.
func (h *ConfirmationHandler) Handle(ctx context.Context, tx pgx.Tx, e events.Envelope) error {
	if e.Event != events.BookingConfirmed {
		return fmt.Errorf("events: BookingConfirmed handler received %s", e.Event)
	}

	payload, err := events.PayloadOf[events.BookingConfirmedPayload](e)
	if err != nil {
		return fmt.Errorf("events: decoding BookingConfirmed %s: %w", e.ID, err)
	}

	to, err := h.resolver.Resolve(ctx, payload.CustomerID)
	if err != nil {
		return fmt.Errorf("events: resolving recipient for booking %s: %w", payload.BookingID, err)
	}

	msg := domain.ComposeConfirmation(domain.Confirmation{
		BookingID:  payload.BookingID,
		CustomerID: payload.CustomerID,
		StartsAt:   payload.StartsAt,
		EndsAt:     payload.EndsAt,
		TotalMinor: payload.TotalMinor,
		Currency:   payload.Currency,
	}, to)

	inserted, err := h.recorder.RecordSent(ctx, tx, domain.SentRecord{
		EventID:           e.ID,
		BookingID:         payload.BookingID,
		CustomerID:        payload.CustomerID,
		Channel:           domain.ChannelEmail,
		RecipientRedacted: to.Redact(),
		Subject:           msg.Subject,
		CorrelationID:     e.CorrelationID,
	})
	if err != nil {
		return fmt.Errorf("events: recording confirmation for booking %s: %w", payload.BookingID, err)
	}
	if !inserted {
		// A row for this envelope id already exists — the inbox normally suppresses a redelivery
		// before we reach here, so this is the belt to that braces. Do not send a second email.
		h.log.InfoContext(ctx, "confirmation already recorded; not resending",
			slog.String("booking_id", payload.BookingID),
			slog.String("recipient", to.Redact()))
		return nil
	}

	if err := h.mailer.Send(ctx, msg); err != nil {
		return fmt.Errorf("events: sending confirmation for booking %s: %w", payload.BookingID, err)
	}

	// The recipient is logged REDACTED, never raw (AC4). correlation_id and trace_id are attached
	// automatically by the shared logger from the context the consumer populated.
	h.log.InfoContext(ctx, "sent booking confirmation email",
		slog.String("booking_id", payload.BookingID),
		slog.String("recipient", to.Redact()),
		slog.String("channel", domain.ChannelEmail))
	return nil
}
