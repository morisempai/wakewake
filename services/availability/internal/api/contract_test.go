//go:build contract

// Provider-side contract verification (AC5).
//
// Every response this service can produce is driven through the REAL router — the same handler
// cmd/availability serves, middleware and generated parameter binding included — and validated
// against contracts/openapi/availability.yaml itself. Not against a hand-written expectation:
// a struct that merely agrees with our own reading of the spec proves only that we are
// consistently wrong.
//
// No database. The store is a fake, because what is under test here is the HTTP contract, not
// the exclusion constraint — that lives in the integration suite, where a real Postgres can
// actually demonstrate it.
package api

import (
	"bytes"
	"context"
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

	"github.com/morisempai/wakewake/services/availability/internal/domain"
)

// specHost is the server URL the spec declares. Route lookup matches against it, so validation
// requests are built with this host rather than the httptest server's ephemeral address.
const specHost = "http://availability:3000"

const (
	resourceID    = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d80"
	bookingID     = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d81"
	reservationID = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d82"
	idempKey      = "idem-key-0123456789"
)

var (
	windowStart = time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	windowEnd   = time.Date(2026, 7, 20, 11, 0, 0, 0, time.UTC)
	stamp       = time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
)

// ---------------------------------------------------------------------------
// spec plumbing
// ---------------------------------------------------------------------------

func loadSpec(t *testing.T) (*openapi3.T, routers.Router) {
	t.Helper()

	raw, err := contracts.Specs.ReadFile(contracts.OpenAPIAvailability)
	if err != nil {
		t.Fatalf("reading the embedded availability spec: %v", err)
	}

	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(raw)
	if err != nil {
		t.Fatalf("parsing the availability spec: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("the availability spec is not valid OpenAPI: %v", err)
	}

	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatalf("building a router from the spec: %v", err)
	}
	return doc, router
}

// response is one recorded exchange, ready to be checked against the spec.
type response struct {
	method  string
	path    string
	status  int
	header  http.Header
	body    []byte
	rawPath string
}

// assertMatchesSpec fails unless the status is documented for this operation AND the body
// validates against its schema.
//
// IncludeResponseStatus is on, so an undocumented status — a 500 where the spec lists none, a
// 409 on an operation that has no conflict case — is a failure rather than a pass by omission.
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
				// The spec declares bearerAuth, but no gateway issues tokens yet (M3) and this
				// service is internal-only per ADR-0006. Authentication is not what this test
				// verifies, so it is stubbed rather than silently ignored.
				AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
			},
		},
		Status:  r.status,
		Header:  r.header,
		Body:    io.NopCloser(bytes.NewReader(r.body)),
		Options: &openapi3filter.Options{IncludeResponseStatus: true},
	})
	if err != nil {
		t.Errorf("%s %s -> %d does not match the spec: %v\nbody: %s",
			r.method, r.rawPath, r.status, err, r.body)
	}
}

// ---------------------------------------------------------------------------
// fake store
// ---------------------------------------------------------------------------

// fakeStore lets each test put the service into the state that produces one documented response.
// It enforces no invariants — that is the database's job and the integration suite's subject.
type fakeStore struct {
	insertErr error
	byIDErr   error
	applyErr  error
	busyErr   error

	reservation domain.Reservation
	windows     []domain.Window
}

func (f *fakeStore) Insert(_ context.Context, _ domain.Claim, r domain.Reservation, ttl time.Duration) (domain.Reservation, error) {
	if f.insertErr != nil {
		return domain.Reservation{}, f.insertErr
	}
	expiry := stamp.Add(ttl)
	out := r
	out.ExpiresAt = &expiry
	out.CreatedAt = stamp
	out.UpdatedAt = stamp
	return out, nil
}

func (f *fakeStore) ByID(context.Context, string) (domain.Reservation, error) {
	if f.byIDErr != nil {
		return domain.Reservation{}, f.byIDErr
	}
	return f.reservation, nil
}

