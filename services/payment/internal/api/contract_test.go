//go:build contract

// Provider-side contract verification (AC1, AC6).
//
// Every response this service can produce is driven through the REAL router — the same handler
// cmd/payment serves, middleware, the raw Stripe webhook, and generated parameter binding included
// — and validated against contracts/openapi/payment.yaml itself. Not against a hand-written
// expectation: a struct that merely agrees with our own reading of the spec proves only that we are
// consistently wrong.
//
// No database and no live Stripe. The store, the provider, and the outcome recorder are fakes,
// because what is under test here is the HTTP contract (and the webhook's signature gate), not the
// persistence — that lives in the integration suite.
package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"

	"github.com/morisempai/wakewake/shared/contracts"
	"github.com/morisempai/wakewake/shared/platform/health"

	"github.com/morisempai/wakewake/services/payment/internal/domain"
	"github.com/morisempai/wakewake/services/payment/internal/stripe"
)

const specHost = "https://api.example.com"

const (
	custID        = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d10"
	otherCustomer = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d11"
	bookingID     = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d14"
	paymentID     = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d20"
	idempKey      = "idem-key-0123456789"
	webhookSecret = "whsec_contract_test_secret"
	intentID      = "pi_contract_123"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

type fakeStore struct {
	ctx    domain.BookingContext
	ctxErr error

	existingFound bool
	existing      domain.Payment

	idemFound     bool
	idemFP        string
	idemPaymentID string

	byID    domain.Payment
	byIDErr error

	insertErr error
}

func (f *fakeStore) BookingContext(context.Context, string) (domain.BookingContext, error) {
	if f.ctxErr != nil {
		return domain.BookingContext{}, f.ctxErr
	}
	return f.ctx, nil
}
func (f *fakeStore) PaymentByBooking(context.Context, string) (domain.Payment, bool, error) {
	return f.existing, f.existingFound, nil
}
func (f *fakeStore) ByID(context.Context, string) (domain.Payment, error) {
	if f.byIDErr != nil {
		return domain.Payment{}, f.byIDErr
	}
	return f.byID, nil
}
func (f *fakeStore) FindIdempotent(context.Context, string) (string, string, bool, error) {
	return f.idemPaymentID, f.idemFP, f.idemFound, nil
}
func (f *fakeStore) InsertPayment(_ context.Context, _ domain.Claim, p domain.Payment) (domain.Payment, error) {
	if f.insertErr != nil {
		return domain.Payment{}, f.insertErr
	}
	return p, nil
}

type fakeProvider struct {
	intent domain.Intent
	err    error
}

func (f *fakeProvider) CreateIntent(context.Context, domain.IntentRequest) (domain.Intent, error) {
	if f.err != nil {
		return domain.Intent{}, f.err
	}
	return f.intent, nil
}

type fakeRecorder struct {
	result domain.OutcomeResult
	err    error
	calls  int
}

func (f *fakeRecorder) RecordOutcome(_ context.Context, _, _, _ string, _ domain.Transition) (domain.OutcomeResult, error) {
	f.calls++
	if f.err != nil {
		return domain.OutcomeResult{}, f.err
	}
	return f.result, nil
}

func payableContext() domain.BookingContext {
	return domain.BookingContext{
		BookingID: bookingID, CustomerID: custID, AmountMinor: 12000, Currency: "EUR",
		HoldExpiresAt: time.Now().Add(15 * time.Minute),
	}
}

func pendingPayment() domain.Payment {
	return domain.Payment{
		ID: paymentID, BookingID: bookingID, Status: domain.StatusPending, AmountMinor: 12000,
		Currency: "EUR", Provider: "stripe", ProviderPaymentID: intentID,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
}

// ---------------------------------------------------------------------------
// spec plumbing
// ---------------------------------------------------------------------------

func loadSpec(t *testing.T) (*openapi3.T, routers.Router) {
	t.Helper()

	raw, err := contracts.Specs.ReadFile(contracts.OpenAPIPayment)
	if err != nil {
		t.Fatalf("reading the embedded payment spec: %v", err)
	}
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(raw)
	if err != nil {
		t.Fatalf("parsing the payment spec: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("the payment spec is not valid OpenAPI: %v", err)
	}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatalf("building a router from the spec: %v", err)
	}
	return doc, router
}

type response struct {
	method  string
	rawPath string
	status  int
	header  http.Header
	body    []byte
}

func assertMatchesSpec(t *testing.T, router routers.Router, r response) {
	t.Helper()

	req, err := http.NewRequest(r.method, specHost+r.rawPath, http.NoBody)
	if err != nil {
		t.Fatalf("building the validation request: %v", err)
	}
	route, pathParams, err := router.FindRoute(req)
	if err != nil {
		t.Fatalf("%s %s is not an operation in the spec: %v", r.method, r.rawPath, err)
	}

	err = openapi3filter.ValidateResponse(context.Background(), &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request:    req,
			PathParams: pathParams,
			Route:      route,
			Options:    &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		},
		Status:  r.status,
		Header:  r.header,
		Body:    io.NopCloser(bytes.NewReader(r.body)),
		Options: &openapi3filter.Options{IncludeResponseStatus: true},
	})
	if err != nil {
		t.Errorf("%s %s -> %d does not match the spec: %v\nbody: %s", r.method, r.rawPath, r.status, err, r.body)
	}
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type harness struct {
	server   *httptest.Server
	router   routers.Router
	recorder *fakeRecorder
	logs     *bytes.Buffer
}

func newHarness(t *testing.T, store domain.Store, provider domain.Provider, recorder *fakeRecorder, readinessFails bool) *harness {
	t.Helper()

	_, specRouter := loadSpec(t)

	svc := domain.NewService(store, provider, time.Now, func() (string, error) { return paymentID, nil })

	checker := health.NewChecker(time.Second)
	checker.Register("postgres", func(context.Context) error {
		if readinessFails {
			return errFakeDependencyDown
		}
		return nil
	})

	logs := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))

	webhook := NewWebhookHandler(svc, recorder, log, webhookSecret, stripe.DefaultTolerance, time.Now)
	server := httptest.NewServer(NewRouter(NewServer(svc, log), webhook, checker, log))
	t.Cleanup(server.Close)

	return &harness{server: server, router: specRouter, recorder: recorder, logs: logs}
}

