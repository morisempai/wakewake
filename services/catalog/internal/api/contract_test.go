//go:build contract

// Provider-side contract verification (AC1).
//
// Every response this service can produce is driven through the REAL router — the same handler
// cmd/catalog serves, middleware and generated parameter binding included — and validated against
// contracts/openapi/catalog.yaml itself. Not against a hand-written expectation: a struct that
// merely agrees with our own reading of the spec proves only that we are consistently wrong.
//
// No database. The store is a fake, because what is under test here is the HTTP contract, not the
// SQL — the filters and the keyset live in the integration suite, where a real Postgres can
// actually demonstrate them.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/google/uuid"

	"github.com/morisempai/wakewake/shared/contracts"
	"github.com/morisempai/wakewake/shared/platform/health"

	"github.com/morisempai/wakewake/services/catalog/internal/domain"
)

// specHost is the server URL catalog.yaml declares. Route lookup matches against it, so validation
// requests are built with this host rather than the httptest server's ephemeral address.
const specHost = "https://api.example.com"

// ---------------------------------------------------------------------------
// spec plumbing
// ---------------------------------------------------------------------------

func loadSpec(t *testing.T) (*openapi3.T, routers.Router) {
	t.Helper()

	raw, err := contracts.Specs.ReadFile(contracts.OpenAPICatalog)
	if err != nil {
		t.Fatalf("reading the embedded catalog spec: %v", err)
	}

	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(raw)
	if err != nil {
		t.Fatalf("parsing the catalog spec: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("the catalog spec is not valid OpenAPI: %v", err)
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
	rawPath string
	status  int
	header  http.Header
	body    []byte
}

// assertMatchesSpec fails unless the status is documented for this operation AND the body validates
// against its schema.
//
// IncludeResponseStatus is on, so an undocumented status — a 500 where the spec lists none — is a
// failure rather than a pass by omission.
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

// fakeStore lets each test put the service into the state that produces one documented response. It
// enforces no filtering or ordering — that is the database's job and the integration suite's
// subject.
type fakeStore struct {
	products []domain.Product
	listErr  error

	byID    domain.Product
	byIDErr error
}

func (f *fakeStore) List(_ context.Context, _ domain.Filter, _ *domain.Cursor, limit int) ([]domain.Product, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if len(f.products) > limit {
		return f.products[:limit], nil
	}
	return f.products, nil
}

func (f *fakeStore) ByID(context.Context, string) (domain.Product, error) {
	if f.byIDErr != nil {
		return domain.Product{}, f.byIDErr
	}
	return f.byID, nil
}

// validProduct is a product whose every field satisfies the contract's Product schema, so a 200
// body built from it is expected to validate. Tests vary only what they are exercising.
func validProduct(t *testing.T) domain.Product {
	t.Helper()
	desc := "A comfortable day cruiser."
	return domain.Product{
		ID:             newUUID(t),
		ResourceID:     newUUID(t),
		Type:           domain.TypeYacht,
		Name:           "Sunseeker Manhattan",
		Description:    &desc,
		Capacity:       8,
		Location:       "lake-geneva",
		BasePriceMinor: 250000,
		Currency:       "EUR",
		MediaURLs:      []string{"https://cdn.example.com/boats/1.jpg"},
		CreatedAt:      time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC),
		UpdatedAt:      time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC),
	}
}

func newUUID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return id.String()
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

	svc := domain.NewService(store)

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

