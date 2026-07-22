// Package events holds this service's consumer handlers. They are deliberately thin: parse the
// payload, decide nothing, call one domain transition.
//
// Delivery is at-least-once and duplicates are guaranteed, so every handler here runs through
// shared/platform/inbox, which executes it inside the same transaction that records the event as
// processed. That is what makes "idempotent, keyed on the envelope id" true rather than claimed.
package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/morisempai/wakewake/shared/contracts/events"

	"github.com/morisempai/wakewake/services/availability/internal/domain"
	"github.com/morisempai/wakewake/services/availability/internal/infra"
)

// BookingCancelledHandler releases the reservation backing a cancelled booking — the
// compensation leg of the ADR-0003 saga.
type BookingCancelledHandler struct {
	store *infra.Store
	svc   *domain.Service
	log   *slog.Logger
}

// NewBookingCancelledHandler wires the consumer.
func NewBookingCancelledHandler(store *infra.Store, svc *domain.Service, log *slog.Logger) *BookingCancelledHandler {
	return &BookingCancelledHandler{store: store, svc: svc, log: log.With(slog.String("consumer", "BookingCancelled"))}
}

// Handle runs inside the transaction inbox.Process opened.
//
// Every write goes through the supplied tx. Reaching for the pool instead would put the release
// in one transaction and the dedupe row in another, so a crash between them either loses the
// compensation — leaving a slot held for a booking that no longer exists — or replays it.
//
// Unknown payload fields are ignored by construction: events.PayloadOf does not set
// DisallowUnknownFields, because the contract requires consumers to tolerate additive producer
// changes without a coordinated deploy.
func (h *BookingCancelledHandler) Handle(ctx context.Context, tx pgx.Tx, e events.Envelope) error {
	if e.Event != events.BookingCancelled {
		// The queue is bound to one routing key, so this is a topology bug rather than bad data.
		return fmt.Errorf("events: BookingCancelled handler received %s", e.Event)
	}

	payload, err := events.PayloadOf[events.BookingCancelledPayload](e)
	if err != nil {
		return fmt.Errorf("events: decoding BookingCancelled %s: %w", e.ID, err)
	}

	// The contract types reservation_id as `oneOf: [Uuid, null]`, and null means the hold was
	// already released. Nothing is owed, so the event is accepted and dropped — the dedupe row
	// still commits, so a redelivery will not reconsider it.
	if payload.ReservationID == nil || *payload.ReservationID == "" {
		h.log.DebugContext(ctx, "cancelled booking carried no reservation; nothing to release",
			slog.String("booking_id", payload.BookingID))
		return nil
	}
	reservationID := *payload.ReservationID

	reservation, changed, err := h.store.ApplyTx(ctx, tx, reservationID, h.svc.CompensateCancelledBooking())
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Nothing to compensate. Retrying cannot conjure the row, and dead-lettering it would
			// bury an operator in messages about reservations that are already gone. The
			// compensation's goal — that no window is held for this booking — is satisfied.
			h.log.WarnContext(ctx, "cancelled booking names a reservation that does not exist",
				slog.String("booking_id", payload.BookingID),
				slog.String("reservation_id", reservationID))
			return nil
		}
		return fmt.Errorf("events: releasing %s for cancelled booking %s: %w",
			reservationID, payload.BookingID, err)
	}

	// A reservation held for a different booking must never be released on this booking's
	// behalf. That would free a live slot and leave a paying customer's window bookable by
	// somebody else. It can only happen if Booking sent the wrong id, so it is returned as an
	// error: the message dead-letters and a human sees it, rather than the damage being silent.
	//
	// Checked after the transition, which is safe because returning an error rolls the whole
	// transaction back — the release never commits.
	if reservation.BookingID != payload.BookingID {
		return fmt.Errorf(
			"events: BookingCancelled for booking %s names reservation %s, which belongs to booking %s",
			payload.BookingID, reservationID, reservation.BookingID)
	}

	if changed {
		h.log.InfoContext(ctx, "released reservation for a cancelled booking",
			slog.String("booking_id", payload.BookingID),
			slog.String("reservation_id", reservationID),
			slog.String("cancel_reason", string(payload.Reason)))
	} else {
		h.log.DebugContext(ctx, "reservation was already released",
			slog.String("reservation_id", reservationID))
	}
	return nil
}