var errFakeDependencyDown = &dependencyDownError{}

type dependencyDownError struct{}

func (*dependencyDownError) Error() string { return "connection refused" }

func bearerToken(sub string) string {
	enc := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	header := enc(map[string]string{"alg": "none", "typ": "JWT"})
	payload := enc(map[string]string{"sub": sub})
	return "Bearer " + header + "." + payload + ".sig"
}

func (h *harness) do(t *testing.T, method, path, body string, headers map[string]string) response {
	t.Helper()

	var reader io.Reader = http.NoBody
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()

	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	return response{method: method, rawPath: path, status: res.StatusCode, header: res.Header, body: raw}
}

func (h *harness) check(t *testing.T, method, path, body string, headers map[string]string, wantStatus int) response {
	t.Helper()
	got := h.do(t, method, path, body, headers)
	if got.status != wantStatus {
		t.Errorf("%s %s -> %d, want %d\nbody: %s", method, path, got.status, wantStatus, got.body)
	}
	assertMatchesSpec(t, h.router, got)
	return got
}

func authHeaders(sub, key string) map[string]string {
	h := map[string]string{"Authorization": bearerToken(sub)}
	if key != "" {
		h["Idempotency-Key"] = key
	}
	return h
}

// ---------------------------------------------------------------------------
// AC6 — createPayment: every documented response validates against the spec
// ---------------------------------------------------------------------------

func TestCreatePaymentResponsesMatchTheSpec_Issue18_AC6(t *testing.T) {
	t.Parallel()

	okProvider := &fakeProvider{intent: domain.Intent{ID: intentID, ClientSecret: "pi_secret_abc"}}
	body := `{"booking_id":"` + bookingID + `"}`

	cases := []struct {
		name     string
		store    domain.Store
		provider domain.Provider
		body     string
		headers  map[string]string
		want     int
	}{
		{"201 pending", &fakeStore{ctx: payableContext()}, okProvider, body, authHeaders(custID, idempKey), http.StatusCreated},
		{"404 booking_not_found", &fakeStore{ctxErr: domain.ErrBookingNotFound}, okProvider, body, authHeaders(custID, idempKey), http.StatusNotFound},
		{"422 booking_not_payable", &fakeStore{ctx: domain.BookingContext{BookingID: bookingID, CustomerID: custID, AmountMinor: 100, Currency: "EUR", HoldExpiresAt: time.Now().Add(-time.Minute)}}, okProvider, body, authHeaders(custID, idempKey), http.StatusUnprocessableEntity},
		{"409 payment_already_exists", &fakeStore{ctx: payableContext(), existingFound: true, existing: pendingPayment()}, okProvider, body, authHeaders(custID, idempKey), http.StatusConflict},
		{"409 idempotency_key_reuse", &fakeStore{ctx: payableContext(), idemFound: true, idemFP: "a-different-fingerprint"}, okProvider, body, authHeaders(custID, idempKey), http.StatusConflict},
		{"502 provider_error", &fakeStore{ctx: payableContext()}, &fakeProvider{err: domain.ErrProviderError}, body, authHeaders(custID, idempKey), http.StatusBadGateway},
		{"401 without a token", &fakeStore{ctx: payableContext()}, okProvider, body, map[string]string{"Idempotency-Key": idempKey}, http.StatusUnauthorized},
		{"400 missing Idempotency-Key", &fakeStore{ctx: payableContext()}, okProvider, body, map[string]string{"Authorization": bearerToken(custID)}, http.StatusBadRequest},
		{"400 Idempotency-Key too short", &fakeStore{ctx: payableContext()}, okProvider, body, authHeaders(custID, "short"), http.StatusBadRequest},
		{"400 malformed JSON body", &fakeStore{ctx: payableContext()}, okProvider, `{"booking_id": not-json`, authHeaders(custID, idempKey), http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tc.store, tc.provider, &fakeRecorder{}, false)
			h.check(t, http.MethodPost, "/v1/payments", tc.body, tc.headers, tc.want)
		})
	}
}

