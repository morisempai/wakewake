// Command payment runs the payment service: create a Stripe PaymentIntent for a held booking, and
// turn a verified Stripe webhook into PaymentSucceeded / PaymentFailed.
//
// Bootstrap only (service-template): read config, wire dependencies, start, shut down. Every
// decision this file appears to make is really made elsewhere and merely assembled here.
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

	"github.com/google/uuid"

	"github.com/morisempai/wakewake/shared/contracts/events"
	"github.com/morisempai/wakewake/shared/platform/broker"
	"github.com/morisempai/wakewake/shared/platform/consumer"
	"github.com/morisempai/wakewake/shared/platform/health"
	"github.com/morisempai/wakewake/shared/platform/logging"
	"github.com/morisempai/wakewake/shared/platform/outbox"
	"github.com/morisempai/wakewake/shared/platform/pgxx"

	"github.com/morisempai/wakewake/services/payment/internal/api"
	"github.com/morisempai/wakewake/services/payment/internal/config"
	"github.com/morisempai/wakewake/services/payment/internal/domain"
	appevents "github.com/morisempai/wakewake/services/payment/internal/events"
	"github.com/morisempai/wakewake/services/payment/internal/infra"
	"github.com/morisempai/wakewake/services/payment/internal/stripe"
)

func main() {
	// The container HEALTHCHECK runs the binary itself against its own probe. The runtime image is
	// distroless, so there is no curl, wget, or shell to do it with.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := probe(); err != nil {
			fmt.Fprintf(os.Stderr, "payment: healthcheck: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "payment: %v\n", err)
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

	publisher, err := broker.NewPublisher(conn)
	if err != nil {
		return err
	}
	defer func() { _ = publisher.Close() }()

	// The relay is built before the store so the store can kick it on commit. No cycle: the relay
	// needs the pool and the publisher, neither of which needs the store.
	relay := outbox.NewRelay(pool, publisher, log, outbox.RelayConfig{})

	store := infra.NewStore(pool, relay.Kick)

	// The Stripe client uses the real HTTP transport in production; the secret key is held inside it
	// and only ever placed in the Authorization header, never logged.
	provider := stripe.NewClient(cfg.Stripe.BaseURL, cfg.Stripe.SecretKey, nil)

	svc := domain.NewService(store, provider, time.Now, newPaymentID)

	webhook := api.NewWebhookHandler(svc, store, log, cfg.Stripe.WebhookSecret, cfg.Stripe.WebhookTolerance, time.Now)

	checker := health.NewChecker(2 * time.Second)
	checker.Register("postgres", pool.Ping)
	checker.Register("rabbitmq", func(context.Context) error {
		if conn.IsClosed() {
			return errors.New("broker connection is closed")
		}
		return nil
	})

	handler := api.NewRouter(api.NewServer(svc, log), webhook, checker, log)

	server := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	cancelCtx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()

	var wg sync.WaitGroup
	fail := make(chan error, 4)

	start := func(name string, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(cancelCtx); err != nil && !errors.Is(err, context.Canceled) {
				fail <- fmt.Errorf("%s: %w", name, err)
			}
		}()
	}

	start("outbox relay", relay.Run)

	// One consumer per event (ADR-0010). Payment consumes only BookingHeld in this slice.
	bookingHeld := appevents.NewBookingHeldHandler(store, log)
	start("consumer:BookingHeld", func(c context.Context) error {
		return consumer.Run(c, conn, pool, log, consumer.Options{
			Service: config.ServiceName,
			Events:  []string{events.BookingHeld},
		}, bookingHeld.Handle)
	})

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

	// Drain in-flight requests before tearing down the workers they depend on.
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

// newPaymentID mints a UUIDv7, so payment ids sort by creation time and the primary key index stays
// append-mostly rather than scattering inserts across the whole keyspace.
func newPaymentID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("payment: generating uuid: %w", err)
	}
	return id.String(), nil
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
