package stripe

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/morisempai/wakewake/services/payment/internal/domain"
)

// roundTripFunc stubs the HTTP transport so no request ever reaches the real Stripe.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

// CreateIntent must carry the idempotency key to Stripe and must send no card data — only the
// amount, currency, and booking metadata.
func TestCreateIntentCarriesIdempotencyKeyAndNoCardData_Issue18(t *testing.T) {
	t.Parallel()

	var (
		gotIdempotencyKey string
		gotAuth           string
		gotBody           string
	)
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotIdempotencyKey = r.Header.Get("Idempotency-Key")
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return jsonResponse(200, `{"id":"pi_abc","client_secret":"pi_abc_secret_xyz","status":"requires_payment_method"}`), nil
	})

	client := NewClient("https://stripe.test", "sk_test_key", &http.Client{Transport: transport})

	intent, err := client.CreateIntent(context.Background(), domain.IntentRequest{
		BookingID:      "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d14",
		AmountMinor:    12000,
		Currency:       "EUR",
		IdempotencyKey: "idem-key-0123456789",
	})
	if err != nil {
		t.Fatalf("CreateIntent returned %v", err)
	}

	if intent.ID != "pi_abc" || intent.ClientSecret != "pi_abc_secret_xyz" {
		t.Errorf("intent = %+v, want id pi_abc / secret pi_abc_secret_xyz", intent)
	}
	if gotIdempotencyKey != "idem-key-0123456789" {
		t.Errorf("Idempotency-Key header = %q, want the request's key (a retried charge without it is a double charge)", gotIdempotencyKey)
	}
	if gotAuth != "Bearer sk_test_key" {
		t.Errorf("Authorization header = %q, want the bearer secret", gotAuth)
	}
	// The request must contain only amount/currency/metadata — never anything resembling card data.
	for _, forbidden := range []string{"card", "number", "cvc", "cvv", "exp_month", "exp_year", "pan"} {
		if strings.Contains(strings.ToLower(gotBody), forbidden) {
			t.Errorf("the Stripe request body contains %q — this service must never handle card data: %s", forbidden, gotBody)
		}
	}
	if !strings.Contains(gotBody, "amount=12000") || !strings.Contains(gotBody, "currency=eur") {
		t.Errorf("the Stripe request is missing the amount/currency: %s", gotBody)
	}
}

// A Stripe error or an unreachable Stripe is a retryable provider error, and the error must not leak
// the secret key.
func TestCreateIntentMapsStripeFailureToProviderError_Issue18(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		transport roundTripFunc
	}{
		{
			name: "non-2xx from Stripe",
			transport: func(*http.Request) (*http.Response, error) {
				return jsonResponse(402, `{"error":{"message":"card_declined"}}`), nil
			},
		},
		{
			name: "transport error (unreachable)",
			transport: func(*http.Request) (*http.Response, error) {
				return nil, errors.New("dial tcp: connection refused")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := NewClient("https://stripe.test", "sk_test_supersecret", &http.Client{Transport: tc.transport})
			_, err := client.CreateIntent(context.Background(), domain.IntentRequest{
				AmountMinor: 100, Currency: "EUR", IdempotencyKey: "idem-key-0123456789",
			})
			if !errors.Is(err, domain.ErrProviderError) {
				t.Fatalf("error = %v, want ErrProviderError", err)
			}
			if strings.Contains(err.Error(), "sk_test_supersecret") {
				t.Errorf("the provider error leaks the secret key: %v", err)
			}
		})
	}
}