func (f *fakeStore) Apply(_ context.Context, _ string, fn domain.Transition) (domain.Reservation, bool, error) {
	if f.applyErr != nil {
		return domain.Reservation{}, false, f.applyErr
	}
	next, emissions, err := fn(f.reservation)
	if err != nil {
		return domain.Reservation{}, false, err
	}
	return next, len(emissions) > 0, nil
}

func (f *fakeStore) Busy(context.Context, string, time.Time, time.Time) ([]domain.Window, error) {
	if f.busyErr != nil {
		return nil, f.busyErr
	}
	if f.windows == nil {
		return []domain.Window{}, nil
	}
	return f.windows, nil
}

func (f *fakeStore) Now(context.Context) (time.Time, error) { return stamp, nil }

func (f *fakeStore) ExpiredHolds(context.Context, int) ([]string, error) { return nil, nil }

func heldReservation() domain.Reservation {
	expiry := stamp.Add(15 * time.Minute)
	return domain.Reservation{
		ID:         reservationID,
		ResourceID: resourceID,
		BookingID:  bookingID,
		StartsAt:   windowStart,
		EndsAt:     windowEnd,
		Status:     domain.StatusHeld,
		ExpiresAt:  &expiry,
		CreatedAt:  stamp,
		UpdatedAt:  stamp,
	}
}

func releasedReservation() domain.Reservation {
	reason := domain.ReasonPaymentFailed
	r := heldReservation()
	r.Status = domain.StatusReleased
	r.ExpiresAt = nil
	r.ReleasedReason = &reason
	return r
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type harness struct {
	server *httptest.Server
	router routers.Router
	doc    *openapi3.T
}

func newHarness(t *testing.T, store domain.Store, readinessFails bool) *harness {
	t.Helper()

	doc, specRouter := loadSpec(t)

	svc := domain.NewService(store, func() time.Time { return stamp },
		func() (string, error) { return reservationID, nil }, 15*time.Minute)

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

// do issues a request and records the response for validation.
func (h *harness) do(t *testing.T, method, path string, body string, headers map[string]string) response {
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

	return response{
		method:  method,
		path:    path,
		rawPath: path,
		status:  res.StatusCode,
		header:  res.Header,
		body:    raw,
	}
}

// check drives the request, asserts the status, and validates the body against the spec.
func (h *harness) check(t *testing.T, method, path, body string, headers map[string]string, wantStatus int) response {
	t.Helper()

	got := h.do(t, method, path, body, headers)
	if got.status != wantStatus {
		t.Errorf("%s %s -> %d, want %d\nbody: %s", method, path, got.status, wantStatus, got.body)
	}
	assertMatchesSpec(t, h.router, got)
	return got
}

func reserveBody(startsAt, endsAt time.Time) string {
	return fmt.Sprintf(
		`{"resource_id":%q,"booking_id":%q,"starts_at":%q,"ends_at":%q}`,
		resourceID, bookingID,
		startsAt.Format(time.RFC3339), endsAt.Format(time.RFC3339))
}

func withKey(key string) map[string]string { return map[string]string{"Idempotency-Key": key} }

// ---------------------------------------------------------------------------
// AC5 — every documented response validates against the spec
// ---------------------------------------------------------------------------

func TestCreateReservationResponsesMatchTheSpec_Issue5_AC5(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		store   *fakeStore
		body    string
		headers map[string]string
		want    int
	}{
		{
			name:    "201 reserved",
			store:   &fakeStore{},
			body:    reserveBody(windowStart, windowEnd),
			headers: withKey(idempKey),
			want:    http.StatusCreated,
		},
		{
			name:    "409 reservation_overlap",
			store:   &fakeStore{insertErr: domain.ErrSlotUnavailable},
			body:    reserveBody(windowStart, windowEnd),
			headers: withKey(idempKey),
			want:    http.StatusConflict,
		},
		{
			name:    "409 idempotency_key_reuse",
			store:   &fakeStore{insertErr: domain.ErrIdempotencyKeyReuse},
			body:    reserveBody(windowStart, windowEnd),
			headers: withKey(idempKey),
			want:    http.StatusConflict,
		},
		{
			name:    "422 invalid_time_window",
			store:   &fakeStore{},
			body:    reserveBody(windowEnd, windowStart),
			headers: withKey(idempKey),
			want:    http.StatusUnprocessableEntity,
		},
		{
			name:    "400 missing Idempotency-Key",
			store:   &fakeStore{},
			body:    reserveBody(windowStart, windowEnd),
			headers: nil,
			want:    http.StatusBadRequest,
		},
		{
			name:    "400 Idempotency-Key below the spec's minLength",
			store:   &fakeStore{},
			body:    reserveBody(windowStart, windowEnd),
			headers: withKey("short"),
			want:    http.StatusBadRequest,
		},
		{
			name:    "400 malformed JSON body",
			store:   &fakeStore{},
			body:    `{"resource_id": not-json`,
			headers: withKey(idempKey),
			want:    http.StatusBadRequest,
		},
		{
			name:    "400 resource_id is not a uuid",
			store:   &fakeStore{},
			body:    `{"resource_id":"not-a-uuid","booking_id":"` + bookingID + `","starts_at":"2026-07-20T10:00:00Z","ends_at":"2026-07-20T11:00:00Z"}`,
			headers: withKey(idempKey),
			want:    http.StatusBadRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tc.store, false)
			h.check(t, http.MethodPost, "/v1/reservations", tc.body, tc.headers, tc.want)
		})
	}
}

