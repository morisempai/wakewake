// Package events holds this service's consumer handlers. They are deliberately thin: parse the
// payload, decide nothing that the domain should decide, run one transition.
//
// Delivery is at-least-once and duplicates are guaranteed, so every handler here runs through
// shared/platform/inbox, which executes it inside the same transaction that records the event as
// processed. That is what makes "idempotent, keyed on the envelope id" true rather than claimed.
// Unknown payload fields are ignored by construction: events.PayloadOf does not set
// DisallowUnknownFields, so a producer can add an optional field without a coordinated deploy.
package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/morisempai/wakewake/shared/contracts/events"

	"github.com/morisempai/wakewake/services/booking/internal/domain"
	"github.com/morisempai/wakewake/services/booking/internal/infra"
)

// PaymentSucceededHandler confirms the booking on the payment-success path and stages
// BookingConfirmed. The confirm is synchronous: the transition calls Availability's confirm HTTP
// endpoint while the booking row is locked, inside the dedupe transaction, so the reservation and
// the booking move together.
type PaymentSucceededHandler struct {
	store *infra.Store
	svc   *domain.Service
	log   *slog.Logger
}

// NewPaymentSucceededHandler wires the consumer.
func NewPaymentSucceededHandler(store *infra.Store, svc *domain.Service, log *slog.Logger) *PaymentSucceededHandler {
	return &PaymentSucceededHandler{store: store, svc: svc, log: log.With(slog.String("consumer", "PaymentSucceeded"))}
}

// Handle runs inside the transaction inbox.Process opened.
func (h *PaymentSucceededHandler) Handle(ctx context.Context, tx pgx.Tx, e events.Envelope) error {
	if e.Event != events.PaymentSucceeded {
		return fmt.Errorf("events: PaymentSucceeded handler received %s", e.Event)
	}

	payload, err := events.PayloadOf[events.PaymentSucceededPayload](e)
	if err != nil {
		return fmt.Errorf("events: decoding PaymentSucceeded %s: %w", e.ID, err)
	}

	booking, changed, err := h.store.ApplyTx(ctx, tx, payload.BookingID, h.svc.ConfirmOnPayment(ctx, payload.PaymentID))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// A payment for a booking that does not exist: retrying cannot conjure it, and
			// dead-lettering would bury an operator in noise. Accept so the dedupe row commits.
			h.log.WarnContext(ctx, "payment succeeded for an unknown booking",
				slog.String("booking_id", payload.BookingID), slog.String("payment_id", payload.PaymentID))
			return nil
		}
		return fmt.Errorf("events: confirming booking %s: %w", payload.BookingID, err)
	}

	switch {
	case changed && booking.Status == domain.StatusConfirmed:
		h.log.InfoContext(ctx, "confirmed booking after payment",
			slog.String("booking_id", payload.BookingID), slog.String("payment_id", payload.PaymentID))
	case changed && booking.Status == domain.StatusCancelled:
		// The reservation had already been released; the booking could not be confirmed and was
		// cancelled instead. A paid-for booking that lost its slot needs reconciliation.
		h.log.ErrorContext(ctx, "payment succeeded but the reservation was already released; booking cancelled",
			slog.String("booking_id", payload.BookingID), slog.String("payment_id", payload.PaymentID))
	default:
		h.log.WarnContext(ctx, "payment succeeded but the booking was not in a confirmable state",
			slog.String("booking_id", payload.BookingID), slog.String("status", string(booking.Status)))
	}
	return nil
}

// PaymentFailedHandler cancels the held booking — the compensation leg of the saga — and stages
// BookingCancelled. Availability self-releases by consuming that event, so this does NOT call
// Availability's release endpoint.
type PaymentFailedHandler struct {
	store *infra.Store
	svc   *domain.Service
	log   *slog.Logger
}

// NewPaymentFailedHandler wires the consumer.
func NewPaymentFailedHandler(store *infra.Store, svc *domain.Service, log *slog.Logger) *PaymentFailedHandler {
	return &PaymentFailedHandler{store: store, svc: svc, log: log.With(slog.String("consumer", "PaymentFailed"))}
}

// Handle runs inside the transaction inbox.Process opened.
func (h *PaymentFailedHandler) Handle(ctx context.Context, tx pgx.Tx, e events.Envelope) error {
	if e.Event != events.PaymentFailed {
		return fmt.Errorf("events: PaymentFailed handler received %s", e.Event)
	}

	payload, err := events.PayloadOf[events.PaymentFailedPayload](e)
	if err != nil {
		return fmt.Errorf("events: decoding PaymentFailed %s: %w", e.ID, err)
	}

	booking, changed, err := h.store.ApplyTx(ctx, tx, payload.BookingID, h.svc.CompensateOnPaymentFailure())
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			h.log.WarnContext(ctx, "payment failed for an unknown booking",
				slog.String("booking_id", payload.BookingID))
			return nil
		}
		return fmt.Errorf("events: cancelling booking %s after payment failure: %w", payload.BookingID, err)
	}

	if changed {
		h.log.InfoContext(ctx, "cancelled booking after payment failure",
			slog.String("booking_id", payload.BookingID), slog.String("failure_code", payload.FailureCode))
	} else {
		h.log.DebugContext(ctx, "booking was already terminal on payment failure",
			slog.String("booking_id", payload.BookingID), slog.String("status", string(booking.Status)))
	}
	return nil
}
