package router_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/morisempai/wakewake/shared/platform/correlation"
	"github.com/morisempai/wakewake/shared/platform/health"
	"github.com/morisempai/wakewake/shared/platform/logging"

	"github.com/morisempai/wakewake/services/gateway/internal/auth"
	"github.com/morisempai/wakewake/services/gateway/internal/authtest"
	"github.com/morisempai/wakewake/services/gateway/internal/config"
	"github.com/morisempai/wakewake/services/gateway/internal/ratelimit"
	"github.com/morisempai/wakewake/services/gateway/internal/router"
)

const testIssuer = "https://issuer.test/realms/booking"

func discardLogger() *slog.Logger {
	return logging.New(logging.Options{Service: "gateway-test", Out: io.Discard})
}

// recorder is a fake upstream service. It records the last request it saw and answers with its own
// name, so a test can assert both that the right upstream was hit and what it received.
type recorder struct {
	name string
	mu   sync.Mutex
	hits int
	path string
	corr string
}

func (u *recorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hits++
		u.path = r.URL.Path
		u.corr = r.Header.Get(correlation.Header)
		u.mu.Unlock()
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, u.name)
	})
}

func (u *recorder) snapshot() (hits int, path, corr string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits, u.path, u.corr
}

type fixture struct {
	handler   http.Handler
	catalog   *recorder
	avail     *recorder
	booking   *recorder
	payment   *recorder
	issuer    *authtest.Issuer
	validAuth string
}

func newFixture(t *testing.T, rps float64, burst int) *fixture {
	t.Helper()

	f := &fixture{
		catalog: &recorder{name: "catalog"},
		avail:   &recorder{name: "availability"},
		booking: &recorder{name: "booking"},
		payment: &recorder{name: "payment"},
	}

	start := func(r *recorder) string {
		srv := httptest.NewServer(r.handler())
		t.Cleanup(srv.Close)
		return srv.URL
	}

	log := discardLogger()
	ups, err := router.BuildUpstreams(config.Upstreams{
		Catalog:      start(f.catalog),
		Availability: start(f.avail),
		Booking:      start(f.booking),
		Payment:      start(f.payment),
	}, log)
	if err != nil {
		t.Fatalf("BuildUpstreams: %v", err)
	}

	f.issuer = authtest.NewIssuer(t)
	jwks := f.issuer.StartJWKS(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	verifier, err := auth.NewVerifier(ctx, jwks.URL, testIssuer, 30*time.Second)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	checker := health.NewChecker(time.Second)
	checker.Register("jwks", verifier.Ready)

	f.handler = router.New(ups, verifier, ratelimit.New(rps, burst), checker, log)
	f.validAuth = "Bearer " + f.issuer.Sign(t, jwt.RegisteredClaims{
		Issuer:    testIssuer,
		Subject:   "user-abc",
		IssuedAt:  jwt.NewNumericDate(time.Now()),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	})
	return f
}

func (f *fixture) do(t *testing.T, method, path, auth string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func TestRouting_EachPrefixReachesItsUpstream(t *testing.T) {
	f := newFixture(t, 1000, 1000)

	tests := []struct {
		method string
		path   string
		want   *recorder
	}{
		{http.MethodGet, "/v1/products", f.catalog},
		{http.MethodGet, "/v1/products/123", f.catalog},
		{http.MethodGet, "/v1/resources", f.avail},
		{http.MethodGet, "/v1/resources/r-1", f.avail},
		{http.MethodPost, "/v1/reservations", f.avail},
		{http.MethodGet, "/v1/reservations/res-9", f.avail},
		{http.MethodGet, "/v1/bookings/b-1", f.booking},
		{http.MethodPost, "/v1/payments", f.payment},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			before, _, _ := tc.want.snapshot()
			rec := f.do(t, tc.method, tc.path, f.validAuth, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if rec.Body.String() != tc.want.name {
				t.Fatalf("reached %q, want %q", rec.Body.String(), tc.want.name)
			}
			after, gotPath, _ := tc.want.snapshot()
			if after != before+1 {
				t.Fatalf("upstream %s hit count did not increase", tc.want.name)
			}
			if gotPath != tc.path {
				t.Fatalf("upstream saw path %q, want %q", gotPath, tc.path)
			}
		})
	}
}

func TestAuth_Table(t *testing.T) {
	f := newFixture(t, 1000, 1000)
	now := time.Now()

	sign := func(c jwt.RegisteredClaims) string { return "Bearer " + f.issuer.Sign(t, c) }
	valid := jwt.RegisteredClaims{Issuer: testIssuer, Subject: "u", ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour))}

	tests := []struct {
		name       string
		auth       string
		wantStatus int
	}{
		{"valid", sign(valid), http.StatusOK},
		{"missing", "", http.StatusUnauthorized},
		{"malformed", "Bearer garbage", http.StatusUnauthorized},
		{"expired", sign(jwt.RegisteredClaims{Issuer: testIssuer, Subject: "u", ExpiresAt: jwt.NewNumericDate(now.Add(-time.Hour))}), http.StatusUnauthorized},
		{"wrong issuer", sign(jwt.RegisteredClaims{Issuer: "https://evil", Subject: "u", ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour))}), http.StatusUnauthorized},
		{"bad signature", "Bearer " + authtest.NewIssuer(t).Sign(t, valid), http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before, _, _ := f.catalog.snapshot()
			rec := f.do(t, http.MethodGet, "/v1/products", tc.auth, nil)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			after, _, _ := f.catalog.snapshot()
			if tc.wantStatus == http.StatusUnauthorized {
				if after != before {
					t.Fatalf("upstream was reached on a 401 — auth did not stop the request")
				}
				if code := errorCode(t, rec.Body.Bytes()); code != "unauthenticated" {
					t.Fatalf("401 error code = %q, want unauthenticated", code)
				}
			}
		})
	}
}

