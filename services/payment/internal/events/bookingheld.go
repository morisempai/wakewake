// Package events holds this service's consumer handlers. They are deliberately thin: parse the
// payload, decide nothing that the domain should decide, write one row.
//
// Delivery is at-least-once and duplicates are guaranteed, so every handler here runs through
// shared/platform/inbox, which executes it inside the same transaction that records the event as
// processed. That is what makes "idempotent, keyed on the envelope id" true rather than claimed.
// Unknown payload fields are ignored by construction: events.PayloadOf does not set
// DisallowUnknownFields, so a producer can add an optional field without a coordinated deploy.
package events

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/morisempai/wakewake/shared/contracts/events"

	"github.com/morisempai/wakewake/services/payment/internal/domain"
	"github.com/morisempai/wakewake/services/payment/internal/infra"
)

// BookingHeldHandler records payment's context for a newly held booking: the authoritative amount
// and currency to charge and the hold's expiry. createPayment later reads the price from here
// rather than from the request (payment.yaml). This is the "prepare a payment context for the hold"
// step from the saga (booking-events.yaml).
type BookingHeldHandler struct {
	store *infra.Store
	log   *slog.Logger
}

// NewBookingHeldHandler wires the consumer.
func NewBookingHeldHandler(store *infra.Store, log *slog.Logger) *BookingHeldHandler {
	return &BookingHeldHandler{store: store, log: log.With(slog.String("consumer", "BookingHeld"))}
}

// Handle runs inside the transaction inbox.Process opened, so the context row and the dedupe row
// commit together.
func (h *BookingHeldHandler) Handle(ctx context.Context, tx pgx.Tx, e events.Envelope) error {
	if e.Event != events.BookingHeld {
		return fmt.Errorf("events: BookingHeld handler received %s", e.Event)
	}

	payload, err := events.PayloadOf[events.BookingHeldPayload](e)
	if err != nil {
		return fmt.Errorf("events: decoding BookingHeld %s: %w", e.ID, err)
	}

	if err := h.store.UpsertBookingContext(ctx, tx, domain.BookingContext{
		BookingID:     payload.BookingID,
		CustomerID:    payload.CustomerID,
		AmountMinor:   payload.TotalMinor,
		Currency:      payload.Currency,
		HoldExpiresAt: payload.HoldExpiresAt,
	}); err != nil {
		return fmt.Errorf("events: recording booking context %s: %w", payload.BookingID, err)
	}

	h.log.InfoContext(ctx, "prepared payment context for a held booking",
		slog.String("booking_id", payload.BookingID),
		slog.Int64("amount_minor", payload.TotalMinor),
		slog.String("currency", payload.Currency))
	return nil
}
