package infra

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	availability "github.com/morisempai/wakewake/shared/contracts/openapi/availability"
	"github.com/morisempai/wakewake/shared/platform/httpx"

	"github.com/morisempai/wakewake/services/booking/internal/domain"
)

// ReservationsClient calls the Availability HTTP API through the committed generated client
// (AC5). It maps Availability's status codes onto the domain outcomes booking.yaml declares — a
// 409 reservation_overlap becomes ErrSlotUnavailable (409 slot_unavailable), a 422 on confirm
// becomes ErrReservationReleased, and anything transient becomes ErrAvailabilityUnavailable — so a
// downstream status never leaks out of the store as a raw 500.
type ReservationsClient struct {
	client   availability.ClientWithResponsesInterface
	fallback time.Duration
}

var _ domain.Reservations = (*ReservationsClient)(nil)

// NewReservationsClient builds the client. The http.Client propagates the correlation id onto
// every outbound request (AC5) and retries only where it is safe — createReservation carries an
// Idempotency-Key, so a lost response is retried without risking a second hold.
//
// holdTTLFallback is used only if Availability ever omits expires_at on a held reservation, which
// its own schema forbids; it keeps a misbehaving upstream from producing a held booking with no
// hold_expires_at rather than trusting the app clock in the normal path.
func NewReservationsClient(baseURL string, holdTTLFallback time.Duration) (*ReservationsClient, error) {
	client, err := availability.NewClientWithResponses(baseURL,
		availability.WithHTTPClient(httpx.NewClient(httpx.ClientConfig{})))
	if err != nil {
		return nil, fmt.Errorf("booking: building availability client: %w", err)
	}
	return &ReservationsClient{client: client, fallback: holdTTLFallback}, nil
}

// Hold reserves the window (createReservation).
func (c *ReservationsClient) Hold(ctx context.Context, req domain.HoldRequest) (domain.Reservation, error) {
	resourceID, err := uuid.Parse(req.ResourceID)
	if err != nil {
		return domain.Reservation{}, fmt.Errorf("booking: resource id %q is not a uuid: %w", req.ResourceID, err)
	}
	bookingID, err := uuid.Parse(req.BookingID)
	if err != nil {
		return domain.Reservation{}, fmt.Errorf("booking: booking id %q is not a uuid: %w", req.BookingID, err)
	}

	res, err := c.client.CreateReservationWithResponse(ctx,
		&availability.CreateReservationParams{IdempotencyKey: req.IdempotencyKey},
		availability.CreateReservationJSONRequestBody{
			ResourceId: resourceID,
			BookingId:  bookingID,
			StartsAt:   req.StartsAt.UTC(),
			EndsAt:     req.EndsAt.UTC(),
		})
	if err != nil {
		// Transport failure or exhausted retries: no hold was created, safe to retry (503).
		return domain.Reservation{}, fmt.Errorf("%w: %v", domain.ErrAvailabilityUnavailable, err)
	}

	switch res.StatusCode() {
	case http.StatusCreated:
		if res.JSON201 == nil {
			return domain.Reservation{}, fmt.Errorf("%w: 201 with no body", domain.ErrAvailabilityUnavailable)
		}
		return c.toReservation(*res.JSON201), nil

	case http.StatusConflict:
		// 409 is either a lost race for the slot or this key already used with a different body.
		// Only the code distinguishes them, and they map to different booking outcomes.
		if res.JSON409 != nil && string(res.JSON409.Error.Code) == "idempotency_key_reuse" {
			return domain.Reservation{}, domain.ErrIdempotencyKeyReuse
		}
		return domain.Reservation{}, domain.ErrSlotUnavailable

	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout, http.StatusInternalServerError:
		return domain.Reservation{}, fmt.Errorf("%w: availability returned %d", domain.ErrAvailabilityUnavailable, res.StatusCode())

	default:
		// 400/422 here would mean booking and Availability disagree on a request booking already
		// validated — an internal inconsistency, surfaced as a 500 rather than dressed up as a
		// client error the caller cannot act on.
		return domain.Reservation{}, fmt.Errorf("booking: unexpected availability status %d creating reservation", res.StatusCode())
	}
}

// Confirm promotes the reservation (confirmReservation). Idempotent per the contract.
func (c *ReservationsClient) Confirm(ctx context.Context, reservationID string) error {
	id, err := uuid.Parse(reservationID)
	if err != nil {
		return fmt.Errorf("booking: reservation id %q is not a uuid: %w", reservationID, err)
	}

	res, err := c.client.ConfirmReservationWithResponse(ctx, id, &availability.ConfirmReservationParams{})
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrAvailabilityUnavailable, err)
	}

	switch res.StatusCode() {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return domain.ErrReservationNotFound
	case http.StatusUnprocessableEntity:
		// The hold was already released — its TTL swept it before payment landed.
		return domain.ErrReservationReleased
	default:
		return fmt.Errorf("%w: availability returned %d confirming reservation", domain.ErrAvailabilityUnavailable, res.StatusCode())
	}
}

func (c *ReservationsClient) toReservation(r availability.Reservation) domain.Reservation {
	expires := time.Now().Add(c.fallback).UTC()
	if r.ExpiresAt != nil {
		expires = r.ExpiresAt.UTC()
	}
	return domain.Reservation{
		ID:        r.Id.String(),
		BookingID: r.BookingId.String(),
		ExpiresAt: expires,
	}
}
