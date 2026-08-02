package api

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

	"github.com/morisempai/wakewake/services/booking/internal/config"
	"github.com/morisempai/wakewake/services/booking/internal/domain"
)

// grafanaDerivedFieldRegex is the exact pattern Grafana's derived field uses to lift a trace_id out
// of a log line and link it to the trace. If a booking log line does not match it, the trace↔log
// jump silently breaks in the dashboard — so this test asserts the shape, not just presence.
var grafanaDerivedFieldRegex = regexp.MustCompile(`"trace_id":"(\w+)"`)

// TestRouterEmitsTraceID is the acceptance test for issue #36 (M4, ADR-0013): a request served
// through NewRouter must emit a log line carrying a trace_id, which only happens when obs.Handler
// wraps the chain as the outermost middleware and a TracerProvider is installed. /healthz is served
// by the shared health checker on the probe path — reachable without auth — so it exercises the
// middleware chain without needing a token or a live domain dependency.
func TestRouterEmitsTraceID(t *testing.T) {
	// Install the global TracerProvider and W3C propagator. With no Endpoint, spans are recorded
	// (so trace_id reaches the logs) but never exported — no collector required.
	shutdown, err := obs.Init(context.Background(), obs.Config{Service: config.ServiceName})
	if err != nil {
		t.Fatalf("obs.Init: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	var buf bytes.Buffer
	log := logging.New(logging.Options{Service: config.ServiceName, Out: &buf})

	// A minimal Server: /healthz never reaches domain logic, so a nil-backed service is enough.
	svc := domain.NewService(nil, nil, nil, time.Now, func() (string, error) { return "", nil }, 15*time.Minute)
	checker := health.NewChecker(time.Second)

	server := httptest.NewServer(NewRouter(NewServer(svc, log), checker, log))
	t.Cleanup(server.Close)

	res, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", res.StatusCode)
	}
	if !grafanaDerivedFieldRegex.Match(buf.Bytes()) {
		t.Fatalf("no trace_id in booking logs; obs.Handler not wired?\n%s", buf.String())
	}
}
