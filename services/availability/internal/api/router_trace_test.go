package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/morisempai/wakewake/shared/platform/health"
	"github.com/morisempai/wakewake/shared/platform/logging"
	"github.com/morisempai/wakewake/shared/platform/obs"

	"github.com/morisempai/wakewake/services/availability/internal/config"
)

// grafanaDerivedFieldRegex is the exact regex Grafana's derived-field configuration uses to lift a
// trace_id out of a log line and link it to the trace in Tempo. If a log line emitted while
// serving a request does not match this, trace↔log correlation is silently broken in the
// dashboard even though the trace itself exports fine. See ADR-0013.
var grafanaDerivedFieldRegex = regexp.MustCompile(`"trace_id":"(\w+)"`)

// TestRouterEmitsTraceID drives a request through the REAL NewRouter — the same handler the binary
// serves — and asserts the log line it produces carries a trace_id. That only holds if
// obs.Handler wraps the chain as the outermost middleware, so the OTel server span exists before
// the logging middleware runs. /healthz is served by the health checker and touches no domain
// logic, so a minimally-constructed Server is enough to exercise the middleware chain.
func TestRouterEmitsTraceID(t *testing.T) {
	// A TracerProvider with an active (always-sample) sampler must be installed as the global, or
	// otelhttp starts no recording span and no trace_id reaches the logs. No exporter: Endpoint is
	// empty, so spans are recorded but shipped nowhere.
	shutdown, err := obs.Init(context.Background(), obs.Config{Service: config.ServiceName})
	if err != nil {
		t.Fatalf("obs.Init: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	var buf bytes.Buffer
	log := logging.New(logging.Options{Service: config.ServiceName, Out: &buf})

	checker := health.NewChecker(2 * time.Second)
	router := NewRouter(NewServer(nil, log), checker, log)

	ts := httptest.NewServer(router)
	t.Cleanup(ts.Close)

	res, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("/healthz returned %d, want 200", res.StatusCode)
	}

	if !grafanaDerivedFieldRegex.Match(buf.Bytes()) {
		t.Fatalf("no log line matched Grafana's derived-field regex %s; got:\n%s",
			grafanaDerivedFieldRegex, buf.String())
	}
}
