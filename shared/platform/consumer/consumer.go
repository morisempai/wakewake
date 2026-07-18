// Package consumer runs the event-handling loop: parse, dedupe, handle, ack, retry, dead-letter.
//
// It exists in shared/ because testing-standards requires every consumer test to cover duplicate
// delivery, unknown payload fields, and a dependency being down. Five services hand-rolling that
// loop means five subtly different retry policies and five chances to get idempotency wrong —
// and "wrong" here means the guarantee is untested while looking tested.
package consumer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/morisempai/wakewake/shared/contracts/events"
	"github.com/morisempai/wakewake/shared/platform/broker"
	"github.com/morisempai/wakewake/shared/platform/correlation"
	"github.com/morisempai/wakewake/shared/platform/inbox"
)

// Options configures one consumer.
type Options struct {
	// Service is this consumer's name, used for queue naming and dedupe scoping. Required.
	Service string

	// Events are the event names to bind and consume. Required.
	Events []string

	// Prefetch defaults to 20 (ADR-0010). Notification uses 5: it calls SMTP, which is slower
	// and less tolerant of parallelism than a database write.
	Prefetch int

	// MaxAttempts defaults to 3 (ADR-0010). After that the message is dead-lettered rather than
	// requeued — anything surviving three attempts over ~6s is not transient, and an infinite
	// requeue loop hides the bug instead of surfacing it.
	MaxAttempts int

	// Backoff defaults to 200ms, 1s, 5s, each with full jitter.
	Backoff []time.Duration
}

func (o *Options) applyDefaults() {
	if o.Prefetch <= 0 {
		o.Prefetch = 20
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 3
	}
	if len(o.Backoff) == 0 {
		o.Backoff = []time.Duration{200 * time.Millisecond, time.Second, 5 * time.Second}
	}
}

// Run consumes the configured events until ctx is cancelled.
//
// The handler runs through inbox.Process, so it executes inside the same transaction that
// records the event as processed. That is what makes "idempotent, keyed on the envelope id"
// actually true rather than merely claimed — see the inbox package doc for why any arrangement
// of two separate transactions is broken.
func Run(ctx context.Context, conn *broker.Conn, pool *pgxpool.Pool, log *slog.Logger, o Options, h inbox.Handler) error {
	o.applyDefaults()

	if o.Service == "" {
		return fmt.Errorf("consumer: Service is required")
	}
	if len(o.Events) == 0 {
		return fmt.Errorf("consumer: no events to consume")
	}

	log = log.With(slog.String("component", "consumer"))

	errs := make(chan error, len(o.Events))
	for _, event := range o.Events {
		go func(event string) {
			errs <- consumeOne(ctx, conn, pool, log, o, event, h)
		}(event)
	}

	// One queue per event (ADR-0010), so a poison message on one event type cannot block the
	// others. Return on the first genuine failure; ctx cancellation is a clean stop.
	for range o.Events {
		if err := <-errs; err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	return nil
}

func consumeOne(ctx context.Context, conn *broker.Conn, pool *pgxpool.Pool, log *slog.Logger, o Options, event string, h inbox.Handler) error {
	queue, err := conn.DeclareConsumerQueue(o.Service, event)
	if err != nil {
		return err
	}

	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	defer func() { _ = ch.Close() }()

	if err := ch.Qos(o.Prefetch, 0, false); err != nil {
		return fmt.Errorf("consumer: setting prefetch on %s: %w", queue, err)
	}

	// autoAck=false: the broker must not consider a message handled until the handler's
	// transaction has committed. With autoAck the message is gone the instant it is delivered,
	// so a crash mid-handler loses it silently — ADR-0002 requires manual acks for this reason.
	deliveries, err := ch.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consumer: consuming %s: %w", queue, err)
	}

	log.InfoContext(ctx, "consuming", slog.String("queue", queue), slog.Int("prefetch", o.Prefetch))

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case d, ok := <-deliveries:
			if !ok {
				return fmt.Errorf("consumer: delivery channel for %s closed", queue)
			}
			handleDelivery(ctx, pool, log, o, queue, d, h)
		}
	}
}

func handleDelivery(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, o Options, queue string, d amqp.Delivery, h inbox.Handler) {
	e, err := events.Parse(d.Body)
	if err != nil {
		// Malformed or unknown: retrying cannot help, because the bytes will not change. Straight
		// to the DLQ, where a human can look at it.
		log.ErrorContext(ctx, "unparseable message, dead-lettering without retry",
			slog.String("queue", queue), slog.String("error", err.Error()))
		_ = d.Nack(false, false)
		return
	}

	ctx = correlation.WithID(ctx, e.CorrelationID)
	log = log.With(slog.String("event", e.Event), slog.String("event_id", e.ID))

	start := time.Now()
	var lastErr error

	for attempt := 0; attempt < o.MaxAttempts; attempt++ {
		if attempt > 0 {
			wait := o.Backoff[min(attempt-1, len(o.Backoff)-1)]
			select {
			case <-time.After(fullJitter(wait)):
			case <-ctx.Done():
				// Shutting down mid-backoff. Requeue so another replica (or this one after
				// restart) picks it up: this is the one place requeue=true is correct, because
				// nothing is wrong with the message and the work was never completed.
				_ = d.Nack(false, true)
				return
			}
		}

		processed, err := inbox.Process(ctx, pool, o.Service, e, h)
		if err == nil {
			if processed {
				log.InfoContext(ctx, "handled", slog.Duration("duration", time.Since(start)))
			} else {
				// Already processed: a redelivery. Ack it — the work is done, just not now.
				log.DebugContext(ctx, "duplicate delivery ignored")
			}
			_ = d.Ack(false)
			return
		}
		lastErr = err
		log.WarnContext(ctx, "handler failed, will retry",
			slog.Int("attempt", attempt+1), slog.Int("max_attempts", o.MaxAttempts),
			slog.String("error", err.Error()))
	}

	// requeue=false: requeue=true puts the message back at the head of the queue and it spins
	// immediately, burning CPU and log volume while making no progress. The DLQ is the honest
	// destination for something that has genuinely failed.
	log.ErrorContext(ctx, "handler exhausted attempts, dead-lettering",
		slog.String("queue", queue), slog.String("error", lastErr.Error()))
	_ = d.Nack(false, false)
}

// fullJitter returns a duration uniformly in [0, d) — full jitter (ADR-0010).
//
// Full rather than equal jitter because the failure mode is N consumer replicas retrying in
// lockstep against one recovering database. Equal jitter keeps them clustered around a common
// centre; full jitter spreads them across the whole window.
func fullJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Float64() * float64(d))
}
