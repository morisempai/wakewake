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

// ReservationReleasedHandler cancels a booking whose hold expired. Availability's TTL sweeper frees
// the window and publishes ReservationReleased; booking reacts to exactly one reason — hold_expired
// — and cancels the abandoned booking. The other reasons (payment_failed, booking_cancelled) are
// consequences of booking's own actions, so reacting to them would loop.
type ReservationReleasedHandler struct {
	store *infra.Store
	svc   *domain.Service
	log   *slog.Logger
}

// NewReservationReleasedHandler wires the consumer.
func NewReservationReleasedHandler(store *infra.Store, svc *domain.Service, log *slog.Logger) *ReservationReleasedHandler {
	return &ReservationReleasedHandler{store: store, svc: svc, log: log.With(slog.String("consumer", "ReservationReleased"))}
}

// Handle runs inside the transaction inbox.Process opened.
func (h *ReservationReleasedHandler) Handle(ctx context.Context, tx pgx.Tx, e events.Envelope) error {
	if e.Event != events.ReservationReleased {
		return fmt.Errorf("events: ReservationReleased handler received %s", e.Event)
	}

	payload, err := events.PayloadOf[events.ReservationReleasedPayload](e)
	if err != nil {
		return fmt.Errorf("events: decoding ReservationReleased %s: %w", e.ID, err)
	}

	// hold_expired is the only reason booking must react to (booking-events.yaml). A release booking
	// itself caused is accepted and dropped — the dedupe row still commits, so a redelivery will
	// not reconsider it.
	if payload.Reason != events.ReleaseHoldExpired {
		h.log.DebugContext(ctx, "ignoring a release booking did not need to react to",
			slog.String("booking_id", payload.BookingID), slog.String("reason", string(payload.Reason)))
		return nil
	}

	booking, changed, err := h.store.ApplyTx(ctx, tx, payload.BookingID, h.svc.ExpireHold())
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			h.log.WarnContext(ctx, "TTL release for an unknown booking",
				slog.String("booking_id", payload.BookingID))
			return nil
		}
		return fmt.Errorf("events: cancelling expired booking %s: %w", payload.BookingID, err)
	}

	if changed {
		h.log.InfoContext(ctx, "cancelled booking whose hold expired",
			slog.String("booking_id", payload.BookingID),
			slog.String("reservation_id", payload.ReservationID))
	} else {
		h.log.DebugContext(ctx, "expired-hold release for a booking that was no longer held",
			slog.String("booking_id", payload.BookingID), slog.String("status", string(booking.Status)))
	}
	return nil
}
