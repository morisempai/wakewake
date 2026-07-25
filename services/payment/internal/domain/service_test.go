package domain

import (
	"context"
	"errors"
	"testing"
	"time"
)

const (
	testBookingID = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d14"
	testCustomer  = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d10"
	otherCustomer = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d11"
	testPaymentID = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d20"
	testIntentID  = "pi_test_123"
	testIdemKey   = "idem-key-0123456789"
	testClientSec = "pi_test_123_secret_abc"
)

var (
	now    = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	future = now.Add(15 * time.Minute)
	past   = now.Add(-time.Minute)
)

// fakeStore is a controllable in-memory Store for the createPayment unit tests.
type fakeStore struct {
	ctx    BookingContext
	ctxErr error

	existing      Payment // returned by PaymentByBooking when existingFound
	existingFound bool

	idemPaymentID string
	idemFP        string
	idemFound     bool

	byID    Payment
	byIDErr error

	insertErr error
	inserted  Payment
	insertN   int
}

func (f *fakeStore) BookingContext(context.Context, string) (BookingContext, error) {
	if f.ctxErr != nil {
		return BookingContext{}, f.ctxErr
	}
	return f.ctx, nil
}

func (f *fakeStore) PaymentByBooking(context.Context, string) (Payment, bool, error) {
	return f.existing, f.existingFound, nil
}

func (f *fakeStore) ByID(context.Context, string) (Payment, error) {
	if f.byIDErr != nil {
		return Payment{}, f.byIDErr
	}
	return f.byID, nil
}

func (f *fakeStore) FindIdempotent(context.Context, string) (string, string, bool, error) {
	return f.idemPaymentID, f.idemFP, f.idemFound, nil
}

func (f *fakeStore) InsertPayment(_ context.Context, _ Claim, p Payment) (Payment, error) {
	f.insertN++
	if f.insertErr != nil {
		return Payment{}, f.insertErr
	}
	f.inserted = p
	return p, nil
}

// fakeProvider records the intent request so a test can assert what was sent to Stripe.
type fakeProvider struct {
	intent Intent
	err    error
	calls  int
	got    IntentRequest
}

func (f *fakeProvider) CreateIntent(_ context.Context, req IntentRequest) (Intent, error) {
	f.calls++
	f.got = req
	if f.err != nil {
		return Intent{}, f.err
	}
	return f.intent, nil
}

func payableContext() BookingContext {
	return BookingContext{
		BookingID:     testBookingID,
		CustomerID:    testCustomer,
		AmountMinor:   12000,
		Currency:      "EUR",
		HoldExpiresAt: future,
	}
}

func newService(store Store, provider Provider) *Service {
	return NewService(store, provider,
		func() time.Time { return now },
		func() (string, error) { return testPaymentID, nil })
}

func createCmd() CreateCommand {
	return CreateCommand{BookingID: testBookingID, Idempotency: Claim{Key: testIdemKey, Fingerprint: "fp-1"}}
}

// The happy path: a pending payment is created, the amount comes from the booking context (never
// the request), and the idempotency key is passed to Stripe.
func TestCreatePaymentCreatesPendingAndReadsAmountFromBooking_Issue18(t *testing.T) {
	t.Parallel()

	store := &fakeStore{ctx: payableContext()}
	provider := &fakeProvider{intent: Intent{ID: testIntentID, ClientSecret: testClientSec}}
	svc := newService(store, provider)

	p, secret, err := svc.CreatePayment(context.Background(), createCmd())
	if err != nil {
		t.Fatalf("CreatePayment returned %v", err)
	}

	if p.Status != StatusPending {
		t.Errorf("status = %q, want pending", p.Status)
	}
	if p.AmountMinor != 12000 || p.Currency != "EUR" {
		t.Errorf("amount/currency = %d/%s, want 12000/EUR from the booking", p.AmountMinor, p.Currency)
	}
	if p.ProviderPaymentID != testIntentID {
		t.Errorf("provider_payment_id = %q, want the Stripe intent id", p.ProviderPaymentID)
	}
	if secret != testClientSec {
		t.Errorf("client secret = %q, want Stripe's", secret)
	}
	if provider.got.IdempotencyKey != testIdemKey {
		t.Errorf("Stripe idempotency key = %q, want the request's key", provider.got.IdempotencyKey)
	}
	if provider.got.AmountMinor != 12000 {
		t.Errorf("Stripe amount = %d, want 12000 read from the booking, not the request", provider.got.AmountMinor)
	}
}

