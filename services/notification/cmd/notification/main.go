// Command notification runs the notification service: it consumes BookingConfirmed and sends one
// confirmation email per booking.
//
// Bootstrap only (service-template): read config, wire dependencies, start, shut down. It serves
// /healthz and /readyz for orchestration probes even though it exposes no public API — a
// consumer-only service still needs to tell the orchestrator whether it is alive and ready.
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
	"sync"
	"syscall"
	"time"

	"github.com/morisempai/wakewake/shared/contracts/events"
	"github.com/morisempai/wakewake/shared/platform/broker"
	"github.com/morisempai/wakewake/shared/platform/consumer"
	"github.com/morisempai/wakewake/shared/platform/correlation"
	"github.com/morisempai/wakewake/shared/platform/health"
	"github.com/morisempai/wakewake/shared/platform/httpx"
	"github.com/morisempai/wakewake/shared/platform/logging"
	"github.com/morisempai/wakewake/shared/platform/obs"
	"github.com/morisempai/wakewake/shared/platform/pgxx"

	"github.com/morisempai/wakewake/services/notification/internal/config"
	appevents "github.com/morisempai/wakewake/services/notification/internal/events"
	"github.com/morisempai/wakewake/services/notification/internal/infra"
)

func main() {
	// The container HEALTHCHECK runs the binary itself against its own probe. The runtime image is
	// distroless, so there is no curl, wget, or shell to do it with.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := probe(); err != nil {
			fmt.Fprintf(os.Stderr, "notification: healthcheck: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "notification: %v\n", err)
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

	// SIGINT/SIGTERM cancel this context, which every long-running component below selects on.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Install the TracerProvider and W3C propagator process-wide. Init never fails on an
	// unreachable collector — spans are dropped, trace_id still reaches the logs (ADR-0013).
	shutdownObs, err := obs.Init(ctx, obs.Config{
		Service:  config.ServiceName,
		Endpoint: cfg.OTLPEndpoint,
		Insecure: true,
	})
	if err != nil {
		return err
	}
	defer func() { _ = shutdownObs(context.Background()) }()

	pool, err := pgxx.NewPool(ctx, pgxx.PoolConfig{
		URL:             cfg.Postgres.URL,
		MaxConns:        cfg.Postgres.MaxConns,
		MaxConnLifetime: cfg.Postgres.MaxConnLifetime,
	})
	if err != nil {
		return err
	}
	defer pool.Close()

	conn, err := broker.Dial(ctx, cfg.AMQP.URL)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	// The recipient resolver is a DEV stub: BookingConfirmed carries no email and there is no
	// identity service in this slice (issue #19). See infra.DevRecipientResolver.
	store := infra.NewStore()
	resolver := infra.NewDevRecipientResolver()
	mailer := infra.NewSMTPMailer(cfg.SMTP.Host, cfg.SMTP.Port, cfg.FromAddress)
	handler := appevents.NewConfirmationHandler(store, resolver, mailer, log)

	checker := health.NewChecker(2 * time.Second)
	checker.Register("postgres", pool.Ping)
	checker.Register("rabbitmq", func(context.Context) error {
		if conn.IsClosed() {
			return errors.New("broker connection is closed")
		}
		return nil
	})

	// Probe-only HTTP surface. obs.Handler is outermost so a server span is in context before
	// correlation and logging run, and every probe log line carries a trace_id.
	probeHandler := newProbeHandler(checker, log)

	server := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           probeHandler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	cancelCtx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()

	var wg sync.WaitGroup
	fail := make(chan error, 2)

	// The consumer runs its handler through shared/platform/inbox, so each BookingConfirmed is
	// handled inside the transaction that records it as processed — see the inbox package doc for
	// why one transaction is the only correct arrangement.
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := consumer.Run(cancelCtx, conn, pool, log, consumer.Options{
			Service: config.ServiceName,
			Events:  []string{events.BookingConfirmed},
			// Notification calls SMTP, which is slower and less tolerant of parallelism than a
			// database write, so it prefetches fewer messages than the ADR-0010 default of 20.
			Prefetch: 5,
		}, handler.Handle)
		if err != nil && !errors.Is(err, context.Canceled) {
			fail <- fmt.Errorf("consumer:BookingConfirmed: %w", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		log.InfoContext(ctx, "http server listening", slog.String("addr", cfg.HTTP.Addr))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fail <- fmt.Errorf("http server: %w", err)
		}
	}()

	// Stop on the first component failure or on a signal, whichever comes first.
	var runErr error
	select {
	case runErr = <-fail:
		log.ErrorContext(ctx, "shutting down after a component failure", slog.String("error", runErr.Error()))
	case <-ctx.Done():
		log.InfoContext(ctx, "shutdown signal received")
	}

	// Drain in-flight probes before tearing down the workers.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.ErrorContext(ctx, "http server did not shut down cleanly", slog.String("error", err.Error()))
	}

	cancelAll()
	wg.Wait()
	log.InfoContext(context.Background(), "stopped")

	return runErr
}

// newProbeHandler builds the probe-only HTTP surface (/healthz + /readyz). obs.Handler wraps it
// outermost so otelhttp starts a server span before correlation and logging run — otherwise the
// probe log lines carry no trace_id and trace↔log correlation silently breaks (ADR-0013). Kept as
// a helper because this service has no internal/api router package to host the wiring or test it.
func newProbeHandler(checker *health.Checker, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	checker.Mount(mux)
	return obs.Handler(correlation.Middleware(httpx.LogMiddleware(log)(mux)), config.ServiceName)
}

// probe performs the container health check: a GET against this process's own /healthz. Liveness
// only — it deliberately does NOT call /readyz, so a briefly unreachable dependency does not turn
// into a restart loop.
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
