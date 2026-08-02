package api

import (
	"log/slog"
	"net/http"

	"github.com/morisempai/wakewake/services/payment/internal/config"
	spec "github.com/morisempai/wakewake/shared/contracts/openapi/payment"
	"github.com/morisempai/wakewake/shared/platform/correlation"
	"github.com/morisempai/wakewake/shared/platform/health"
	"github.com/morisempai/wakewake/shared/platform/httpx"
	"github.com/morisempai/wakewake/shared/platform/obs"
)

// NewRouter assembles the whole HTTP surface: probes, the raw Stripe webhook, the generated spec
// router, the auth middleware, and the correlation/logging chain.
//
// It lives here rather than in cmd/ so the contract tests exercise the SAME handler the binary
// serves — including the raw webhook route and the generated wrapper's parameter binding.
//
// The Stripe webhook is mounted on its exact method+path, which Go's ServeMux prefers over the
// catch-all `/`, so it — not the generated strict handler — receives the request. This is
// load-bearing: the generated handler decodes the body, and the webhook needs the raw bytes to
// verify the signature.
func NewRouter(srv *Server, webhook *WebhookHandler, checker *health.Checker, log *slog.Logger) http.Handler {
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
		// hands out internals.
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			log.ErrorContext(r.Context(), "unhandled error serving request",
				slog.String("route", r.Pattern), slog.String("error", err.Error()))
			WriteError(w, r, http.StatusInternalServerError, spec.ErrorErrorCodeInternalError,
				"The request could not be completed.")
		},
	})

	generated := spec.HandlerWithOptions(strict, spec.StdHTTPServerOptions{
		// Path and query parameter binding failures: a malformed UUID in the path, a missing
		// Idempotency-Key header.
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			log.WarnContext(r.Context(), "rejecting request with unbindable parameters",
				slog.String("path", r.URL.Path), slog.String("error", err.Error()))
			WriteError(w, r, http.StatusBadRequest, spec.ErrorErrorCodeValidationFailed,
				"One or more request parameters are missing or malformed.")
		},
	})

	mux := http.NewServeMux()

	// Probes on their specific patterns, mounted before the catch-all so Go's ServeMux routes them
	// here. shared/platform/health is canonical; /healthz checks nothing on purpose.
	checker.Mount(mux)

	// The raw webhook, on its exact route so it wins over the generated `/`. It reads the raw body
	// for signature verification — see webhook.go.
	mux.Handle("POST /v1/webhooks/stripe", webhook)

	mux.Handle("/", generated)

	// Auth reads the JWT `sub` into the context for the strict handlers; the probes and the webhook
	// ignore it (the webhook authenticates by Stripe signature).
	//
	// obs.Handler is the OUTERMOST wrapper: otelhttp must start the server span before anything
	// else runs, so the span context is already on the request context when correlation and the
	// log middleware read from it — that is what puts a trace_id on every log line, including the
	// inbound Stripe webhook's (ADR-0013). Correlation stays just inside it: every log line,
	// staged event, and error envelope downstream reads the correlation id from the context, so
	// nothing but the tracing span may run before it.
	return obs.Handler(correlation.Middleware(httpx.LogMiddleware(log)(authMiddleware(mux))), config.ServiceName)
}
