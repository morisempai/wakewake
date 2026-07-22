package domain

import "errors"

// Domain error values. `api` maps each to the status and error code that
// contracts/openapi/payment.yaml declares; `infra` and the Stripe provider translate driver errors
// and provider responses into these, so no pgx type and no HTTP status ever reaches a handler as
// anything but a domain outcome.
var (
	// ErrBookingNotFound is a createPayment for a booking this service has no context for — it
	// never saw the BookingHeld, or the id is wrong. Contract: 404 booking_not_found.
	ErrBookingNotFound = errors.New("payment: no such booking")

	// ErrBookingNotPayable is a booking whose hold has expired (the only payability signal payment
	// holds locally). Contract: 422 booking_not_payable.
	ErrBookingNotPayable = errors.New("payment: the booking is not payable")

	// ErrPaymentAlreadyExists is a second createPayment for a booking that already has a payment in
	// flight, under a different Idempotency-Key. Contract: 409 payment_already_exists.
	ErrPaymentAlreadyExists = errors.New("payment: a payment already exists for this booking")

	// ErrIdempotencyKeyReuse is the same Idempotency-Key with a different request body.
	// Contract: 409 idempotency_key_reuse.
	ErrIdempotencyKeyReuse = errors.New("payment: idempotency key reused with a different request body")

	// ErrProviderError is Stripe rejecting the request or being unreachable — no charge was made.
	// Contract: 502 provider_error. Safe to retry.
	ErrProviderError = errors.New("payment: the payment provider failed")

	// ErrPaymentNotFound is an unknown payment id on getPayment. Contract: 404 payment_not_found.
	ErrPaymentNotFound = errors.New("payment: no such payment")

	// ErrForbidden is a payment that belongs to another customer's booking. Contract: 403 unauthorized.
	ErrForbidden = errors.New("payment: the payment belongs to another customer")
)
