package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"

	"github.com/morisempai/wakewake/shared/platform/correlation"
	"github.com/morisempai/wakewake/shared/platform/logging"
)

// grafanaDerivedFieldRegex is copied verbatim from the Loki datasource in
// infra/observability/grafana/provisioning/datasources/datasources.yml. If the two ever
// disagree, trace-to-log correlation breaks silently in Grafana while every test still passes —
// so this test asserts a real log line satisfies the real regex.
const grafanaDerivedFieldRegex = `"trace_id":"(\w+)"`

func TestLogLineSatisfiesGrafanaDerivedFieldRegex(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New(logging.Options{Service: "availability", Out: &buf})

	// A real span, so the emitted trace ID is a real one rather than a hand-written string that
	// happens to match. A fake would let the format drift without the test noticing.
	tp := trace.NewTracerProvider()
	defer func() { _ = tp.Shutdown(context.Background()) }()
	otel.SetTracerProvider(tp)

	ctx, span := tp.Tracer("test").Start(context.Background(), "reserve")
	defer span.End()
	ctx = correlation.WithID(ctx, "corr-abc123")

	log.InfoContext(ctx, "reservation created")

	line := buf.String()
	if !regexp.MustCompile(grafanaDerivedFieldRegex).MatchString(line) {
		t.Fatalf("log line does not satisfy Grafana's derived-field regex %s\nline: %s",
			grafanaDerivedFieldRegex, line)
	}

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log line is not valid JSON: %v\nline: %s", err, line)
	}
	for _, field := range []string{"service", "correlation_id", "trace_id", "span_id"} {
		if _, ok := rec[field]; !ok {
			t.Errorf("log line is missing required field %q: %s", field, line)
		}
	}
	if rec["service"] != "availability" {
		t.Errorf("service = %v, want availability", rec["service"])
	}
	if rec["correlation_id"] != "corr-abc123" {
		t.Errorf("correlation_id = %v, want corr-abc123", rec["correlation_id"])
	}
}

// TestNoTraceFieldsWithoutASpan documents the deliberate absence: with no active span there is no
// trace to correlate to, and emitting an empty or zero trace_id would make Grafana render a
// broken link on every line.
func TestNoTraceFieldsWithoutASpan(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New(logging.Options{Service: "catalog", Out: &buf})

	log.InfoContext(correlation.WithID(context.Background(), "corr-1"), "no span here")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if _, ok := rec["trace_id"]; ok {
		t.Errorf("trace_id emitted with no active span: %s", buf.String())
	}
	if rec["correlation_id"] != "corr-1" {
		t.Errorf("correlation_id should still be present: %s", buf.String())
	}
}

func TestLevelFromString(t *testing.T) {
	cases := []struct {
		in    string
		want  slog.Level
		known bool
	}{
		{"debug", slog.LevelDebug, true},
		{"INFO", slog.LevelInfo, true},
		{"", slog.LevelInfo, true},
		{"warn", slog.LevelWarn, true},
		{"warning", slog.LevelWarn, true},
		{"error", slog.LevelError, true},
		{"nonsense", slog.LevelInfo, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, known := logging.LevelFromString(tc.in)
			if got != tc.want {
				t.Errorf("level = %v, want %v", got, tc.want)
			}
			if known != tc.known {
				t.Errorf("known = %v, want %v", known, tc.known)
			}
		})
	}
}
