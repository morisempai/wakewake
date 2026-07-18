// Package logging provides the structured JSON logger every service uses.
//
// The field names here are not a style preference. Grafana's Loki datasource in
// infra/observability/grafana/provisioning/datasources/datasources.yml declares a derived field
// with matcherRegex `"trace_id":"(\w+)"`, which is what links a log line to its trace. A service
// that emits `traceId` or nests the value produces logs that look perfectly fine, pass every
// test, and silently cannot be correlated with traces in Grafana.
//
// That regex makes `trace_id` a contract with the observability stack. Treat it as one.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/morisempai/wakewake/shared/platform/correlation"
)

// Field names. Kept as constants so a typo is a compile error rather than a silently
// uncorrelatable log line.
const (
	FieldService       = "service"
	FieldCorrelationID = "correlation_id"
	FieldTraceID       = "trace_id"
	FieldSpanID        = "span_id"
)

// Options configures the logger.
type Options struct {
	// Service is emitted on every line. Required — a log aggregator with unlabelled lines is
	// a text file with extra steps.
	Service string

	// Level defaults to info. debug is expected to be off in production.
	Level slog.Level

	// Out defaults to os.Stdout. Container logs go to stdout; the collector ships them.
	Out io.Writer

	// AddSource includes file:line. Off by default: it is measurably expensive and rarely the
	// thing you need when the correlation ID already tells you the story.
	AddSource bool
}

// New builds the service logger. The returned logger emits JSON with `service` on every line and
// enriches each record with `correlation_id`, `trace_id`, and `span_id` from the context.
func New(o Options) *slog.Logger {
	out := o.Out
	if out == nil {
		out = os.Stdout
	}
	base := slog.NewJSONHandler(out, &slog.HandlerOptions{
		Level:     o.Level,
		AddSource: o.AddSource,
	})
	h := &contextHandler{Handler: base}
	return slog.New(h).With(slog.String(FieldService, o.Service))
}

// LevelFromString parses a level name, defaulting to info for anything unrecognised. It defaults
// rather than erroring because a typo in LOG_LEVEL should not stop a service from booting — but
// it is deliberately not silent about it.
func LevelFromString(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, true
	case "", "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

// contextHandler copies the correlation ID and the active span's IDs onto every record.
//
// Doing it here rather than at each call site is the whole point: a handler that must remember
// to attach the correlation ID will eventually forget, and the line that forgets is the one you
// need during an incident.
type contextHandler struct {
	slog.Handler
}

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := correlation.FromContext(ctx); id != "" {
		r.AddAttrs(slog.String(FieldCorrelationID, id))
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		// Lowercase hex, which is what the Grafana derived-field regex `(\w+)` expects and what
		// Tempo stores. TraceID.String() already produces that form.
		r.AddAttrs(
			slog.String(FieldTraceID, sc.TraceID().String()),
			slog.String(FieldSpanID, sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithGroup(name)}
}
