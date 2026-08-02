package router_test

// Tracing seam (M4, ADR-0013): router.New must apply obs.Handler OUTERMOST, so the otelhttp
// server span has already started by the time the inner httpx.LogMiddleware emits its request log
// line. The logging handler stamps trace_id onto a record only when a valid span context is on the
// request context; if obs.Handler were missing (or nested below the log middleware) the line would
// carry no trace_id at all.
//
// The assertion uses Grafana's derived-field regex verbatim: trace_id is a contract with the
// observability stack, and a log line that does not match it will not link to its trace in Tempo.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/morisempai/wakewake/shared/platform/health"
	"github.com/morisempai/wakewake/shared/platform/logging"
	"github.com/morisempai/wakewake/shared/platform/obs"

	"github.com/morisempai/wakewake/services/gateway/internal/auth"
	"github.com/morisempai/wakewake/services/gateway/internal/authtest"
	"github.com/morisempai/wakewake/services/gateway/internal/config"
	"github.com/morisempai/wakewake/services/gateway/internal/ratelimit"
	"github.com/morisempai/wakewake/services/gateway/internal/router"
)

// grafanaDerivedFieldRegex is the exact matcher the observability stack uses to pull trace_id out
// of a JSON log line and turn it into a link to the trace.
var grafanaDerivedFieldRegex = regexp.MustCompile(`"trace_id":"(\w+)"`)

func TestNewEmitsTraceIDThroughOtelHandler(t *testing.T) {
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

	// Build the real router with the same construction the other router tests use. /healthz is
	// mounted before auth and touches no domain logic, so it exercises the full outer middleware
	// chain without needing a signed token.
	ups, err := router.BuildUpstreams(config.Upstreams{
		Catalog:      "http://catalog.invalid",
		Availability: "http://availability.invalid",
		Booking:      "http://booking.invalid",
		Payment:      "http://payment.invalid",
	}, log)
	if err != nil {
		t.Fatalf("BuildUpstreams: %v", err)
	}

	issuer := authtest.NewIssuer(t)
	jwks := issuer.StartJWKS(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	verifier, err := auth.NewVerifier(ctx, jwks.URL, testIssuer, 30*time.Second)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	checker := health.NewChecker(2 * time.Second)
	checker.Register("jwks", verifier.Ready)

	handler := router.New(ups, verifier, ratelimit.New(1000, 1000), checker, log)

	ts := httptest.NewServer(handler)
	defer ts.Close()

	res, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz: status = %d, want 200", res.StatusCode)
	}

	if !grafanaDerivedFieldRegex.MatchString(buf.String()) {
		t.Fatalf("request log line carries no trace_id matching %s — obs.Handler is not applied "+
			"outermost in router.New.\nlog output:\n%s", grafanaDerivedFieldRegex, buf.String())
	}
}
