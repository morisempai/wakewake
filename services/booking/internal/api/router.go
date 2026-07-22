package api

import (
	"log/slog"
	"net/http"

	spec "github.com/morisempai/wakewake/shared/contracts/openapi/booking"
	"github.com/morisempai/wakewake/shared/platform/correlation"
	"github.com/morisempai/wakewake/shared/platform/health"
	"github.com/morisempai/wakewake/shared/platform/httpx"
)

// NewRouter assembles the whole HTTP surface: probes, the generated spec router, the auth
// middleware, and the correlation/logging chain.
//
// It lives here rather than in cmd/ so the contract tests exercise the SAME handler the binary
// serves — including the error paths that only exist in the generated wrapper's parameter binding.
func NewRouter(srv *Server, checker *health.Checker, log *slog.Logger) http.Handler {
	strict := spec.NewStrictHandlerWithOptions(srv, nil, spec.StrictHTTPServerOptions{
		// A body that is not valid JSON, or whose types do not match the schema. The generated
		// default writes plain text, which would not match the spec's Error envelope.
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			log.WarnContext(r.Context(), "rejecting malformed request body",
				slog.String("route", r.Pattern), slog.String("error", err.Error()))
			WriteError(w, r, http.StatusBadRequest, spec.ErrorErrorCodeValidationFailed,
				"The request body is not valid for this endpoint.")
		},

		// A handler returned an error rather than a typed response: an unmapped domain error or a
		// genuine fault. The cause is logged, never echoed — a 5xx that returns a driver message
		// hands out the schema.
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			log.ErrorContext(r.Context(), "unhandled error serving request",
				slog.String("route", r.Pattern), slog.String("error", err.Error()))
			WriteError(w, r, http.StatusInternalServerError, spec.ErrorErrorCodeInternalError,
				"The request could not be completed.")
		},
	})

	generated := spec.HandlerWithOptions(strict, spec.StdHTTPServerOptions{
		// Path and query parameter binding failures: a malformed UUID in the path, a bad limit,
		// a missing Idempotency-Key header.
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			log.WarnContext(r.Context(), "rejecting request with unbindable parameters",
				slog.String("path", r.URL.Path), slog.String("error", err.Error()))
			WriteError(w, r, http.StatusBadRequest, spec.ErrorErrorCodeValidationFailed,
				"One or more request parameters are missing or malformed.")
		},
	})

	mux := http.NewServeMux()

	// Mounted BEFORE the catch-all and on more specific patterns, so Go's ServeMux routes the
	// probes here. shared/platform/health is the canonical implementation; /healthz checks nothing
	// on purpose so a database blip does not turn into a crash loop.
	checker.Mount(mux)
	mux.Handle("/", generated)

	// Auth reads the JWT `sub` into the context for the strict handlers; the probes ignore it.
	// Correlation is outermost: every log line, staged event, and error envelope downstream reads
	// the id from the context, so nothing may run before it is placed there.
	return correlation.Middleware(httpx.LogMiddleware(log)(authMiddleware(mux)))
}
