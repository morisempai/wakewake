package api

// Tracing seam (M4, ADR-0013): the router must apply obs.Handler OUTERMOST, so the otelhttp
// server span has already started by the time the inner httpx.LogMiddleware emits its request
// log line. The logging handler stamps trace_id onto a record only when a valid span context is
// on the request context; if obs.Handler were missing (or nested below the log middleware) the
// line would carry no trace_id at all. This also gives the inbound Stripe webhook a server span,
// so webhook logs carry a trace_id too.
//
// The assertion uses Grafana's derived-field regex verbatim: trace_id is a contract with the
// observability stack, and a log line that does not match it will not link to its trace in Tempo.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/morisempai/wakewake/services/payment/internal/config"
	"github.com/morisempai/wakewake/shared/platform/health"
	"github.com/morisempai/wakewake/shared/platform/logging"
	"github.com/morisempai/wakewake/shared/platform/obs"
)

// grafanaDerivedFieldRegex is the exact matcher the observability stack uses to pull trace_id out
// of a JSON log line and turn it into a link to the trace.
var grafanaDerivedFieldRegex = regexp.MustCompile(`"trace_id":"(\w+)"`)

func TestNewRouterEmitsTraceIDThroughOtelHandler(t *testing.T) {
	// Install the process-wide TracerProvider + W3C propagator. Endpoint is empty on purpose:
	// spans are recorded (so trace_id reaches the logs) but nothing is exported, which is exactly
	// the state this test wants — no collector, just the id threading.
	shutdown, err := obs.Init(context.Background(), obs.Config{Service: config.ServiceName})
	if err != nil {
		t.Fatalf("obs.Init: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	// Capture the request log line the inner LogMiddleware writes.
	var buf bytes.Buffer
	log := logging.New(logging.Options{Service: config.ServiceName, Out: &buf})

	// /healthz is served by the health checker and never touches domain logic, so a Server with a
	// nil service is enough to exercise the full middleware chain. The webhook is mounted on its
	// own POST route and is never reached by the unauthenticated /healthz probe, so a nil handler
	// is safe here — it is not invoked.
	checker := health.NewChecker(2 * time.Second)
	handler := NewRouter(NewServer(nil, log), (*WebhookHandler)(nil), checker, log)

	ts := httptest.NewServer(handler)
	defer ts.Close()

	res, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz: status = %d, want 200", res.StatusCode)
	}

	if !grafanaDerivedFieldRegex.MatchString(buf.String()) {
		t.Fatalf("request log line carries no trace_id matching %s — obs.Handler is not applied "+
			"outermost in NewRouter.\nlog output:\n%s", grafanaDerivedFieldRegex, buf.String())
	}
}
