package api

import (
	"log/slog"
	"net/http"

	spec "github.com/morisempai/wakewake/shared/contracts/openapi/availability"
	"github.com/morisempai/wakewake/shared/platform/correlation"
	"github.com/morisempai/wakewake/shared/platform/health"
	"github.com/morisempai/wakewake/shared/platform/httpx"
)

// NewRouter assembles the whole HTTP surface: probes, the generated spec router, and the
// middleware chain.
//
// It lives here rather than in cmd/ so the contract tests exercise the SAME handler the binary
// serves — including the error paths that only exist in the generated wrapper's parameter
// binding. A test that builds its own router proves things about a router nobody runs.
func NewRouter(srv *Server, checker *health.Checker, log *slog.Logger) http.Handler {
	strict := spec.NewStrictHandlerWithOptions(srv, nil, spec.StrictHTTPServerOptions{
		// A body that is not valid JSON, or whose types do not match the schema. The generated
		// default writes plain text, which would fail AC5's "including the error envelope on
		// every non-2xx".
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			log.WarnContext(r.Context(), "rejecting malformed request body",
				slog.String("route", r.Pattern), slog.String("error", err.Error()))
			WriteError(w, r, http.StatusBadRequest, spec.ValidationFailed,
				"The request body is not valid for this endpoint.")
		},

		// A handler returned an error rather than a typed response: an unmapped domain error or
		// a genuine fault. The cause is logged, never echoed — the specs say 5xx bodies carry no
		// internals, and a 500 that returns a driver message hands out the schema.
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			log.ErrorContext(r.Context(), "unhandled error serving request",
				slog.String("route", r.Pattern), slog.String("error", err.Error()))
			WriteError(w, r, http.StatusInternalServerError, spec.InternalError,
				"The request could not be completed.")
		},
	})

	// The generated router owns its own mux. Probes are deliberately NOT taken from it — see
	// below — so this mux only ever serves the /v1 routes in practice.
	generated := spec.HandlerWithOptions(strict, spec.StdHTTPServerOptions{
		// Path and query parameter binding failures: a malformed UUID in the path, a `from` that
		// is not RFC 3339, a missing Idempotency-Key header.
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			log.WarnContext(r.Context(), "rejecting request with unbindable parameters",
				slog.String("path", r.URL.Path), slog.String("error", err.Error()))
			WriteError(w, r, http.StatusBadRequest, spec.ValidationFailed,
				"One or more request parameters are missing or malformed.")
		},
	})

	mux := http.NewServeMux()

	// Mounted BEFORE the catch-all and on more specific patterns, so Go's ServeMux routes the
	// probes here. shared/platform/health is the canonical implementation — in particular
	// /healthz checks nothing on purpose, because a liveness probe that fails when Postgres
	// blinks gets the container killed and turns a database blip into a crash loop.
	checker.Mount(mux)
	mux.Handle("/", generated)

	// Correlation is outermost: every log line, every staged event, and every error envelope
	// downstream reads the id from the context, so nothing may run before it is placed there.
	return correlationFirst(httpx.LogMiddleware(log)(mux))
}

// correlationFirst is a named alias for the shared middleware, kept separate so the ordering
// comment above has something to point at.
func correlationFirst(next http.Handler) http.Handler {
	return correlation.Middleware(next)
}