func (h *harness) do(t *testing.T, method, path string, headers map[string]string) response {
	t.Helper()

	req, err := http.NewRequest(method, h.server.URL+path, http.NoBody)
	if err != nil {
		t.Fatalf("building request: %v", err)
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

// check drives the request, asserts the status, and validates the body against the spec.
func (h *harness) check(t *testing.T, method, path string, headers map[string]string, wantStatus int) response {
	t.Helper()

	got := h.do(t, method, path, headers)
	if got.status != wantStatus {
		t.Errorf("%s %s -> %d, want %d\nbody: %s", method, path, got.status, wantStatus, got.body)
	}
	assertMatchesSpec(t, h.router, got)
	return got
}

// ---------------------------------------------------------------------------
// AC1 — listProducts responses validate against the spec
// ---------------------------------------------------------------------------

func TestListProductsResponsesMatchTheSpec_Issue13_AC1(t *testing.T) {
	t.Parallel()

	t.Run("200 with products", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{products: []domain.Product{validProduct(t)}}, false)
		got := h.check(t, http.MethodGet, "/v1/products", nil, http.StatusOK)
		if !strings.Contains(string(got.body), `"next_cursor":null`) {
			t.Errorf("a single-product page must report next_cursor null: %s", got.body)
		}
	})

	// An empty catalog must serialise `data` as [] and next_cursor as null — data is a required
	// array and a nil slice would marshal to null, failing the schema.
	t.Run("200 empty catalog", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{}, false)
		got := h.check(t, http.MethodGet, "/v1/products", nil, http.StatusOK)
		if !strings.Contains(string(got.body), `"data":[]`) {
			t.Errorf("empty page did not marshal data as []: %s", got.body)
		}
	})

	// More rows than the page size means a further page exists: next_cursor must be a non-null
	// opaque string.
	t.Run("200 with a next_cursor", func(t *testing.T) {
		t.Parallel()
		many := make([]domain.Product, 5)
		for i := range many {
			many[i] = validProduct(t)
		}
		h := newHarness(t, &fakeStore{products: many}, false)
		got := h.check(t, http.MethodGet, "/v1/products?limit=2", nil, http.StatusOK)
		if strings.Contains(string(got.body), `"next_cursor":null`) {
			t.Errorf("a full page with more rows behind it must carry a non-null next_cursor: %s", got.body)
		}
	})

	badRequests := []struct {
		name string
		path string
	}{
		{"limit above the maximum", "/v1/products?limit=500"},
		{"limit below the minimum", "/v1/products?limit=0"},
		{"limit not an integer", "/v1/products?limit=lots"},
		{"unknown product type", "/v1/products?type=submarine"},
		{"min_capacity below 1", "/v1/products?min_capacity=0"},
		{"malformed cursor", "/v1/products?cursor=not-a-real-cursor"},
	}
	for _, tc := range badRequests {
		t.Run("400 "+tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, &fakeStore{}, false)
			h.check(t, http.MethodGet, tc.path, nil, http.StatusBadRequest)
		})
	}

	// A recognised type is accepted, proving the enum check rejects only genuinely bad values.
	t.Run("200 with a valid type filter", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{products: []domain.Product{validProduct(t)}}, false)
		h.check(t, http.MethodGet, "/v1/products?type=yacht&min_capacity=4&location=lake-geneva", nil, http.StatusOK)
	})
}

// ---------------------------------------------------------------------------
// AC1 + AC4 — getProduct responses validate; unknown id is 404 product_not_found
// ---------------------------------------------------------------------------

func TestGetProductResponsesMatchTheSpec_Issue13_AC1_AC4(t *testing.T) {
	t.Parallel()

	t.Run("200 found", func(t *testing.T) {
		t.Parallel()
		p := validProduct(t)
		h := newHarness(t, &fakeStore{byID: p}, false)
		got := h.check(t, http.MethodGet, "/v1/products/"+p.ID, nil, http.StatusOK)
		if !strings.Contains(string(got.body), `"id":"`+p.ID+`"`) {
			t.Errorf("body does not carry the product id: %s", got.body)
		}
	})

	// AC4: unknown id -> 404 with the shared envelope and the exact code the enum defines.
	t.Run("404 product_not_found", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{byIDErr: domain.ErrNotFound}, false)
		got := h.check(t, http.MethodGet, "/v1/products/"+newUUID(t), nil, http.StatusNotFound)
		if !strings.Contains(string(got.body), `"code":"product_not_found"`) {
			t.Errorf("404 body does not use the product_not_found code: %s", got.body)
		}
	})

	// A path parameter that is not a UUID is rejected by the generated binder as a 400, rendered
	// through the contract envelope rather than oapi-codegen's plain text.
	//
	// CONTRACT GAP, pinned as a test rather than left implicit: getProduct documents only 200, 401,
	// and 404 — no 400. A malformed product_id in the path therefore produces a correct envelope
	// under a status the spec does not list, so ValidateResponse would reject the STATUS ("status is
	// not supported") through no fault of this code. What is guaranteed, and what this pins, is that
	// the BODY is the contract's Error envelope with the validation_failed code. Raised as a
	// contract-change candidate: getProduct needs a documented 400.
	t.Run("400 product_id is not a uuid", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{}, false)
		got := h.do(t, http.MethodGet, "/v1/products/not-a-uuid", nil)

		if got.status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400\nbody: %s", got.status, got.body)
		}
		assertBodyMatchesSchema(t, h.doc, "Error", got.body)
		if !strings.Contains(string(got.body), `"code":"validation_failed"`) {
			t.Errorf("400 body does not use the validation_failed code: %s", got.body)
		}
		if _, documented := operationResponses(t, h.doc, "/v1/products/{product_id}", http.MethodGet)["400"]; documented {
			t.Error("the spec now documents a 400 on getProduct; assert it against the spec directly instead")
		}
	})
}

