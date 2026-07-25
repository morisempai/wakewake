package stripe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/morisempai/wakewake/services/payment/internal/domain"
)

// Client talks to Stripe's PaymentIntents API. It is the infra implementation of domain.Provider.
//
// The HTTP transport is injectable so tests stub it and never reach the real Stripe (a hard rule):
// a fake RoundTripper returns canned PaymentIntents and asserts the Idempotency-Key header is
// present and no card data is sent. The secret key is held here and put ONLY in the Authorization
// header — never logged, never echoed.
type Client struct {
	baseURL    string
	secretKey  string
	httpClient *http.Client
}

var _ domain.Provider = (*Client)(nil)

// NewClient wires the Stripe client. baseURL defaults to Stripe's API and is overridable so a test
// or a local mock can point it elsewhere; httpClient defaults to a 5s-timeout client.
func NewClient(baseURL, secretKey string, httpClient *http.Client) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://api.stripe.com"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &Client{baseURL: baseURL, secretKey: secretKey, httpClient: httpClient}
}

// intentResponse is the subset of a Stripe PaymentIntent we read. Stripe returns far more; we
// deliberately keep only the id, the client secret, and the status, and never the payment method.
type intentResponse struct {
	ID           string `json:"id"`
	ClientSecret string `json:"client_secret"`
	Status       string `json:"status"`
}

// CreateIntent creates (or replays, on the same idempotency key) a PaymentIntent for the amount.
//
// No card data is sent — only the amount, the currency, and the booking id as metadata. The
// idempotency key is passed to Stripe so a retried charge is deduplicated by Stripe itself, which
// is the real double-charge protection (payment.yaml).
func (c *Client) CreateIntent(ctx context.Context, req domain.IntentRequest) (domain.Intent, error) {
	form := url.Values{}
	form.Set("amount", strconv.FormatInt(req.AmountMinor, 10))
	form.Set("currency", strings.ToLower(req.Currency))
	form.Set("automatic_payment_methods[enabled]", "true")
	if req.BookingID != "" {
		form.Set("metadata[booking_id]", req.BookingID)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/payment_intents", strings.NewReader(form.Encode()))
	if err != nil {
		return domain.Intent{}, fmt.Errorf("%w: building request: %v", domain.ErrProviderError, err)
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Authorization", "Bearer "+c.secretKey)
	if req.IdempotencyKey != "" {
		httpReq.Header.Set("Idempotency-Key", req.IdempotencyKey)
	}

	res, err := c.httpClient.Do(httpReq)
	if err != nil {
		// Unreachable or timed out: no charge was made, so this is a retryable provider error, not
		// a fault. The error string never contains the secret (it is only in the header above).
		return domain.Intent{}, fmt.Errorf("%w: %v", domain.ErrProviderError, err)
	}
	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return domain.Intent{}, fmt.Errorf("%w: reading response: %v", domain.ErrProviderError, err)
	}

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// The body is not surfaced to the client (the handler maps this to a generic 502); Stripe
		// error bodies do not contain card data, but they are still not ours to echo.
		return domain.Intent{}, fmt.Errorf("%w: stripe returned %d", domain.ErrProviderError, res.StatusCode)
	}

	var intent intentResponse
	if err := json.Unmarshal(body, &intent); err != nil {
		return domain.Intent{}, fmt.Errorf("%w: decoding response: %v", domain.ErrProviderError, err)
	}
	if intent.ID == "" {
		return domain.Intent{}, fmt.Errorf("%w: stripe returned no payment intent id", domain.ErrProviderError)
	}

	return domain.Intent{ID: intent.ID, ClientSecret: intent.ClientSecret, Status: intent.Status}, nil
}
