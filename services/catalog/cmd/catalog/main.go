// Command catalog runs the read-mostly product catalog service.
//
// Bootstrap only (service-template): read config, wire dependencies, start, shut down. Every
// decision this file appears to make is really a decision made elsewhere and merely assembled here.
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
	"github.com/morisempai/wakewake/shared/platform/pgxx"

	"github.com/morisempai/wakewake/services/catalog/internal/api"
	"github.com/morisempai/wakewake/services/catalog/internal/config"
	"github.com/morisempai/wakewake/services/catalog/internal/domain"
	"github.com/morisempai/wakewake/services/catalog/internal/infra"
)

func main() {
	// The container HEALTHCHECK runs the binary itself against its own probe. The runtime image is
	// distroless, so there is no curl, wget, or shell to do it with, and pulling in a shell just to
	// answer "is this process alive" would be a larger attack surface than the check is worth.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := probe(); err != nil {
			fmt.Fprintf(os.Stderr, "catalog: healthcheck: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		// Written to stderr rather than logged: a failure before the logger exists has nowhere else
		// to go, and one after it still deserves to be the last thing on the terminal.
		fmt.Fprintf(os.Stderr, "catalog: %v\n", err)
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

	// SIGINT/SIGTERM cancel this context, which the server shutdown below selects on.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := pgxx.NewPool(ctx, pgxx.PoolConfig{
		URL:             cfg.Postgres.URL,
		MaxConns:        cfg.Postgres.MaxConns,
		MaxConnLifetime: cfg.Postgres.MaxConnLifetime,
	})
	if err != nil {
		return err
	}
	defer pool.Close()

	store := infra.NewStore(pool)
	svc := domain.NewService(store)

	checker := health.NewChecker(2 * time.Second)
	checker.Register("postgres", pool.Ping)

	handler := api.NewRouter(api.NewServer(svc, log), checker, log)

	server := &http.Server{
		Addr:    cfg.HTTP.Addr,
		Handler: handler,
		// Bounded so a slow or stuck client cannot pin a connection indefinitely. Every handler
		// here is a single short query.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.InfoContext(ctx, "http server listening", slog.String("addr", cfg.HTTP.Addr))
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

// probe performs the container health check: a GET against this process's own /healthz.
//
// Liveness only. It deliberately does NOT call /readyz — failing a HEALTHCHECK restarts the
// container, and restarting because a dependency is briefly unreachable turns a database blip into
// a crash loop.
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
