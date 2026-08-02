package main

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

	"github.com/morisempai/wakewake/services/notification/internal/config"
)

// TestProbeHandlerEmitsTraceID pins the trace↔log correlation contract for the notification
// service's probe-only HTTP surface: a request through newProbeHandler must produce a log line
// carrying a trace_id, which is only possible when obs.Handler (otelhttp) is the outermost
// middleware and starts a server span before correlation/logging run. The regex is the Grafana
// derived-field matcher — see shared/platform/logging and ADR-0013.
func TestProbeHandlerEmitsTraceID(t *testing.T) {
	// obs.Init installs the W3C propagator and a TracerProvider so a server span is minted even
	// with no collector configured; trace_id still reaches the logs.
	shutdown, err := obs.Init(context.Background(), obs.Config{Service: config.ServiceName})
	if err != nil {
		t.Fatalf("obs.Init: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	var buf bytes.Buffer
	log := logging.New(logging.Options{Service: config.ServiceName, Out: &buf})

	checker := health.NewChecker(2 * time.Second)

	srv := httptest.NewServer(newProbeHandler(checker, log))
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz returned %d, want 200", res.StatusCode)
	}

	traceID := regexp.MustCompile(`"trace_id":"(\w+)"`)
	if !traceID.Match(buf.Bytes()) {
		t.Fatalf("probe request log line carries no trace_id; obs.Handler must be outermost so a span is in context when the log middleware runs.\nlog output:\n%s", buf.String())
	}
}