func TestGetReservationResponsesMatchTheSpec_Issue5_AC5(t *testing.T) {
	t.Parallel()

	t.Run("200 found", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{reservation: heldReservation()}, false)
		h.check(t, http.MethodGet, "/v1/reservations/"+reservationID, "", nil, http.StatusOK)
	})

	t.Run("200 released carries released_reason", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{reservation: releasedReservation()}, false)
		got := h.check(t, http.MethodGet, "/v1/reservations/"+reservationID, "", nil, http.StatusOK)
		if !strings.Contains(string(got.body), `"released_reason":"payment_failed"`) {
			t.Errorf("released reservation omitted released_reason: %s", got.body)
		}
	})

	t.Run("404 reservation_not_found", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{byIDErr: domain.ErrNotFound}, false)
		h.check(t, http.MethodGet, "/v1/reservations/"+reservationID, "", nil, http.StatusNotFound)
	})
}

func TestConfirmReservationResponsesMatchTheSpec_Issue5_AC5(t *testing.T) {
	t.Parallel()

	path := "/v1/reservations/" + reservationID + "/actions/confirm"

	t.Run("200 confirmed", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{reservation: heldReservation()}, false)
		got := h.check(t, http.MethodPost, path, "", nil, http.StatusOK)
		if !strings.Contains(string(got.body), `"status":"confirmed"`) {
			t.Errorf("body does not report the confirmed status: %s", got.body)
		}
	})

	t.Run("404 reservation_not_found", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{applyErr: domain.ErrNotFound}, false)
		h.check(t, http.MethodPost, path, "", nil, http.StatusNotFound)
	})

	t.Run("422 reservation_already_released", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{reservation: releasedReservation()}, false)
		h.check(t, http.MethodPost, path, "", nil, http.StatusUnprocessableEntity)
	})
}

