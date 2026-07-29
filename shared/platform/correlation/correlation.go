// Package correlation carries the correlation ID across HTTP calls, events, logs, and spans.
//
// A correlation ID is minted once at the edge and then propagated unchanged for the life of the
// request — through the gateway, into each service, onto every event the request produces, and
// out to every log line and span. It is what makes a distributed booking flow readable as one
// story rather than five unrelated ones.
//
// It lives in context.Context rather than being threaded as a parameter because it must survive
// every intermediate call, including ones that have no business knowing about it.
package correlation

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Header is the wire name, matching the X-Correlation-Id parameter in every OpenAPI spec.
const Header = "X-Correlation-Id"

// MaxLen mirrors the contract's maxLength: 128. A caller-supplied value longer than this is
// truncated rather than rejected — the ID is for tracing, and failing a booking because someone
// sent a long header would be a self-inflicted outage.
const MaxLen = 128

type ctxKey struct{}

// NewID mints a correlation ID. UUIDv7 so IDs sort by time, which makes a log search for "what
// else happened around this request" a range scan rather than a guess.
func NewID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	// NewV7 fails only if the system entropy source fails. A degraded ID beats no ID: losing
	// traceability is worse than losing time-ordering.
	return uuid.NewString()
}

// WithID returns a context carrying the correlation ID, truncated to MaxLen.
func WithID(ctx context.Context, id string) context.Context {
	if len(id) > MaxLen {
		id = id[:MaxLen]
	}
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the correlation ID, or "" if the context carries none.
//
// Callers that need one should use Ensure instead. This returning "" is a signal that a code
// path forgot to propagate, which is worth seeing in logs rather than silently papering over.
func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// Ensure returns the context's correlation ID, minting and attaching one if absent. Use it at
// entry points that may be reached without an upstream ID — a scheduled sweeper, a consumer
// handling an event whose envelope lost its ID, a CLI.
func Ensure(ctx context.Context) (context.Context, string) {
	if id := FromContext(ctx); id != "" {
		return ctx, id
	}
	id := NewID()
	return WithID(ctx, id), id
}

// Middleware takes the correlation ID from the incoming request, or mints one, and echoes it on
// the response so a client can quote it in a bug report.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(Header)
		if id == "" {
			id = NewID()
		}
		ctx := WithID(r.Context(), id)
		// If a tracing middleware is outside this one, the server span is already on the context
		// (ADR-0013 mandates that order). Tag it so Tempo can pivot from a trace to its logs by
		// correlation id, the reverse of the log→trace derived field.
		if span := trace.SpanFromContext(ctx); span.IsRecording() {
			span.SetAttributes(attribute.String("correlation_id", FromContext(ctx)))
		}
		w.Header().Set(Header, FromContext(ctx))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RoundTripper propagates the correlation ID onto outgoing requests. Wrap any http.Client that
// talks to another service with this, or the chain breaks at the first hop that forgets.
type RoundTripper struct {
	Base http.RoundTripper
}

func (rt RoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	base := rt.Base
	if base == nil {
		base = http.DefaultTransport
	}
	if id := FromContext(r.Context()); id != "" && r.Header.Get(Header) == "" {
		// Clone before mutating: the caller may reuse or retry the original request, and
		// mutating a shared *http.Request is a data race.
		r = r.Clone(r.Context())
		r.Header.Set(Header, id)
	}
	return base.RoundTrip(r)
}