func TestCreatePaymentErrorMapping_Issue18(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		store        *fakeStore
		provider     *fakeProvider
		wantErr      error
		wantProvider int // expected Stripe calls
	}{
		{
			name:    "unknown booking",
			store:   &fakeStore{ctxErr: ErrBookingNotFound},
			wantErr: ErrBookingNotFound, wantProvider: 0,
		},
		{
			name: "hold expired is not payable",
			store: &fakeStore{ctx: BookingContext{
				BookingID: testBookingID, CustomerID: testCustomer, AmountMinor: 100, Currency: "EUR", HoldExpiresAt: past,
			}},
			wantErr: ErrBookingNotPayable, wantProvider: 0,
		},
		{
			name:    "key reused with a different body",
			store:   &fakeStore{ctx: payableContext(), idemFound: true, idemFP: "a-different-fingerprint"},
			wantErr: ErrIdempotencyKeyReuse, wantProvider: 0,
		},
		{
			name:    "booking already has a payment",
			store:   &fakeStore{ctx: payableContext(), existingFound: true, existing: Payment{ID: testPaymentID}},
			wantErr: ErrPaymentAlreadyExists, wantProvider: 0,
		},
		{
			name:     "stripe unavailable",
			store:    &fakeStore{ctx: payableContext()},
			provider: &fakeProvider{err: ErrProviderError},
			wantErr:  ErrProviderError, wantProvider: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			provider := tc.provider
			if provider == nil {
				provider = &fakeProvider{intent: Intent{ID: testIntentID, ClientSecret: testClientSec}}
			}
			svc := newService(tc.store, provider)

			_, _, err := svc.CreatePayment(context.Background(), createCmd())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if provider.calls != tc.wantProvider {
				t.Errorf("Stripe called %d times, want %d", provider.calls, tc.wantProvider)
			}
			if tc.store.insertN != 0 {
				t.Errorf("a payment was inserted on an error path (%d inserts)", tc.store.insertN)
			}
		})
	}
}

// A replay returns the original payment and does NOT insert a second one; the client secret is
// recovered from Stripe (idempotent on the same key), since we never persist it.
func TestCreatePaymentReplayReturnsOriginalWithoutASecondInsert_Issue18_AC3(t *testing.T) {
	t.Parallel()

	original := Payment{
		ID: testPaymentID, BookingID: testBookingID, Status: StatusPending,
		AmountMinor: 12000, Currency: "EUR", Provider: providerStripe, ProviderPaymentID: testIntentID,
	}
	store := &fakeStore{
		ctx:           payableContext(),
		idemFound:     true,
		idemFP:        "fp-1", // matches createCmd's fingerprint
		idemPaymentID: testPaymentID,
		byID:          original,
	}
	provider := &fakeProvider{intent: Intent{ID: testIntentID, ClientSecret: testClientSec}}
	svc := newService(store, provider)

	p, secret, err := svc.CreatePayment(context.Background(), createCmd())
	if err != nil {
		t.Fatalf("replay returned %v", err)
	}
	if p.ID != testPaymentID {
		t.Errorf("replay returned payment %q, want the original %q", p.ID, testPaymentID)
	}
	if store.insertN != 0 {
		t.Errorf("replay inserted %d payments, want 0", store.insertN)
	}
	if secret != testClientSec {
		t.Errorf("replay client secret = %q, want Stripe's replayed secret", secret)
	}
}

// Get authorises against the booking's owner.
func TestGetAuthorisesAgainstTheBookingOwner_Issue18(t *testing.T) {
	t.Parallel()

	payment := Payment{ID: testPaymentID, BookingID: testBookingID, Status: StatusPending}

	t.Run("owner sees it", func(t *testing.T) {
		t.Parallel()
		store := &fakeStore{byID: payment, ctx: payableContext()}
		got, err := newService(store, &fakeProvider{}).Get(context.Background(), testPaymentID, testCustomer)
		if err != nil {
			t.Fatalf("Get returned %v", err)
		}
		if got.ID != testPaymentID {
			t.Errorf("got %q, want %q", got.ID, testPaymentID)
		}
	})

	t.Run("another customer is forbidden", func(t *testing.T) {
		t.Parallel()
		store := &fakeStore{byID: payment, ctx: payableContext()}
		_, err := newService(store, &fakeProvider{}).Get(context.Background(), testPaymentID, otherCustomer)
		if !errors.Is(err, ErrForbidden) {
			t.Errorf("error = %v, want ErrForbidden", err)
		}
	})

	t.Run("unknown payment", func(t *testing.T) {
		t.Parallel()
		store := &fakeStore{byIDErr: ErrPaymentNotFound}
		_, err := newService(store, &fakeProvider{}).Get(context.Background(), testPaymentID, testCustomer)
		if !errors.Is(err, ErrPaymentNotFound) {
			t.Errorf("error = %v, want ErrPaymentNotFound", err)
		}
	})
}
