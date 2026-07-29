package obs_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"go.opentelemetry.io/otel"

	"github.com/morisempai/wakewake/shared/platform/correlation"
	"github.com/morisempai/wakewake/shared/platform/httpx"
	"github.com/morisempai/wakewake/shared/platform/logging"
	"github.com/morisempai/wakewake/shared/platform/obs"
)

// grafanaDerivedFieldRegex is the exact matcher Grafana's Loki datasource uses to link a log line
// to its trace (infra/observability/.../datasources.yml). The whole point of the bootstrap is to
// make this fire at runtime.
var grafanaDerivedFieldRegex = regexp.MustCompile(`"trace_id":"(\w+)"`)

func TestInitRequiresService(t *testing.T) {
	if _, err := obs.Init(context.Background(), obs.Config{}); err == nil {
		t.Fatal("expected error when Service is empty")
	}
}

// Without an exporter endpoint, Init must still install a recording provider: spans are created and
// valid, so trace_id reaches the logs even when the collector is down (the dev inner loop).
func TestInitRecordsWithoutExporter(t *testing.T) {
	shutdown, err := obs.Init(context.Background(), obs.Config{Service: "test-svc"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	_, span := otel.Tracer("t").Start(context.Background(), "op")
	if !span.SpanContext().IsValid() {
		t.Fatal("expected a valid (recording) span context with no exporter configured")
	}
	span.End()
}

// Init must accept the OTel-standard URL endpoint form (scheme decides TLS), not only host:port.
// Exporter construction is lazy, so this succeeds even with nothing listening.
func TestInitAcceptsURLEndpoint(t *testing.T) {
	shutdown, err := obs.Init(context.Background(), obs.Config{
		Service:  "url-svc",
		Endpoint: "http://localhost:4317",
	})
	if err != nil {
		t.Fatalf("Init with URL endpoint: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
}

// The load-bearing property: the canonical server chain (obs.Handler → correlation → log → mux)
// produces log lines carrying a trace_id that matches the Grafana derived-field regex, plus the
// correlation_id. Getting the middleware order wrong is exactly what this guards.
func TestServerChainEmitsTraceIDAndCorrelationInLogs(t *testing.T) {
	shutdown, err := obs.Init(context.Background(), obs.Config{Service: "svc"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	var buf bytes.Buffer
	log := logging.New(logging.Options{Service: "svc", Out: &buf})

	var mux http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.InfoContext(r.Context(), "handling request")
		w.WriteHeader(http.StatusOK)
	})
	handler := obs.Handler(correlation.Middleware(httpx.LogMiddleware(log)(mux)), "svc")

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/ping")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()

	out := buf.Bytes()
	if !grafanaDerivedFieldRegex.Match(out) {
		t.Fatalf("expected a trace_id matching %s in logs; got:\n%s", grafanaDerivedFieldRegex, out)
	}
	if !bytes.Contains(out, []byte(`"correlation_id"`)) {
		t.Fatalf("expected correlation_id in logs; got:\n%s", out)
	}
}

// The outbound wrapper must propagate BOTH traceparent (so the trace continues across the hop) and
// X-Correlation-Id (so the correlation id survives). A bare correlation.RoundTripper would send the
// second but not the first.
func TestRoundTripperPropagatesTraceAndCorrelation(t *testing.T) {
	shutdown, err := obs.Init(context.Background(), obs.Config{Service: "client-svc"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	var gotTraceparent, gotCorrelation string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("traceparent")
		gotCorrelation = r.Header.Get(correlation.Header)
	}))
	t.Cleanup(srv.Close)

	ctx := correlation.WithID(context.Background(), "corr-abc")
	ctx, span := otel.Tracer("t").Start(ctx, "client-call")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	client := &http.Client{Transport: obs.RoundTripper(nil)}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	span.End()

	if gotTraceparent == "" {
		t.Error("expected a traceparent header on the outbound request")
	}
	if gotCorrelation != "corr-abc" {
		t.Errorf("correlation header = %q, want %q", gotCorrelation, "corr-abc")
	}
}
