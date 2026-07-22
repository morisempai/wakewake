package domain

import (
	"context"
	"errors"
	"fmt"
)

// CreateCommand is a validated-at-the-edge createPayment request. The booking id is the only thing
// the client supplies; the amount is read from the booking context (payment.yaml).
type CreateCommand struct {
	BookingID   string
	Idempotency Claim
}

// Service is the payment use-case layer: it decides, the store persists, and the Provider port
// reaches Stripe.
//
// It holds no state beyond its dependencies, so it is safe for concurrent use by every HTTP handler
// and the webhook at once.
type Service struct {
	store    Store
	provider Provider
	now      Clock
	newID    IDGen
}

// NewService wires the service. The clock and id generator are parameters rather than package calls
// so tests are deterministic (testing-standards).
func NewService(store Store, provider Provider, now Clock, newID IDGen) *Service {
	return &Service{store: store, provider: provider, now: now, newID: newID}
}

// CreatePayment starts a payment for a held booking: read the authoritative amount from the booking
// context, create a Stripe PaymentIntent carrying the idempotency key, and record a pending payment.
//
// The returned client secret is Stripe's, for the browser to complete the charge directly. It is
// NEVER persisted (payment.yaml) — on a replay it is recovered from Stripe, which returns the same
// intent for the same idempotency key.
//
// Ordering is deliberate. The booking context is resolved first so an unknown or unpayable booking
// is rejected before Stripe is touched. A replay short-circuits next. Only a genuinely new request
// creates an intent and inserts the payment; the intent is created BEFORE the row so a crash
// between them is recoverable — a retry with the same key replays the same intent at Stripe rather
// than opening a second charge.
func (s *Service) CreatePayment(ctx context.Context, cmd CreateCommand) (Payment, string, error) {
	if cmd.BookingID == "" {
		return Payment{}, "", ErrBookingNotFound
	}

	bctx, err := s.store.BookingContext(ctx, cmd.BookingID)
	if err != nil {
		return Payment{}, "", err
	}

	// The only payability signal payment holds locally is the hold's expiry. A confirmed or
	// cancelled booking is not distinguishable here — see the README's flagged limitation.
	if !s.now().Before(bctx.HoldExpiresAt) {
		return Payment{}, "", ErrBookingNotPayable
	}

	// Replay short-circuit: a retried request with the same key returns its original payment. The
	// client secret is recovered from Stripe (idempotent on the same key), since we never store it.
	if paymentID, fingerprint, found, err := s.store.FindIdempotent(ctx, cmd.Idempotency.Key); err != nil {
		return Payment{}, "", err
	} else if found {
		if fingerprint != cmd.Idempotency.Fingerprint {
			return Payment{}, "", ErrIdempotencyKeyReuse
		}
		existing, err := s.store.ByID(ctx, paymentID)
		if err != nil {
			return Payment{}, "", err
		}
		intent, err := s.createIntent(ctx, existing.BookingID, existing.AmountMinor, existing.Currency, cmd.Idempotency.Key)
		if err != nil {
			return Payment{}, "", err
		}
		return existing, intent.ClientSecret, nil
	}

	// A booking gets at most one payment in flight. Checked before Stripe so the common conflict
	// costs no charge attempt; the unique constraint is the race-safe backstop below.
	if _, exists, err := s.store.PaymentByBooking(ctx, cmd.BookingID); err != nil {
		return Payment{}, "", err
	} else if exists {
		return Payment{}, "", ErrPaymentAlreadyExists
	}

	paymentID, err := s.newID()
	if err != nil {
		return Payment{}, "", fmt.Errorf("payment: generating payment id: %w", err)
	}

	intent, err := s.createIntent(ctx, bctx.BookingID, bctx.AmountMinor, bctx.Currency, cmd.Idempotency.Key)
	if err != nil {
		return Payment{}, "", err
	}

	p := NewPayment(paymentID, bctx.BookingID, bctx.AmountMinor, bctx.Currency, intent.ID, s.now())

	stored, err := s.store.InsertPayment(ctx, cmd.Idempotency, p)
	if err != nil {
		return Payment{}, "", err
	}
	return stored, intent.ClientSecret, nil
}

// Get returns one payment, enforcing that it belongs to the caller's booking.
func (s *Service) Get(ctx context.Context, id, customerID string) (Payment, error) {
	p, err := s.store.ByID(ctx, id)
	if err != nil {
		return Payment{}, err
	}
	// Authorisation is by the booking's owner. The customer id is not on the payment row, so it is
	// resolved through the booking context; a payment whose context we no longer have cannot be
	// authorised and is treated as forbidden rather than leaked.
	bctx, err := s.store.BookingContext(ctx, p.BookingID)
	if err != nil {
		if errors.Is(err, ErrBookingNotFound) {
			return Payment{}, ErrForbidden
		}
		return Payment{}, err
	}
	if bctx.CustomerID != customerID {
		return Payment{}, ErrForbidden
	}
	return p, nil
}

// OutcomeTransition is the transition the webhook applies inside the dedupe transaction: promote or
// fail the payment and stage the corresponding event. Pure — it only decides, the store persists.
func (s *Service) OutcomeTransition(o WebhookOutcome) Transition {
	return func(p Payment) (Payment, []Emission, error) {
		if o.Succeeded {
			return p.MarkSucceeded(s.now())
		}
		return p.MarkFailed(o.FailureCode, s.now())
	}
}

// createIntent calls Stripe and normalises its failure onto ErrProviderError.
func (s *Service) createIntent(ctx context.Context, bookingID string, amountMinor int64, currency, idempotencyKey string) (Intent, error) {
	intent, err := s.provider.CreateIntent(ctx, IntentRequest{
		BookingID:      bookingID,
		AmountMinor:    amountMinor,
		Currency:       currency,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		if errors.Is(err, ErrProviderError) {
			return Intent{}, err
		}
		return Intent{}, fmt.Errorf("%w: %v", ErrProviderError, err)
	}
	return intent, nil
}
