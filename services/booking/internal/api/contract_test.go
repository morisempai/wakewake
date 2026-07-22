//go:build contract

// Provider-side contract verification (AC5, AC6).
//
// Every response this service can produce is driven through the REAL router — the same handler
// cmd/booking serves, middleware and generated parameter binding included — and validated against
// contracts/openapi/booking.yaml itself. Not against a hand-written expectation: a struct that
// merely agrees with our own reading of the spec proves only that we are consistently wrong.
//
// No database and no live Availability/Catalog. The store and the two clients are fakes, because
// what is under test here is the HTTP contract, not the saga — that lives in the integration suite.
package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/morisempai/wakewake/services/booking/internal/domain"
)

// specHost is the server URL the spec declares. Route lookup matches against it.
const specHost = "https://api.example.com"

const (
	custID        = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d10"
	otherCustomer = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d11"
	productID     = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d12"
	resourceID    = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d13"
	bookingID     = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d14"
	reservationID = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d15"
	idempKey      = "idem-key-0123456789"
)

var (
	windowStart = time.Date(2099, 7, 20, 10, 0, 0, 0, time.UTC)
	windowEnd   = time.Date(2099, 7, 20, 11, 0, 0, 0, time.UTC)
	stamp       = time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
)

// ---------------------------------------------------------------------------
// spec plumbing
// ---------------------------------------------------------------------------

