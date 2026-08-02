package httpx_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"

	"github.com/morisempai/wakewake/shared/platform/correlation"
	"github.com/morisempai/wakewake/shared/platform/httpx"
	"github.com/morisempai/wakewake/shared/platform/obs"
)

// NewClient is the one outbound HTTP client every internal service-to-service call goes through
// (booking→availability, booking→catalog, …). For traces to continue across those hops it must
// propagate BOTH W3C trace context (traceparent) and the X-Correlation-Id header. A client that
// sends only the correlation id keeps the human-facing id threaded but breaks the machine trace at
// the first internal hop — exactly the silent failure ADR-0013 exists to prevent.
func TestNewClientPropagatesTraceAndCorrelation(t *testing.T) {
	shutdown, err := obs.Init(context.Background(), obs.Config{Service: "httpx-test"})
	if err != nil {
		t.Fatalf("obs.Init: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	var gotTraceparent, gotCorrelation string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("traceparent")
		gotCorrelation = r.Header.Get(correlation.Header)
	}))
	t.Cleanup(srv.Close)

	ctx := correlation.WithID(context.Background(), "corr-xyz")
	ctx, span := otel.Tracer("t").Start(ctx, "outbound-call")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := httpx.NewClient(httpx.ClientConfig{}).Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	span.End()

	if gotTraceparent == "" {
		t.Error("expected a traceparent header on the outbound request (trace broke at the hop)")
	}
	if gotCorrelation != "corr-xyz" {
		t.Errorf("correlation header = %q, want %q", gotCorrelation, "corr-xyz")
	}
}