// The 201 body carries the one-time client secret; the field must be present on creation.
func TestCreatePaymentReturnsTheClientSecretOnCreation_Issue18_AC6(t *testing.T) {
	t.Parallel()

	h := newHarness(t, &fakeStore{ctx: payableContext()}, &fakeProvider{intent: domain.Intent{ID: intentID, ClientSecret: "pi_secret_once"}}, &fakeRecorder{}, false)
	got := h.check(t, http.MethodPost, "/v1/payments", `{"booking_id":"`+bookingID+`"}`, authHeaders(custID, idempKey), http.StatusCreated)
	if !strings.Contains(string(got.body), `"client_secret":"pi_secret_once"`) {
		t.Errorf("201 body is missing the client secret: %s", got.body)
	}
}

// ---------------------------------------------------------------------------
// AC6 — getPayment
// ---------------------------------------------------------------------------

func TestGetPaymentResponsesMatchTheSpec_Issue18_AC6(t *testing.T) {
	t.Parallel()

	path := "/v1/payments/" + paymentID

	t.Run("200 found", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{byID: pendingPayment(), ctx: payableContext()}, &fakeProvider{}, &fakeRecorder{}, false)
		got := h.check(t, http.MethodGet, path, "", authHeaders(custID, ""), http.StatusOK)
		if strings.Contains(string(got.body), "client_secret") {
			t.Errorf("GET must never carry a client secret: %s", got.body)
		}
	})

	t.Run("403 another customer's payment", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{byID: pendingPayment(), ctx: payableContext()}, &fakeProvider{}, &fakeRecorder{}, false)
		h.check(t, http.MethodGet, path, "", authHeaders(otherCustomer, ""), http.StatusForbidden)
	})

	t.Run("404 not found", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{byIDErr: domain.ErrPaymentNotFound}, &fakeProvider{}, &fakeRecorder{}, false)
		h.check(t, http.MethodGet, path, "", authHeaders(custID, ""), http.StatusNotFound)
	})

	t.Run("401 without a token", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{byID: pendingPayment(), ctx: payableContext()}, &fakeProvider{}, &fakeRecorder{}, false)
		h.check(t, http.MethodGet, path, "", nil, http.StatusUnauthorized)
	})
}

// ---------------------------------------------------------------------------
// AC6 — health probes
// ---------------------------------------------------------------------------

func TestHealthProbeResponsesMatchTheSpec_Issue18_AC6(t *testing.T) {
	t.Parallel()

	t.Run("200 healthz stays up while a dependency is down", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{}, &fakeProvider{}, &fakeRecorder{}, true)
		h.check(t, http.MethodGet, "/healthz", "", nil, http.StatusOK)
	})
	t.Run("200 readyz", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{}, &fakeProvider{}, &fakeRecorder{}, false)
		h.check(t, http.MethodGet, "/readyz", "", nil, http.StatusOK)
	})
	t.Run("503 readyz when a dependency is down", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{}, &fakeProvider{}, &fakeRecorder{}, true)
		h.check(t, http.MethodGet, "/readyz", "", nil, http.StatusServiceUnavailable)
	})
}

// ---------------------------------------------------------------------------
// AC1 — the Stripe webhook: a valid signature is processed, an invalid one is rejected and
// mutates nothing. Both proven through the real router.
// ---------------------------------------------------------------------------

func stripeEvent(id, eventType, intent string) string {
	return `{"id":"` + id + `","type":"` + eventType + `","data":{"object":{"id":"` + intent + `"}}}`
}