// ---------------------------------------------------------------------------
// AC1 — health probes
// ---------------------------------------------------------------------------

func TestHealthProbeResponsesMatchTheSpec_Issue13_AC1(t *testing.T) {
	t.Parallel()

	t.Run("200 healthz", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{}, false)
		h.check(t, http.MethodGet, "/healthz", nil, http.StatusOK)
	})

	// Liveness must NOT check dependencies. A process that reports itself dead because Postgres
	// blinked gets the container killed and restarts into the same unreachable Postgres.
	t.Run("200 healthz stays up while a dependency is down", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{}, true)
		h.check(t, http.MethodGet, "/healthz", nil, http.StatusOK)
	})

	t.Run("200 readyz", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{}, false)
		h.check(t, http.MethodGet, "/readyz", nil, http.StatusOK)
	})

	t.Run("503 readyz when a dependency is down", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, &fakeStore{}, true)
		h.check(t, http.MethodGet, "/readyz", nil, http.StatusServiceUnavailable)
	})
}

// ---------------------------------------------------------------------------
// AC6 — the correlation id is echoed and carried into the error envelope
// ---------------------------------------------------------------------------

func TestTheCorrelationIDIsEchoedAndCarriedIntoTheErrorEnvelope_Issue13_AC6(t *testing.T) {
	t.Parallel()

	const corrID = "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d8e"

	h := newHarness(t, &fakeStore{byIDErr: domain.ErrNotFound}, false)
	got := h.do(t, http.MethodGet, "/v1/products/"+newUUID(t), map[string]string{
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

func TestACorrelationIDIsMintedWhenTheCallerSendsNone_Issue13_AC6(t *testing.T) {
	t.Parallel()

	h := newHarness(t, &fakeStore{byIDErr: domain.ErrNotFound}, false)
	got := h.do(t, http.MethodGet, "/v1/products/"+newUUID(t), nil)

	if got.header.Get("X-Correlation-Id") == "" {
		t.Error("no correlation id was minted")
	}
	if strings.Contains(string(got.body), `"correlation_id":""`) {
		t.Errorf("the error envelope carries an empty correlation_id: %s", got.body)
	}
	assertMatchesSpec(t, h.router, got)
}

// ---------------------------------------------------------------------------
// CONTRACT GAP — no operation documents a 5xx
// ---------------------------------------------------------------------------

// The Error enum includes `internal_error`, but no products operation in the spec declares a 5xx
// response. So a genuine fault produces a response whose STATUS is undocumented, and
// ValidateResponse would reject it with "status is not supported" — through no fault of this code.
//
// What this service can guarantee, and what this test pins, is that the BODY is the contract's
// Error envelope with the internal_error code and no leaked internals. The missing 5xx declaration
// is raised as a contract-change candidate; when it lands, this becomes an ordinary spec check.
func TestAnUnexpectedFailureStillReturnsTheContractErrorEnvelope_Issue13_AC1(t *testing.T) {
	t.Parallel()

	h := newHarness(t, &fakeStore{listErr: errors.New("the database fell over")}, false)
	got := h.do(t, http.MethodGet, "/v1/products", nil)

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

	if _, documented := operationResponses(t, h.doc, "/v1/products", http.MethodGet)["500"]; documented {
		t.Error("the spec now documents a 500 on listProducts; fold this test back into the ordinary spec checks")
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
