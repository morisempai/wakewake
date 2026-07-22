package domain

import (
	"context"
	"time"
)

// The ports this domain needs, declared HERE rather than in internal/infra.
//
// The interface belongs to the consumer, not the implementation (service-template's layering
// rule). That is what keeps this package free of database drivers and HTTP clients and lets the
// charge flow be unit-tested with plain fakes — and CI enforces it with a guard that refuses
// driver-carrying imports anywhere under internal/domain (ADR-0009).

// Transition is a state change decided by the domain and applied by the store inside one
// transaction: the store loads and locks the payment row, hands it here, and persists whatever
// comes back along with the emissions. The webhook's outcome application is expressed this way so
// the payment row moves and PaymentSucceeded/PaymentFailed is staged in the SAME transaction.
type Transition func(current Payment) (next Payment, emit []Emission, err error)

// Claim is the Idempotency-Key contract for the createPayment path.
//
// The key is claimed in the same transaction as the payment insert, so two concurrent requests
// carrying the same key serialise on the idempotency table's primary key rather than each opening
// a charge.
type Claim struct {
	Key         string
	Fingerprint string
}

// OutcomeResult reports what a webhook did, so the handler can return 200 to Stripe in every
// non-error case while logging precisely what happened.
type OutcomeResult struct {
	// Found is true when a payment matched the webhook's PaymentIntent id.
	Found bool
	// Duplicate is true when this Stripe event id was already processed — nothing changed, nothing
	// was emitted.
	Duplicate bool
	// Changed is true when the payment status actually moved (and an event was staged).
	Changed bool
	Payment Payment
}

// Store persists payments and their context. Every mutating method is atomic: a state change and
// the events it makes true commit together or not at all (ADR-0002).
type Store interface {
	// BookingContext returns payment's projection of a held booking, or ErrBookingNotFound.
	BookingContext(ctx context.Context, bookingID string) (BookingContext, error)

	// PaymentByBooking returns the payment for a booking if one exists.
	PaymentByBooking(ctx context.Context, bookingID string) (Payment, bool, error)

	// ByID loads one payment, or ErrPaymentNotFound.
	ByID(ctx context.Context, id string) (Payment, error)

	// FindIdempotent returns the payment id and stored fingerprint a key maps to, for the
	// createPayment path to short-circuit a replay before it calls Stripe again.
	FindIdempotent(ctx context.Context, key string) (paymentID, fingerprint string, found bool, err error)

	// InsertPayment claims the idempotency key and inserts the pending payment — one transaction.
	// On a replay of the same key it returns the ORIGINAL payment; on a different body it returns
	// ErrIdempotencyKeyReuse; on a booking that already has a payment it returns
	// ErrPaymentAlreadyExists.
	InsertPayment(ctx context.Context, claim Claim, p Payment) (Payment, error)
}

// Provider is Stripe, as this domain needs it: create a PaymentIntent for an amount and return the
// client secret. internal/stripe implements it over HTTP with an idempotency key and a stubbable
// transport, so tests never reach the real Stripe.
type Provider interface {
	// CreateIntent creates (or, on an idempotency-key replay, returns) a PaymentIntent for the
	// amount. It returns ErrProviderError when Stripe rejects the request or is unreachable, in
	// which case no charge was made and the caller may retry.
	CreateIntent(ctx context.Context, req IntentRequest) (Intent, error)
}

// IntentRequest is a Stripe PaymentIntents create call. The IdempotencyKey is the client's own key,
// passed through to Stripe so a retried charge is not a double charge (payment.yaml). It carries no
// card data — the browser sends that straight to Stripe.
type IntentRequest struct {
	BookingID      string
	AmountMinor    int64
	Currency       string
	IdempotencyKey string
}

// Intent is the subset of a Stripe PaymentIntent payment keeps: its id (stored as
// provider_payment_id) and its client secret (returned to the browser once, never persisted).
type Intent struct {
	ID           string
	ClientSecret string
	Status       string
}

// Clock returns the current time. Injected so tests state the instant they mean instead of
// sleeping — hold expiry is the behaviour that matters most here.
type Clock func() time.Time

// IDGen mints a payment id. UUIDv7 in production, so ids sort by creation time.
type IDGen func() (string, error)