func TestWebhookAcceptsAValidlySignedPayload_Issue18_AC1(t *testing.T) {
	t.Parallel()

	recorder := &fakeRecorder{result: domain.OutcomeResult{Found: true, Changed: true, Payment: pendingPayment()}}
	h := newHarness(t, &fakeStore{}, &fakeProvider{}, recorder, false)

	body := stripeEvent("evt_ok", stripe.EventPaymentIntentSucceeded, intentID)
	sig := stripe.Sign([]byte(body), webhookSecret, time.Now())

	got := h.check(t, http.MethodPost, "/v1/webhooks/stripe", body,
		map[string]string{"Stripe-Signature": sig}, http.StatusOK)

	if !strings.Contains(string(got.body), `"received":true`) {
		t.Errorf("200 body = %s, want {\"received\":true}", got.body)
	}
	if recorder.calls != 1 {
		t.Errorf("recorder called %d times, want 1 — a valid webhook must be processed", recorder.calls)
	}
}

func TestWebhookRejectsAnInvalidSignatureAndMutatesNothing_Issue18_AC1(t *testing.T) {
	t.Parallel()

	body := stripeEvent("evt_forged", stripe.EventPaymentIntentSucceeded, intentID)

	cases := []struct {
		name string
		sig  string
	}{
		{"absent signature", ""},
		{"wrong secret", stripe.Sign([]byte(body), "whsec_attacker", time.Now())},
		{"tampered body", func() string {
			return stripe.Sign([]byte(stripeEvent("evt_forged", stripe.EventPaymentIntentSucceeded, "pi_other")), webhookSecret, time.Now())
		}()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			recorder := &fakeRecorder{}
			h := newHarness(t, &fakeStore{}, &fakeProvider{}, recorder, false)

			headers := map[string]string{}
			if tc.sig != "" {
				headers["Stripe-Signature"] = tc.sig
			}
			h.check(t, http.MethodPost, "/v1/webhooks/stripe", body, headers, http.StatusBadRequest)

			if recorder.calls != 0 {
				t.Errorf("recorder called %d times on an invalid signature — a rejected webhook must mutate nothing", recorder.calls)
			}
		})
	}
}

// An unknown event type is acknowledged with 200 and ignored — nothing is recorded.
func TestWebhookAcknowledgesAndIgnoresUnknownEventTypes_Issue18_AC1(t *testing.T) {
	t.Parallel()

	recorder := &fakeRecorder{}
	h := newHarness(t, &fakeStore{}, &fakeProvider{}, recorder, false)

	body := stripeEvent("evt_ignored", "customer.created", "")
	sig := stripe.Sign([]byte(body), webhookSecret, time.Now())

	h.check(t, http.MethodPost, "/v1/webhooks/stripe", body, map[string]string{"Stripe-Signature": sig}, http.StatusOK)
	if recorder.calls != 0 {
		t.Errorf("an ignored event type was recorded (%d calls), want 0", recorder.calls)
	}
}

// The signing secret must never appear in the logs, even on the rejection path.
func TestWebhookNeverLogsTheSigningSecret_Issue18_AC6(t *testing.T) {
	t.Parallel()

	recorder := &fakeRecorder{}
	h := newHarness(t, &fakeStore{}, &fakeProvider{}, recorder, false)

	body := stripeEvent("evt_x", stripe.EventPaymentIntentSucceeded, intentID)
	h.do(t, http.MethodPost, "/v1/webhooks/stripe", body, map[string]string{"Stripe-Signature": "t=1,v1=deadbeef"})

	if strings.Contains(h.logs.String(), webhookSecret) {
		t.Errorf("the signing secret leaked into the logs:\n%s", h.logs.String())
	}
}

// ---------------------------------------------------------------------------
// AC6 — correlation id is echoed and carried into the error envelope
// ---------------------------------------------------------------------------

func TestCorrelationIDIsEchoedAndCarriedIntoTheErrorEnvelope_Issue18_AC6(t *testing.T) {
	t.Parallel()

	const corrID = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d8e"

	h := newHarness(t, &fakeStore{ctxErr: domain.ErrBookingNotFound}, &fakeProvider{}, &fakeRecorder{}, false)
	got := h.do(t, http.MethodPost, "/v1/payments", `{"booking_id":"`+bookingID+`"}`, map[string]string{
		"Authorization":    bearerToken(custID),
		"Idempotency-Key":  idempKey,
		"X-Correlation-Id": corrID,
	})

	if echoed := got.header.Get("X-Correlation-Id"); echoed != corrID {
		t.Errorf("X-Correlation-Id response header = %q, want %q", echoed, corrID)
	}
	if !strings.Contains(string(got.body), `"correlation_id":"`+corrID+`"`) {
		t.Errorf("the error envelope does not carry the correlation id: %s", got.body)
	}
	assertMatchesSpec(t, h.router, got)
}