func loadSpec(t *testing.T) (*openapi3.T, routers.Router) {
	t.Helper()

	raw, err := contracts.Specs.ReadFile(contracts.OpenAPIBooking)
	if err != nil {
		t.Fatalf("reading the embedded booking spec: %v", err)
	}

	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(raw)
	if err != nil {
		t.Fatalf("parsing the booking spec: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("the booking spec is not valid OpenAPI: %v", err)
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

// assertMatchesSpec fails unless the status is documented for this operation AND the body validates
// against its schema.
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
			Options: &openapi3filter.Options{
				// The spec declares bearerAuth, but authentication is not what this test verifies,
				// so it is stubbed rather than silently ignored.
				AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
			},
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
// fakes
// ---------------------------------------------------------------------------

type fakeStore struct {
	booking   domain.Booking
	byIDErr   error
	insertErr error
	applyErr  error
	page      domain.ListPage

	findFound       bool
	findFingerprint string
}

func (f *fakeStore) Insert(_ context.Context, _ domain.Claim, b domain.Booking) (domain.Booking, error) {
	if f.insertErr != nil {
		return domain.Booking{}, f.insertErr
	}
	return b, nil
}

func (f *fakeStore) ByID(_ context.Context, _ string) (domain.Booking, error) {
	if f.byIDErr != nil {
		return domain.Booking{}, f.byIDErr
	}
	return f.booking, nil
}

func (f *fakeStore) List(_ context.Context, _ domain.ListQuery) (domain.ListPage, error) {
	return f.page, nil
}

func (f *fakeStore) Apply(_ context.Context, _ string, fn domain.Transition) (domain.Booking, bool, error) {
	if f.applyErr != nil {
		return domain.Booking{}, false, f.applyErr
	}
	next, emissions, err := fn(f.booking)
	if err != nil {
		return domain.Booking{}, false, err
	}
	return next, len(emissions) > 0, nil
}

func (f *fakeStore) FindIdempotent(_ context.Context, _ string) (string, string, bool, error) {
	return bookingID, f.findFingerprint, f.findFound, nil
}

type fakeReservations struct {
	holdErr    error
	confirmErr error
}

func (f *fakeReservations) Hold(_ context.Context, req domain.HoldRequest) (domain.Reservation, error) {
	if f.holdErr != nil {
		return domain.Reservation{}, f.holdErr
	}
	return domain.Reservation{ID: reservationID, BookingID: req.BookingID, ExpiresAt: stamp.Add(15 * time.Minute)}, nil
}

func (f *fakeReservations) Confirm(context.Context, string) error { return f.confirmErr }

type fakeCatalog struct {
	product domain.Product
	err     error
}

func (f *fakeCatalog) Product(context.Context, string) (domain.Product, error) {
	if f.err != nil {
		return domain.Product{}, f.err
	}
	return f.product, nil
}

func heldBooking() domain.Booking {
	expiry := stamp.Add(15 * time.Minute)
	resID := reservationID
	return domain.Booking{
		ID:            bookingID,
		CustomerID:    custID,
		ProductID:     productID,
		ResourceID:    resourceID,
		ReservationID: &resID,
		StartsAt:      windowStart,
		EndsAt:        windowEnd,
		PartySize:     2,
		Status:        domain.StatusHeld,
		HoldExpiresAt: &expiry,
		TotalMinor:    12000,
		Currency:      "EUR",
		CreatedAt:     stamp,
		UpdatedAt:     stamp,
	}
}

func defaultProduct() domain.Product {
	return domain.Product{ResourceID: resourceID, Capacity: 4, PriceMinor: 12000, Currency: "EUR"}
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type harness struct {
	server *httptest.Server
	router routers.Router
	doc    *openapi3.T
}

func newHarness(t *testing.T, store domain.Store, res domain.Reservations, cat domain.Catalog, readinessFails bool) *harness {
	t.Helper()

	doc, specRouter := loadSpec(t)

	svc := domain.NewService(store, res, cat, func() time.Time { return stamp },
		func() (string, error) { return bookingID, nil }, 15*time.Minute)

	checker := health.NewChecker(time.Second)
	checker.Register("postgres", func(context.Context) error {
		if readinessFails {
			return errors.New("connection refused")
		}
		return nil
	})

	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	server := httptest.NewServer(NewRouter(NewServer(svc, log), checker, log))
	t.Cleanup(server.Close)

	return &harness{server: server, router: specRouter, doc: doc}
}

// bearerToken builds an UNSIGNED JWT carrying a `sub` claim, for the auth middleware to read. The
// middleware does not verify signatures (ADR-0006 puts verification at the gateway), so an unsigned
// token is enough to exercise the authenticated paths.
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

func createBody(startsAt, endsAt time.Time, party int) string {
	return fmt.Sprintf(`{"product_id":%q,"starts_at":%q,"ends_at":%q,"party_size":%d}`,
		productID, startsAt.Format(time.RFC3339), endsAt.Format(time.RFC3339), party)
}

func authHeaders(sub, key string) map[string]string {
	h := map[string]string{"Authorization": bearerToken(sub)}
	if key != "" {
		h["Idempotency-Key"] = key
	}
	return h
}

// ---------------------------------------------------------------------------
// AC5/AC6 — every documented response validates against the spec
// ---------------------------------------------------------------------------

func TestCreateBookingResponsesMatchTheSpec_Issue14_AC5(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		store   *fakeStore
		res     *fakeReservations
		cat     *fakeCatalog
		body    string
		headers map[string]string
		want    int
	}{
		{
			name:  "201 held",
			store: &fakeStore{booking: heldBooking()}, res: &fakeReservations{}, cat: &fakeCatalog{product: defaultProduct()},
			body: createBody(windowStart, windowEnd, 2), headers: authHeaders(custID, idempKey), want: http.StatusCreated,
		},
		{
			name:  "404 product_not_found",
			store: &fakeStore{}, res: &fakeReservations{}, cat: &fakeCatalog{err: domain.ErrProductNotFound},
			body: createBody(windowStart, windowEnd, 2), headers: authHeaders(custID, idempKey), want: http.StatusNotFound,
		},
		{
			name:  "409 slot_unavailable",
			store: &fakeStore{}, res: &fakeReservations{holdErr: domain.ErrSlotUnavailable}, cat: &fakeCatalog{product: defaultProduct()},
			body: createBody(windowStart, windowEnd, 2), headers: authHeaders(custID, idempKey), want: http.StatusConflict,
		},
		{
			name:  "409 idempotency_key_reuse",
			store: &fakeStore{findFound: true, findFingerprint: "a-different-fingerprint"}, res: &fakeReservations{}, cat: &fakeCatalog{product: defaultProduct()},
			body: createBody(windowStart, windowEnd, 2), headers: authHeaders(custID, idempKey), want: http.StatusConflict,
		},
		{
			name:  "422 invalid_time_window",
			store: &fakeStore{}, res: &fakeReservations{}, cat: &fakeCatalog{product: defaultProduct()},
			body: createBody(windowEnd, windowStart, 2), headers: authHeaders(custID, idempKey), want: http.StatusUnprocessableEntity,
		},
		{
			name:  "422 party_exceeds_capacity",
			store: &fakeStore{}, res: &fakeReservations{}, cat: &fakeCatalog{product: defaultProduct()},
			body: createBody(windowStart, windowEnd, 99), headers: authHeaders(custID, idempKey), want: http.StatusUnprocessableEntity,
		},
		{
			name:  "503 availability_unavailable",
			store: &fakeStore{}, res: &fakeReservations{holdErr: domain.ErrAvailabilityUnavailable}, cat: &fakeCatalog{product: defaultProduct()},
			body: createBody(windowStart, windowEnd, 2), headers: authHeaders(custID, idempKey), want: http.StatusServiceUnavailable,
		},
		{
			name:  "401 without a token",
			store: &fakeStore{}, res: &fakeReservations{}, cat: &fakeCatalog{product: defaultProduct()},
			body: createBody(windowStart, windowEnd, 2), headers: map[string]string{"Idempotency-Key": idempKey}, want: http.StatusUnauthorized,
		},
		{
			name:  "400 missing Idempotency-Key",
			store: &fakeStore{}, res: &fakeReservations{}, cat: &fakeCatalog{product: defaultProduct()},
			body: createBody(windowStart, windowEnd, 2), headers: map[string]string{"Authorization": bearerToken(custID)}, want: http.StatusBadRequest,
		},
		{
			name:  "400 Idempotency-Key below minLength",
			store: &fakeStore{}, res: &fakeReservations{}, cat: &fakeCatalog{product: defaultProduct()},
			body: createBody(windowStart, windowEnd, 2), headers: authHeaders(custID, "short"), want: http.StatusBadRequest,
		},
		{
			name:  "400 party_size below 1",
			store: &fakeStore{}, res: &fakeReservations{}, cat: &fakeCatalog{product: defaultProduct()},
			body: createBody(windowStart, windowEnd, 0), headers: authHeaders(custID, idempKey), want: http.StatusBadRequest,
		},
		{
			name:  "400 malformed JSON body",
			store: &fakeStore{}, res: &fakeReservations{}, cat: &fakeCatalog{product: defaultProduct()},
			body: `{"product_id": not-json`, headers: authHeaders(custID, idempKey), want: http.StatusBadRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tc.store, tc.res, tc.cat, false)
			h.check(t, http.MethodPost, "/v1/bookings", tc.body, tc.headers, tc.want)
		})
	}
}

func TestListBookingsResponsesMatchTheSpec_Issue14_AC5(t *testing.T) {
	t.Parallel()

	t.Run("200 a page", func(t *testing.T) {
		t.Parallel()
		next := "cursor-abc"
		store := &fakeStore{page: domain.ListPage{Bookings: []domain.Booking{heldBooking()}, NextCursor: &next}}
		h := newHarness(t, store, &fakeReservations{}, &fakeCatalog{}, false)
		got := h.check(t, http.MethodGet, "/v1/bookings", "", authHeaders(custID, ""), http.StatusOK)
		if !strings.Contains(string(got.body), `"next_cursor":"cursor-abc"`) {
			t.Errorf("next_cursor missing: %s", got.body)
		}
	})

	t.Run("200 an empty page marshals data as []", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{page: domain.ListPage{}}, &fakeReservations{}, &fakeCatalog{}, false)
		got := h.check(t, http.MethodGet, "/v1/bookings?status=held", "", authHeaders(custID, ""), http.StatusOK)
		if !strings.Contains(string(got.body), `"data":[]`) {
			t.Errorf("empty result did not marshal as []: %s", got.body)
		}
		if !strings.Contains(string(got.body), `"next_cursor":null`) {
			t.Errorf("next_cursor must be null on the last page: %s", got.body)
		}
	})

	t.Run("401 without a token", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{}, &fakeReservations{}, &fakeCatalog{}, false)
		h.check(t, http.MethodGet, "/v1/bookings", "", nil, http.StatusUnauthorized)
	})
}

