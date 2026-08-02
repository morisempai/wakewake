// Package router assembles the gateway's full external HTTP surface: the health probes, the
// path-prefix routing table, and the middleware chain (correlation, logging, auth, rate limiting)
// that wraps it.
//
// It lives in its own package, separate from cmd/, so the tests exercise the SAME handler the
// binary serves — the whole point of a gateway is the routing and middleware wiring, and a test
// that builds its own chain proves nothing about the one that runs.
package router

import (
	"log/slog"
	"net/http"

	"github.com/morisempai/wakewake/shared/platform/correlation"
	"github.com/morisempai/wakewake/shared/platform/health"
	"github.com/morisempai/wakewake/shared/platform/httpx"
	"github.com/morisempai/wakewake/shared/platform/obs"

	"github.com/morisempai/wakewake/services/gateway/internal/auth"
	"github.com/morisempai/wakewake/services/gateway/internal/config"
	"github.com/morisempai/wakewake/services/gateway/internal/proxy"
	"github.com/morisempai/wakewake/services/gateway/internal/ratelimit"
)

// Upstreams are the four reverse proxies, one per internal service. They are handlers rather than
// URLs so tests can substitute httptest recorders.
type Upstreams struct {
	Catalog      http.Handler
	Availability http.Handler
	Booking      http.Handler
	Payment      http.Handler
}

// BuildUpstreams constructs a reverse proxy per configured base URL.
func BuildUpstreams(up config.Upstreams, log *slog.Logger) (Upstreams, error) {
	catalog, err := proxy.New(up.Catalog, log)
	if err != nil {
		return Upstreams{}, err
	}
	availability, err := proxy.New(up.Availability, log)
	if err != nil {
		return Upstreams{}, err
	}
	booking, err := proxy.New(up.Booking, log)
	if err != nil {
		return Upstreams{}, err
	}
	payment, err := proxy.New(up.Payment, log)
	if err != nil {
		return Upstreams{}, err
	}
	return Upstreams{
		Catalog:      catalog,
		Availability: availability,
		Booking:      booking,
		Payment:      payment,
	}, nil
}

// New assembles the whole surface. The middleware order is deliberate and load-bearing:
//
//   - correlation is OUTERMOST, so every log line and error envelope below it — including a 401 or
//     429 — carries the id, and the response echoes it.
//   - the request log sits just inside correlation, so every request is logged once with its id.
//   - on protected routes, auth runs BEFORE rate limiting, so the limiter can key on the subject
//     rather than only the IP.
//   - the public Stripe webhook skips auth entirely (Stripe signs its own payloads) but is still
//     rate limited by IP, so it cannot be used to flood the payment service.
func New(up Upstreams, verifier *auth.Verifier, limiter *ratelimit.Limiter, checker *health.Checker, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	// Probes first, on their own specific patterns, so ServeMux routes them here rather than to the
	// catch-all. shared/platform/health is canonical: /healthz checks nothing on purpose.
	checker.Mount(mux)

	authmw := auth.Middleware(verifier, log)
	ratemw := ratelimit.Middleware(limiter, log)
	protected := func(h http.Handler) http.Handler { return authmw(ratemw(h)) }

	// Routing table (issue #29). Each prefix is registered both exactly and as a subtree so both the
	// collection route (/v1/products) and item routes (/v1/products/{id}) reach the same upstream.
	handlePrefix(mux, "/v1/products", protected(up.Catalog))
	handlePrefix(mux, "/v1/resources", protected(up.Availability))
	handlePrefix(mux, "/v1/reservations", protected(up.Availability))
	handlePrefix(mux, "/v1/bookings", protected(up.Booking))
	handlePrefix(mux, "/v1/payments", protected(up.Payment))

	// The one public route: Stripe authenticates itself with a signature the payment service
	// verifies, so no JWT is checked here. Method-scoped so only POST is public.
	mux.Handle("POST /v1/webhooks/stripe", ratemw(up.Payment))

	// Anything else is unknown at the edge. Rendered as the shared envelope rather than ServeMux's
	// plain-text 404 so every response the gateway produces has the same shape.
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, r, http.StatusNotFound, "not_found", "No route matches this request.")
	}))

	// obs.Handler is the OUTERMOST wrapper: otelhttp must start the server span before anything
	// else runs, so the span context is already on the request context when correlation and the log
	// middleware read from it — that is what puts a trace_id on every log line (ADR-0013).
	// Correlation stays just inside it: every log line and error envelope downstream reads the
	// correlation id from the context, so nothing but the tracing span may run before it.
	return obs.Handler(correlation.Middleware(httpx.LogMiddleware(log)(mux)), config.ServiceName)
}

// handlePrefix registers a handler for both the exact path and its subtree. Go's ServeMux treats
// "/v1/products" (exact) and "/v1/products/" (subtree) as distinct patterns, and a service exposes
// both the collection and item forms, so both must route to the same upstream.
func handlePrefix(mux *http.ServeMux, prefix string, h http.Handler) {
	mux.Handle(prefix, h)
	mux.Handle(prefix+"/", h)
}
