package ratelimit_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/morisempai/wakewake/shared/platform/logging"

	"github.com/morisempai/wakewake/services/gateway/internal/auth"
	"github.com/morisempai/wakewake/services/gateway/internal/ratelimit"
)

func discardLogger() *slog.Logger {
	return logging.New(logging.Options{Service: "gateway-test", Out: io.Discard})
}

func TestLimiter_AllowBurstThenDeny(t *testing.T) {
	l := ratelimit.New(1, 2) // 1 rps, burst 2

	if !l.Allow("k") || !l.Allow("k") {
		t.Fatalf("first two requests within burst should be allowed")
	}
	if l.Allow("k") {
		t.Fatalf("third request should be denied once the burst is spent")
	}
	// A different key has its own independent bucket.
	if !l.Allow("other") {
		t.Fatalf("a different key must not be affected by another key's usage")
	}
}

func TestMiddleware_429Envelope(t *testing.T) {
	l := ratelimit.New(1, 1)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := ratelimit.Middleware(l, discardLogger())(next)

	newReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/products", nil)
		r.RemoteAddr = "203.0.113.7:44444"
		return r
	}

	first := httptest.NewRecorder()
	h.ServeHTTP(first, newReq())
	if first.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", first.Code)
	}

	second := httptest.NewRecorder()
	h.ServeHTTP(second, newReq())
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", second.Code)
	}
	if second.Header().Get("Retry-After") == "" {
		t.Errorf("429 should carry a Retry-After header")
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &env); err != nil {
		t.Fatalf("429 body is not the shared envelope: %v", err)
	}
	if env.Error.Code != "rate_limited" {
		t.Errorf("error code = %q, want rate_limited", env.Error.Code)
	}
}

// TestMiddleware_KeysOnSubject shows two requests from the SAME subject but different IPs share a
// bucket (identity, not address, is the key), while a different subject is independent.
func TestMiddleware_KeysOnSubject(t *testing.T) {
	l := ratelimit.New(1, 1)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := ratelimit.Middleware(l, discardLogger())(next)

	reqAs := func(sub, addr string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/v1/products", nil)
		r.RemoteAddr = addr
		r = r.WithContext(auth.WithSubject(r.Context(), sub))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	if code := reqAs("alice", "10.0.0.1:1").Code; code != http.StatusOK {
		t.Fatalf("alice first request = %d, want 200", code)
	}
	if code := reqAs("alice", "10.0.0.2:2").Code; code != http.StatusTooManyRequests {
		t.Fatalf("alice second request (new IP, same sub) = %d, want 429", code)
	}
	if code := reqAs("bob", "10.0.0.1:1").Code; code != http.StatusOK {
		t.Fatalf("bob first request = %d, want 200 (independent bucket)", code)
	}
}