func TestGetBookingResponsesMatchTheSpec_Issue14_AC5(t *testing.T) {
	t.Parallel()

	path := "/v1/bookings/" + bookingID

	t.Run("200 found", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{booking: heldBooking()}, &fakeReservations{}, &fakeCatalog{}, false)
		h.check(t, http.MethodGet, path, "", authHeaders(custID, ""), http.StatusOK)
	})

	t.Run("403 another customer's booking", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{booking: heldBooking()}, &fakeReservations{}, &fakeCatalog{}, false)
		h.check(t, http.MethodGet, path, "", authHeaders(otherCustomer, ""), http.StatusForbidden)
	})

	t.Run("404 not found", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{byIDErr: domain.ErrNotFound}, &fakeReservations{}, &fakeCatalog{}, false)
		h.check(t, http.MethodGet, path, "", authHeaders(custID, ""), http.StatusNotFound)
	})

	t.Run("401 without a token", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{booking: heldBooking()}, &fakeReservations{}, &fakeCatalog{}, false)
		h.check(t, http.MethodGet, path, "", nil, http.StatusUnauthorized)
	})
}

func TestCancelBookingResponsesMatchTheSpec_Issue14_AC5(t *testing.T) {
	t.Parallel()

	path := "/v1/bookings/" + bookingID + "/actions/cancel"

	t.Run("200 cancelled", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{booking: heldBooking()}, &fakeReservations{}, &fakeCatalog{}, false)
		got := h.check(t, http.MethodPost, path, `{"reason":"changed my mind"}`, authHeaders(custID, ""), http.StatusOK)
		if !strings.Contains(string(got.body), `"status":"cancelled"`) {
			t.Errorf("body does not report the cancelled status: %s", got.body)
		}
	})

	t.Run("200 with no body is allowed", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{booking: heldBooking()}, &fakeReservations{}, &fakeCatalog{}, false)
		h.check(t, http.MethodPost, path, "", authHeaders(custID, ""), http.StatusOK)
	})

	t.Run("403 another customer's booking", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{booking: heldBooking()}, &fakeReservations{}, &fakeCatalog{}, false)
		h.check(t, http.MethodPost, path, "", authHeaders(otherCustomer, ""), http.StatusForbidden)
	})

	t.Run("404 not found", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{byIDErr: domain.ErrNotFound}, &fakeReservations{}, &fakeCatalog{}, false)
		h.check(t, http.MethodPost, path, "", authHeaders(custID, ""), http.StatusNotFound)
	})

	t.Run("401 without a token", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{booking: heldBooking()}, &fakeReservations{}, &fakeCatalog{}, false)
		h.check(t, http.MethodPost, path, "", nil, http.StatusUnauthorized)
	})
}

