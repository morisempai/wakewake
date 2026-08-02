// Command gateway runs the API gateway: the sole external ingress (ADR-0006, ADR-0012).
//
// Bootstrap only (service-template): read config, build the verifier, limiter, and reverse proxies,
// wire the router, start, shut down. Every decision this file appears to make is made elsewhere and
// merely assembled here.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/morisempai/wakewake/shared/platform/health"
	"github.com/morisempai/wakewake/shared/platform/logging"
	"github.com/morisempai/wakewake/shared/platform/obs"

	"github.com/morisempai/wakewake/services/gateway/internal/auth"
	"github.com/morisempai/wakewake/services/gateway/internal/config"
	"github.com/morisempai/wakewake/services/gateway/internal/ratelimit"
	"github.com/morisempai/wakewake/services/gateway/internal/router"
)

func main() {
	// The container HEALTHCHECK runs the binary against its own probe. The runtime image is
	// distroless, so there is no curl or shell to do it with, and adding one would be a larger
	// attack surface than the check is worth.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := probe(); err != nil {
			fmt.Fprintf(os.Stderr, "gateway: healthcheck: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "gateway: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	level, ok := logging.LevelFromString(cfg.LogLevel)
	log := logging.New(logging.Options{Service: config.ServiceName, Level: level})
	if !ok {
		log.Warn("unrecognised LOG_LEVEL; defaulting to info", slog.String("value", cfg.LogLevel))
	}

	// SIGINT/SIGTERM cancel this context, which both the server shutdown and the JWKS background
	// refresh select on.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Tracing (ADR-0013). Installs the process-wide TracerProvider and W3C propagator so inbound
	// traceparent headers are honored and spans export to the collector when one is configured. With
	// OTEL_EXPORTER_OTLP_ENDPOINT unset the exporter ships nothing — trace_id/span_id still reach the
	// logs — so a missing collector never keeps the gateway from starting. Insecure is the safe
	// default for a bare host:port dev collector; a URL endpoint's scheme overrides it.
	shutdownObs, err := obs.Init(ctx, obs.Config{
		Service:  config.ServiceName,
		Endpoint: cfg.OTLPEndpoint,
		Insecure: true,
	})
	if err != nil {
		return err
	}
	defer func() { _ = shutdownObs(context.Background()) }()

	// Build the verifier first: it fetches the JWKS synchronously, so a bad issuer/JWKS config or an
	// unreachable Keycloak fails the boot loudly rather than at the first request.
	verifier, err := auth.NewVerifier(ctx, cfg.OIDCJWKSURL, cfg.OIDCIssuer, cfg.ClockSkew)
	if err != nil {
		return err
	}

	upstreams, err := router.BuildUpstreams(cfg.Upstreams, log)
	if err != nil {
		return err
	}

	limiter := ratelimit.New(cfg.RateLimit.RPS, cfg.RateLimit.Burst)

	checker := health.NewChecker(2 * time.Second)
	// Readiness depends on having signing keys: a gateway that cannot authenticate anyone must not
	// receive traffic. Liveness (/healthz) deliberately checks nothing.
	checker.Register("jwks", verifier.Ready)

	handler := router.New(upstreams, verifier, limiter, checker, log)

	server := &http.Server{
		Addr:    cfg.HTTP.Addr,
		Handler: handler,
		// Bounded so a slow or stuck client cannot pin a connection. WriteTimeout is generous
		// because responses stream through from upstreams.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.InfoContext(ctx, "http server listening",
			slog.String("addr", cfg.HTTP.Addr),
			slog.String("oidc_issuer", cfg.OIDCIssuer))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http server: %w", err)
		}
	}()

	var runErr error
	select {
	case runErr = <-errCh:
		log.ErrorContext(ctx, "shutting down after a server failure", slog.String("error", runErr.Error()))
	case <-ctx.Done():
		log.InfoContext(ctx, "shutdown signal received")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.ErrorContext(ctx, "http server did not shut down cleanly", slog.String("error", err.Error()))
	}

	log.InfoContext(context.Background(), "stopped")
	return runErr
}

// probe performs the container health check: a GET against this process's own /healthz. Liveness
// only — it deliberately does NOT call /readyz, since failing a HEALTHCHECK restarts the container
// and restarting because Keycloak briefly blinked turns a JWKS blip into a crash loop.
func probe() error {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}

	client := &http.Client{Timeout: 2 * time.Second}
	res, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("/healthz returned %d", res.StatusCode)
	}
	return nil
}
