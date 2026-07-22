package domain

import "errors"

// Domain error values. `api` maps each to the status and error code that
// contracts/openapi/booking.yaml declares; `infra` translates driver errors and the responses of
// the Availability and Catalog HTTP APIs into these, so that no pgx type and no HTTP status code
// ever reaches a handler as anything but a domain outcome.
var (
	// ErrInvalidTimeWindow is ends_at <= starts_at, or a window that starts in the past.
	// Contract: 422 invalid_time_window.
	ErrInvalidTimeWindow = errors.New("booking: the requested time window is invalid")

	// ErrPartyExceedsCapacity is a party_size larger than the product's capacity.
	// Contract: 422 party_exceeds_capacity.
	ErrPartyExceedsCapacity = errors.New("booking: party size exceeds the product's capacity")

	// ErrMissingIdentifier is an absent customer_id or product_id. Contract: 400 validation_failed.
	ErrMissingIdentifier = errors.New("booking: customer_id and product_id are required")

	// ErrProductNotFound is an unknown product_id, reported by Catalog. Contract: 404 product_not_found.
	ErrProductNotFound = errors.New("booking: no such product")

	// ErrSlotUnavailable is Availability rejecting the hold because the window overlaps a live
	// reservation (its exclusion constraint fired, surfacing as 409 reservation_overlap).
	// Contract: 409 slot_unavailable. It is the expected outcome of a lost race, not a fault.
	ErrSlotUnavailable = errors.New("booking: the slot is no longer available")

	// ErrAvailabilityUnavailable is Availability being unreachable or returning a transient error,
	// so no hold was created. Contract: 503 availability_unavailable. Safe to retry.
	ErrAvailabilityUnavailable = errors.New("booking: availability is unavailable")

	// ErrIdempotencyKeyReuse is the same Idempotency-Key with a different request body.
	// Contract: 409 idempotency_key_reuse.
	ErrIdempotencyKeyReuse = errors.New("booking: idempotency key reused with a different request body")

	// ErrNotFound is an unknown booking id. Contract: 404 booking_not_found.
	ErrNotFound = errors.New("booking: no such booking")

	// ErrForbidden is a booking that belongs to a different customer. Contract: 403 unauthorized.
	ErrForbidden = errors.New("booking: the booking belongs to another customer")

	// ErrReservationReleased is Availability rejecting a confirm because the reservation was
	// already released (422 reservation_already_released) — the hold was swept before payment
	// landed. Consumed internally; there is no HTTP endpoint that surfaces it.
	ErrReservationReleased = errors.New("booking: the reservation was already released")

	// ErrReservationNotFound is Availability returning 404 for a reservation booking believes it
	// created. An internal inconsistency; consumed internally.
	ErrReservationNotFound = errors.New("booking: availability does not know this reservation")
)
