package proxy_test

// Tracing seam (M4, ADR-0013): the reverse-proxy transport must be obs.RoundTripper, which layers
// otelhttp over the correlation RoundTripper. A bare correlation.RoundTripper stamps only the
// X-Correlation-Id header onto the upstream request and BREAKS the trace at the first hop — the
// upstream sees no traceparent and starts a fresh, disconnected trace. This test asserts BOTH
// headers reach the upstream, so the trace continues into internal services.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/morisempai/wakewake/shared/platform/correlation"
	"github.com/morisempai/wakewake/shared/platform/obs"

	"github.com/morisempai/wakewake/services/gateway/internal/proxy"
)

func TestProxy_PropagatesTraceparentAndCorrelation(t *testing.T) {
	// A recording TracerProvider must be installed for otelhttp to mint a client span and inject a
	// valid traceparent. Endpoint empty: spans are recorded, nothing is exported.
	shutdown, err := obs.Init(context.Background(), obs.Config{Service: "gateway-test"})
	if err != nil {
		t.Fatalf("obs.Init: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	var gotTraceparent, gotCorrelation string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("traceparent")
		gotCorrelation = r.Header.Get(correlation.Header)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p, err := proxy.New(upstream.URL, discardLogger())
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Seed a correlation id on the request context, as correlation.Middleware does at the edge, so
	// the correlation RoundTripper inside obs.RoundTripper has an id to propagate.
	const corrID = "test-correlation-id"
	req := httptest.NewRequest(http.MethodGet, "/v1/products", nil)
	req = req.WithContext(correlation.WithID(req.Context(), corrID))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// The correlation id has always ridden along; keep asserting it so the obs.RoundTripper swap
	// did not regress it.
	if gotCorrelation != corrID {
		t.Errorf("upstream received %s = %q, want %q — correlation id was not propagated", correlation.Header, gotCorrelation, corrID)
	}
	// The traceparent is the new contract: it is absent with the old correlation.RoundTripper and
	// present with obs.RoundTripper.
	if gotTraceparent == "" {
		t.Fatalf("upstream received no traceparent header — the proxy transport is not " +
			"obs.RoundTripper, so the trace breaks at the gateway hop")
	}
}
