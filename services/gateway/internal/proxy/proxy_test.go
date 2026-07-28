package proxy_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/morisempai/wakewake/shared/platform/logging"

	"github.com/morisempai/wakewake/services/gateway/internal/proxy"
)

func discardLogger() *slog.Logger {
	return logging.New(logging.Options{Service: "gateway-test", Out: io.Discard})
}

func TestProxy_PassesThroughUnchanged(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotQuery  string
		gotBody   string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("X-Upstream-Marker", "catalog")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	p, err := proxy.New(upstream.URL, discardLogger())
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/products/abc?limit=10", strings.NewReader(`{"name":"boat"}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if gotMethod != http.MethodPost {
		t.Errorf("upstream method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/products/abc" {
		t.Errorf("upstream path = %q, want /v1/products/abc", gotPath)
	}
	if gotQuery != "limit=10" {
		t.Errorf("upstream query = %q, want limit=10", gotQuery)
	}
	if gotBody != `{"name":"boat"}` {
		t.Errorf("upstream body = %q", gotBody)
	}

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201 (passed through)", rec.Code)
	}
	if rec.Header().Get("X-Upstream-Marker") != "catalog" {
		t.Errorf("upstream response header did not pass through")
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Errorf("response body = %q", rec.Body.String())
	}
}

func TestProxy_UpstreamDown_502Envelope(t *testing.T) {
	// Start then immediately close a server so its address refuses connections.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()

	p, err := proxy.New(url, discardLogger())
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/products", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("502 body is not the shared envelope: %v", err)
	}
	if env.Error.Code != "bad_gateway" {
		t.Errorf("error code = %q, want bad_gateway", env.Error.Code)
	}
	// The message must not leak the upstream address or transport error.
	if strings.Contains(env.Error.Message, url) {
		t.Errorf("error message leaks the upstream address: %q", env.Error.Message)
	}
}

func TestNew_RejectsRelativeURL(t *testing.T) {
	if _, err := proxy.New("/not-absolute", discardLogger()); err == nil {
		t.Fatalf("expected an error for a non-absolute upstream URL")
	}
}