func TestReleaseReservationResponsesMatchTheSpec_Issue5_AC5(t *testing.T) {
	t.Parallel()

	path := "/v1/reservations/" + reservationID + "/actions/release"

	t.Run("200 with a reason", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{reservation: heldReservation()}, false)
		got := h.check(t, http.MethodPost, path, `{"reason":"payment_failed"}`, nil, http.StatusOK)
		if !strings.Contains(string(got.body), `"released_reason":"payment_failed"`) {
			t.Errorf("body does not carry the requested reason: %s", got.body)
		}
	})

	// The spec marks the request body `required: false`, so this must not be rejected.
	t.Run("200 with no body at all", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{reservation: heldReservation()}, false)
		h.check(t, http.MethodPost, path, "", nil, http.StatusOK)
	})

	t.Run("200 releasing an already-released reservation is idempotent", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{reservation: releasedReservation()}, false)
		h.check(t, http.MethodPost, path, `{"reason":"booking_cancelled"}`, nil, http.StatusOK)
	})

	t.Run("404 reservation_not_found", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{applyErr: domain.ErrNotFound}, false)
		h.check(t, http.MethodPost, path, `{"reason":"hold_expired"}`, nil, http.StatusNotFound)
	})
}

func TestListSlotsResponsesMatchTheSpec_Issue5_AC5(t *testing.T) {
	t.Parallel()

	base := "/v1/resources/" + resourceID + "/slots"

	t.Run("200 with busy windows", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{windows: []domain.Window{{StartsAt: windowStart, EndsAt: windowEnd}}}, false)
		h.check(t, http.MethodGet, base+"?from=2026-07-20T00:00:00Z&to=2026-07-21T00:00:00Z", "", nil, http.StatusOK)
	})

	// An empty result must marshal as [] — `busy` is a required array and null fails the schema.
	t.Run("200 with nothing busy", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{}, false)
		got := h.check(t, http.MethodGet, base+"?from=2026-07-20T00:00:00Z&to=2026-07-21T00:00:00Z", "", nil, http.StatusOK)
		if !strings.Contains(string(got.body), `"busy":[]`) {
			t.Errorf("empty result did not marshal as []: %s", got.body)
		}
	})

	t.Run("400 to is not after from", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{}, false)
		h.check(t, http.MethodGet, base+"?from=2026-07-21T00:00:00Z&to=2026-07-20T00:00:00Z", "", nil, http.StatusBadRequest)
	})

	t.Run("400 from is missing", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{}, false)
		h.check(t, http.MethodGet, base+"?to=2026-07-21T00:00:00Z", "", nil, http.StatusBadRequest)
	})

	t.Run("400 from is not RFC 3339", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{}, false)
		h.check(t, http.MethodGet, base+"?from=yesterday&to=2026-07-21T00:00:00Z", "", nil, http.StatusBadRequest)
	})
}

func TestHealthProbeResponsesMatchTheSpec_Issue5_AC5(t *testing.T) {
	t.Parallel()

	t.Run("200 healthz", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{}, false)
		h.check(t, http.MethodGet, "/healthz", "", nil, http.StatusOK)
	})

	// Liveness must NOT check dependencies. A process that reports itself dead because Postgres
	// blinked gets the container killed, loses its warm pool, and restarts into the same
	// unreachable Postgres — a database blip turned into a crash loop.
	t.Run("200 healthz stays up while a dependency is down", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{}, true)
		h.check(t, http.MethodGet, "/healthz", "", nil, http.StatusOK)
	})

	t.Run("200 readyz", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{}, false)
		h.check(t, http.MethodGet, "/readyz", "", nil, http.StatusOK)
	})

	t.Run("503 readyz when a dependency is down", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{}, true)
		h.check(t, http.MethodGet, "/readyz", "", nil, http.StatusServiceUnavailable)
	})
}