func TestHealthProbeResponsesMatchTheSpec_Issue14_AC5(t *testing.T) {
	t.Parallel()

	t.Run("200 healthz", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{}, &fakeReservations{}, &fakeCatalog{}, false)
		h.check(t, http.MethodGet, "/healthz", "", nil, http.StatusOK)
	})

	t.Run("200 healthz stays up while a dependency is down", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{}, &fakeReservations{}, &fakeCatalog{}, true)
		h.check(t, http.MethodGet, "/healthz", "", nil, http.StatusOK)
	})

	t.Run("200 readyz", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{}, &fakeReservations{}, &fakeCatalog{}, false)
		h.check(t, http.MethodGet, "/readyz", "", nil, http.StatusOK)
	})

	t.Run("503 readyz when a dependency is down", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{}, &fakeReservations{}, &fakeCatalog{}, true)
		h.check(t, http.MethodGet, "/readyz", "", nil, http.StatusServiceUnavailable)
	})
}

// The correlation id is echoed unchanged and reappears in the error envelope.
func TestTheCorrelationIDIsEchoedAndCarriedIntoTheErrorEnvelope(t *testing.T) {
	t.Parallel()

	const corrID = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d8e"

	h := newHarness(t, &fakeStore{}, &fakeReservations{holdErr: domain.ErrSlotUnavailable}, &fakeCatalog{product: defaultProduct()}, false)
	got := h.do(t, http.MethodPost, "/v1/bookings", createBody(windowStart, windowEnd, 2), map[string]string{
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

// A genuine fault still returns the contract Error envelope, and never leaks internals. booking.yaml
// documents no 5xx on these operations except createBooking's 503, so this validates the BODY
// against the Error schema directly for the get path.
func TestAnUnexpectedFailureStillReturnsTheContractErrorEnvelope_Issue14_AC5(t *testing.T) {
	t.Parallel()

	h := newHarness(t, &fakeStore{byIDErr: errors.New("the database fell over")}, &fakeReservations{}, &fakeCatalog{}, false)
	got := h.do(t, http.MethodGet, "/v1/bookings/"+bookingID, "", authHeaders(custID, ""))

	if got.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500\nbody: %s", got.status, got.body)
	}
	assertBodyMatchesSchema(t, h.doc, "Error", got.body)
	if strings.Contains(string(got.body), "the database fell over") {
		t.Errorf("the 500 body echoes the underlying error: %s", got.body)
	}
	if !strings.Contains(string(got.body), `"code":"internal_error"`) {
		t.Errorf("the 500 body does not use the internal_error code: %s", got.body)
	}
}

func assertBodyMatchesSchema(t *testing.T, doc *openapi3.T, name string, body []byte) {
	t.Helper()

	ref, ok := doc.Components.Schemas[name]
	if !ok {
		t.Fatalf("the spec has no #/components/schemas/%s", name)
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("body is not JSON: %v\nbody: %s", err, body)
	}
	if err := ref.Value.VisitJSON(decoded); err != nil {
		t.Errorf("body does not match #/components/schemas/%s: %v\nbody: %s", name, err, body)
	}
}
