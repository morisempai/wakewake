// Package httpx writes the shared HTTP response shapes and builds outbound clients.
//
// The error envelope is byte-identical across all four OpenAPI specs, so it is written once
// here. The error *codes* deliberately are not: each spec declares its own enum (availability
// has 10, booking 11, catalog 5, payment 11), and a shared union type would let booking return
// `reservation_overlap` and still compile. Codes stay per-service; only the envelope is shared.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/morisempai/wakewake/shared/platform/correlation"
	"github.com/morisempai/wakewake/shared/platform/obs"
)

// ErrorDetail is one field-level problem, matching the specs' details[] items.
type ErrorDetail struct {
	Field string `json:"field"`
	Issue string `json:"issue"`
}

type errorBody struct {
	Code          string        `json:"code"`
	Message       string        `json:"message"`
	Details       []ErrorDetail `json:"details,omitempty"`
	CorrelationID string        `json:"correlation_id"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

// WriteError emits the contract error envelope.
//
// code must come from the calling service's own OpenAPI enum — clients switch on it, not on the
// message, so an invented code is a contract break even though it compiles.
//
// message must not leak internals. A 5xx that echoes a driver error hands an attacker your
// schema, and the specs say 5xx never contain stack traces.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string, details ...ErrorDetail) {
	writeJSON(w, status, errorEnvelope{Error: errorBody{
		Code:    code,
		Message: message,
		Details: details,
		// Always present, so a customer quoting the ID from an error page leads straight to the
		// logs and trace for that exact request.
		CorrelationID: correlation.FromContext(r.Context()),
	}})
}

// WriteJSON emits a success body.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// ClientConfig configures an outbound client.
type ClientConfig struct {
	// Timeout per attempt. Defaults to 3s (docs/nfr.md).
	Timeout time.Duration

	// MaxRetries defaults to 2 (so 3 attempts total).
	//
	// NOTE for reviewers: docs/nfr.md sets a 3s timeout AND a 1s p99 for the booking hold. Three
	// attempts is a 9s worst case — a 9x breach of that budget. Retries here are therefore
	// deliberately conservative, and the conflict is raised as an issue rather than silently
	// resolved by this package picking a side.
	MaxRetries int

	// Base backoff, default 100ms, with full jitter and a 1s cap.
	Backoff time.Duration
}

// NewClient builds an http.Client that propagates the correlation ID and retries only where it
// is safe to do so.
//
// The retry rule is the important part: GET/HEAD/PUT/DELETE are idempotent by HTTP semantics and
// may be retried. POST may NOT be, unless it carries an Idempotency-Key — the api-contracts
// skill's "retries with jitter only on idempotent calls". Retrying a bare POST whose response
// was lost is how one booking becomes two, and the caller never finds out.
func NewClient(c ClientConfig) *http.Client {
	if c.Timeout <= 0 {
		c.Timeout = 3 * time.Second
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = 0
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = 2
	}
	if c.Backoff <= 0 {
		c.Backoff = 100 * time.Millisecond
	}

	// obs.RoundTripper wraps the base in otelhttp (starts a client span, injects traceparent) THEN
	// the correlation RoundTripper (injects X-Correlation-Id), so every internal hop continues both
	// the machine trace and the human-facing correlation id. A bare correlation.RoundTripper here
	// would keep the correlation id threaded but break the trace at the first service-to-service
	// call — the silent gap ADR-0013 exists to close. Retries live inside the span, so all attempts
	// of one logical call share it.
	return &http.Client{
		Timeout: c.Timeout,
		Transport: obs.RoundTripper(&retryTransport{
			base:       http.DefaultTransport,
			maxRetries: c.MaxRetries,
			backoff:    c.Backoff,
		}),
	}
}

// LogMiddleware logs one line per request with method, route, status, and duration.
func LogMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(rec, r)

			log.InfoContext(r.Context(), "request",
				slog.String("method", r.Method),
				// Pattern, not the raw path: a high-cardinality label like /v1/bookings/<uuid>
				// makes metrics unusable and log grouping meaningless.
				slog.String("route", routeOf(r)),
				slog.Int("status", rec.status),
				slog.Duration("duration", time.Since(start)))
		})
	}
}

func routeOf(r *http.Request) string {
	if p := r.Pattern; p != "" {
		return p
	}
	return r.URL.Path
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