// The correlation ID is echoed unchanged and reappears in the error envelope, so a customer
// quoting the id from an error page lands on the exact request in the logs.
func TestTheCorrelationIDIsEchoedAndCarriedIntoTheErrorEnvelope(t *testing.T) {
	t.Parallel()

	const corrID = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d8e"

	h := newHarness(t, &fakeStore{insertErr: domain.ErrSlotUnavailable}, false)
	got := h.do(t, http.MethodPost, "/v1/reservations", reserveBody(windowStart, windowEnd), map[string]string{
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

// A request with no correlation ID gets one minted at the edge, so no error envelope is ever
// emitted with an empty correlation_id — which the schema requires to be present.
func TestACorrelationIDIsMintedWhenTheCallerSendsNone(t *testing.T) {
	t.Parallel()

	h := newHarness(t, &fakeStore{insertErr: domain.ErrSlotUnavailable}, false)
	got := h.do(t, http.MethodPost, "/v1/reservations", reserveBody(windowStart, windowEnd), withKey(idempKey))

	if got.header.Get("X-Correlation-Id") == "" {
		t.Error("no correlation id was minted")
	}
	if strings.Contains(string(got.body), `"correlation_id":""`) {
		t.Errorf("the error envelope carries an empty correlation_id: %s", got.body)
	}
	assertMatchesSpec(t, h.router, got)
}

// CONTRACT GAP, documented as a test rather than left implicit.
//
// The Error enum includes `internal_error`, but no operation in the spec declares a 5xx
// response. So a genuine fault produces a response whose STATUS is undocumented, and
// ValidateResponse rejects it with "status is not supported" — through no fault of this code.
//
// What this service can guarantee, and what this test pins, is that the BODY is the contract's
// Error envelope. The missing 5xx declaration is raised as a contract-change candidate; when it
// lands, this test should become an ordinary assertMatchesSpec case.
func TestAnUnexpectedFailureStillReturnsTheContractErrorEnvelope_Issue5_AC5(t *testing.T) {
	t.Parallel()

	h := newHarness(t, &fakeStore{insertErr: errors.New("the database fell over")}, false)
	got := h.do(t, http.MethodPost, "/v1/reservations", reserveBody(windowStart, windowEnd), withKey(idempKey))

	if got.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500\nbody: %s", got.status, got.body)
	}

	assertBodyMatchesSchema(t, h.doc, "Error", got.body)

	// 5xx bodies must not leak internals — no driver text, no stack traces.
	if strings.Contains(string(got.body), "the database fell over") {
		t.Errorf("the 500 body echoes the underlying error: %s", got.body)
	}
	if !strings.Contains(string(got.body), `"code":"internal_error"`) {
		t.Errorf("the 500 body does not use the internal_error code: %s", got.body)
	}

	// And the documented statuses on this operation genuinely exclude 500, which is the gap.
	if _, documented := operationResponses(t, h.doc, "/v1/reservations", http.MethodPost)["500"]; documented {
		t.Error("the spec now documents a 500 on createReservation; fold this test back into the ordinary spec checks")
	}
}

// assertBodyMatchesSchema validates a body against a named component schema, for the one case
// where the status itself is undocumented.
func assertBodyMatchesSchema(t *testing.T, doc *openapi3.T, name string, body []byte) {
	t.Helper()

	ref, ok := doc.Components.Schemas[name]
	if !ok {
		t.Fatalf("the spec has no #/components/schemas/%s", name)
	}

	var decoded any
	if err := decodeJSON(body, &decoded); err != nil {
		t.Fatalf("body is not JSON: %v\nbody: %s", err, body)
	}
	if err := ref.Value.VisitJSON(decoded); err != nil {
		t.Errorf("body does not match #/components/schemas/%s: %v\nbody: %s", name, err, body)
	}
}

func operationResponses(t *testing.T, doc *openapi3.T, path, method string) map[string]*openapi3.ResponseRef {
	t.Helper()

	item := doc.Paths.Find(path)
	if item == nil {
		t.Fatalf("the spec has no path %s", path)
	}
	op := item.GetOperation(method)
	if op == nil {
		t.Fatalf("the spec has no %s on %s", method, path)
	}
	return op.Responses.Map()
}

// decodeJSON is a thin wrapper so the schema check reads cleanly above.
func decodeJSON(raw []byte, into any) error {
	return json.Unmarshal(raw, into)
}