func TestWebhook_PublicBypassesAuth(t *testing.T) {
	f := newFixture(t, 1000, 1000)

	// POST with NO Authorization header proxies straight to payment.
	rec := f.do(t, http.MethodPost, "/v1/webhooks/stripe", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if hits, path, _ := f.payment.snapshot(); hits != 1 || path != "/v1/webhooks/stripe" {
		t.Fatalf("payment upstream not reached correctly: hits=%d path=%q", hits, path)
	}

	// A non-POST to the same path is not public and matches no route → 404.
	rec = f.do(t, http.MethodGet, "/v1/webhooks/stripe", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET webhook status = %d, want 404", rec.Code)
	}
}

func TestCorrelation_MintPropagateEcho(t *testing.T) {
	f := newFixture(t, 1000, 1000)

	t.Run("minted when absent", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/v1/products", f.validAuth, nil)
		echoed := rec.Header().Values(correlation.Header)
		if len(echoed) != 1 || echoed[0] == "" {
			t.Fatalf("response correlation header = %v, want exactly one non-empty value", echoed)
		}
		if _, err := uuid.Parse(echoed[0]); err != nil {
			t.Fatalf("minted correlation id %q is not a uuid: %v", echoed[0], err)
		}
		if _, _, gotCorr := f.catalog.snapshot(); gotCorr != echoed[0] {
			t.Fatalf("upstream received corr %q, response echoed %q — not propagated", gotCorr, echoed[0])
		}
	})

	t.Run("provided passes through unchanged", func(t *testing.T) {
		const id = "client-supplied-correlation-id"
		rec := f.do(t, http.MethodGet, "/v1/products", f.validAuth, map[string]string{correlation.Header: id})
		if got := rec.Header().Get(correlation.Header); got != id {
			t.Fatalf("response echoed %q, want %q", got, id)
		}
		if _, _, gotCorr := f.catalog.snapshot(); gotCorr != id {
			t.Fatalf("upstream received %q, want the client-supplied %q", gotCorr, id)
		}
	})
}

func TestRateLimit_429(t *testing.T) {
	f := newFixture(t, 1, 1) // one token, refilled at 1/s

	first := f.do(t, http.MethodGet, "/v1/products", f.validAuth, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first request = %d, want 200", first.Code)
	}
	second := f.do(t, http.MethodGet, "/v1/products", f.validAuth, nil)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429; body=%s", second.Code, second.Body.String())
	}
	if code := errorCode(t, second.Body.Bytes()); code != "rate_limited" {
		t.Fatalf("429 error code = %q, want rate_limited", code)
	}
}

func TestHealthProbes(t *testing.T) {
	f := newFixture(t, 1000, 1000)
	for _, path := range []string{"/healthz", "/readyz"} {
		rec := f.do(t, http.MethodGet, path, "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body is not the shared error envelope: %v; body=%s", err, body)
	}
	return env.Error.Code
}
