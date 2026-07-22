package domain

import "errors"

// Domain error values. `api` maps each to the status and error code that
// contracts/openapi/availability.yaml declares; `infra` translates driver errors into these so
// that no pgx type ever reaches a handler.
var (
	// ErrInvalidTimeWindow is ends_at <= starts_at. Contract: 422 invalid_time_window.
	ErrInvalidTimeWindow = errors.New("availability: ends_at must be strictly after starts_at")

	// ErrMissingIdentifier is an absent resource_id or booking_id. Contract: 400 validation_failed.
	ErrMissingIdentifier = errors.New("availability: resource_id and booking_id are required")

	// ErrSlotUnavailable is the no-double-booking invariant firing — SQLSTATE 23P01 from the
	// exclusion constraint, translated by infra. Contract: 409 reservation_overlap.
	//
	// It is the expected, designed outcome of a lost race, not a fault.
	ErrSlotUnavailable = errors.New("availability: the window overlaps a live reservation")

	// ErrNotFound is an unknown reservation id. Contract: 404 reservation_not_found.
	ErrNotFound = errors.New("availability: no such reservation")

	// ErrAlreadyReleased is a confirm against a released reservation. Released is terminal, so
	// this can never become valid by retrying. Contract: 422 reservation_already_released.
	ErrAlreadyReleased = errors.New("availability: the reservation was already released")

	// ErrIdempotencyKeyReuse is the same Idempotency-Key with a different request body.
	// Contract: 409 idempotency_key_reuse.
	ErrIdempotencyKeyReuse = errors.New("availability: idempotency key reused with a different request body")

	// ErrInvalidQueryWindow is a slot query whose `to` is not after `from`. Contract: 400.
	ErrInvalidQueryWindow = errors.New("availability: `to` must be after `from`")

	// ErrInvalidReleaseReason is a release reason outside the contract enum. Contract: 400.
	ErrInvalidReleaseReason = errors.New("availability: release reason is not one of the contract values")
)
