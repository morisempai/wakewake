// Package domain holds the payment lifecycle: what a payment is, which state changes are legal,
// and which facts each one makes true.
//
// It imports nothing but the standard library and shared/contracts/events (pure types). No pgx,
// no amqp, no HTTP client — CI enforces this (ADR-0009). The orchestration lives in service.go
// over narrow interfaces (ports.go); everything that needs a driver, a socket, or Stripe is
// implemented in internal/infra and internal/stripe. The reason is not purity for its own sake:
// the state machine and the money-handling rules are the part that must be reasoned about and
// tested without a database or a live Stripe.
//
// PCI runs through the whole package: a Payment carries only Stripe references, a status, and an
// amount. There is no field anywhere here for a card number, a CVV, or an expiry, by construction.
//
// Time is always a parameter, never read from the clock, so "the hold expired" and "charged just
// now" are testable without waiting.
package domain

import (
	"time"

	"github.com/morisempai/wakewake/shared/contracts/events"
)

// Status is the payment lifecycle state, matching the PaymentStatus enum in
// contracts/openapi/payment.yaml.
type Status string

const (
	// StatusPending — PaymentIntent created, outcome unknown. The create response returns this;
	// it says nothing about whether the charge will succeed.
	StatusPending Status = "pending"
	// StatusSucceeded — Stripe confirmed the charge via a verified webhook. Terminal.
	StatusSucceeded Status = "succeeded"
	// StatusFailed — Stripe declined or the intent was cancelled, via a verified webhook. Terminal.
	StatusFailed Status = "failed"
)

// providerStripe is the only provider in this slice, matching the Payment.provider const in the
// contract. Split-bill and other providers are deferred.
const providerStripe = "stripe"

// Emission is one event a transition made true, to be staged in the outbox inside the same
// transaction as the state change (ADR-0002).
//
// Domain returns these rather than publishing: publishing needs a broker, and a domain that can
// reach a broker can publish a fact whose transaction later rolled back.
type Emission struct {
	Event       string
	AggregateID string
	Payload     any
}

// BookingContext is payment's own projection of a BookingHeld: the authoritative amount and
// currency to charge and the hold's expiry. createPayment reads the price from here, never from the
// request (payment.yaml).
type BookingContext struct {
	BookingID     string
	CustomerID    string
	AmountMinor   int64
	Currency      string
	HoldExpiresAt time.Time
}

// WebhookOutcome is the decision distilled from a VERIFIED Stripe webhook: which PaymentIntent it
// concerns and whether the charge succeeded or failed. The webhook handler builds it only after the
// signature has been verified — an unverified body never reaches here.
type WebhookOutcome struct {
	StripeEventID     string
	StripeType        string
	ProviderPaymentID string
	Succeeded         bool
	FailureCode       string
}

// Payment is one pending, succeeded, or failed payment.
//
// Transitions are value methods returning a NEW Payment, so a rejected transition cannot leave a
// half-mutated entity behind for a caller that ignored the error.
type Payment struct {
	ID          string
	BookingID   string
	Status      Status
	AmountMinor int64
	Currency    string
	Provider    string

	// ProviderPaymentID is the Stripe PaymentIntent id (pi_...). A reference, never an instrument.
	ProviderPaymentID string

	// FailureCode is Stripe's decline code, non-nil exactly when Status is StatusFailed (the schema
	// enforces the biconditional).
	FailureCode *string

	// CorrelationID is the correlation id captured at createPayment time (the id the gateway
	// forwarded). It is persisted so the Stripe webhook — an external callback that carries no
	// correlation header — can re-hydrate the original id onto the outcome events it stages rather
	// than the fresh one the webhook's own request minted (issue #23). Empty for rows created before
	// the feature existed; the webhook falls back to mint-on-empty in that case.
	CorrelationID string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewPayment builds a pending payment for a held booking, around the Stripe PaymentIntent just
// created for it. The amount and currency come from the booking context, never from the caller.
func NewPayment(id, bookingID string, amountMinor int64, currency, providerPaymentID string, now time.Time) Payment {
	return Payment{
		ID:                id,
		BookingID:         bookingID,
		Status:            StatusPending,
		AmountMinor:       amountMinor,
		Currency:          currency,
		Provider:          providerStripe,
		ProviderPaymentID: providerPaymentID,
		CreatedAt:         now.UTC(),
		UpdatedAt:         now.UTC(),
	}
}

// MarkSucceeded promotes a pending payment on a verified success webhook and states
// PaymentSucceeded. Booking consumes that fact to confirm the booking.
//
// A success for an already-succeeded payment is a no-op with no second emission — delivery is
// at-least-once and PaymentSucceeded must be stated once. A success for a payment already marked
// failed is contradictory; it is accepted as a no-op (so the webhook's dedupe row still commits)
// and the handler logs it for reconciliation rather than silently flipping a terminal state.
func (p Payment) MarkSucceeded(now time.Time) (Payment, []Emission, error) {
	switch p.Status {
	case StatusSucceeded, StatusFailed:
		return p, nil, nil
	}

	next := p
	next.Status = StatusSucceeded
	next.FailureCode = nil
	next.UpdatedAt = now.UTC()

	return next, []Emission{{
		Event:       events.PaymentSucceeded,
		AggregateID: p.ID,
		Payload: events.PaymentSucceededPayload{
			PaymentID:         p.ID,
			BookingID:         p.BookingID,
			AmountMinor:       p.AmountMinor,
			Currency:          p.Currency,
			ProviderPaymentID: p.ProviderPaymentID,
		},
	}}, nil
}

// MarkFailed moves a pending payment to failed on a verified failure webhook and states
// PaymentFailed — the fact booking consumes to compensate (cancel the held booking). Idempotent on
// an already-failed payment; a failure for an already-succeeded payment is contradictory and
// accepted as a no-op for the handler to log.
func (p Payment) MarkFailed(failureCode string, now time.Time) (Payment, []Emission, error) {
	switch p.Status {
	case StatusFailed, StatusSucceeded:
		return p, nil, nil
	}
	if failureCode == "" {
		// The contract requires a non-empty failure_code on PaymentFailed. Stripe usually supplies
		// one; a webhook that omits it still describes a real failure, so a stable fallback beats
		// staging an invalid event that fails validation after the transaction has committed.
		failureCode = "payment_failed"
	}

	next := p
	next.Status = StatusFailed
	code := failureCode
	next.FailureCode = &code
	next.UpdatedAt = now.UTC()

	return next, []Emission{{
		Event:       events.PaymentFailed,
		AggregateID: p.ID,
		Payload: events.PaymentFailedPayload{
			PaymentID:   p.ID,
			BookingID:   p.BookingID,
			AmountMinor: p.AmountMinor,
			Currency:    p.Currency,
			FailureCode: failureCode,
		},
	}}, nil
}
