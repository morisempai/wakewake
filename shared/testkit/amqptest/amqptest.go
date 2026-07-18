// Package amqptest starts a real RabbitMQ for integration tests.
//
// Like pgtest, this exists so five service agents do not each build container setup, and so the
// messaging guarantees are tested against a real broker rather than a fake. A fake publisher
// cannot demonstrate publisher confirms, dead-lettering, or redelivery — and those are exactly
// the behaviours ADR-0002's at-least-once claim rests on.
package amqptest

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/rabbitmq"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/morisempai/wakewake/shared/contracts/events"
	"github.com/morisempai/wakewake/shared/platform/broker"
)

var (
	once sync.Once
	url  string
	err  error
	ctr  *rabbitmq.RabbitMQContainer
)

// container starts one broker per test binary and reuses it. Isolation between tests comes from
// unique consumer names rather than separate brokers — starting a RabbitMQ per test would make
// the suite unusably slow.
func container(ctx context.Context) (string, error) {
	once.Do(func() {
		c, e := rabbitmq.Run(ctx,
			"rabbitmq:3.13-management-alpine",
			testcontainers.WithWaitStrategy(
				wait.ForLog("Server startup complete").
					WithStartupTimeout(90*time.Second),
			),
		)
		if e != nil {
			err = fmt.Errorf("amqptest: starting rabbitmq: %w", e)
			return
		}
		ctr = c
		url, e = c.AmqpURL(ctx)
		if e != nil {
			err = fmt.Errorf("amqptest: amqp url: %w", e)
		}
	})
	return url, err
}

// Conn returns a broker connection with the exchanges declared.
func Conn(t *testing.T) *broker.Conn {
	t.Helper()

	ctx := context.Background()
	amqpURL, e := container(ctx)
	if e != nil {
		t.Fatalf("%v", e)
	}

	c, e := broker.Dial(ctx, amqpURL)
	if e != nil {
		t.Fatalf("amqptest: dialling broker: %v", e)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// URL returns the broker URL, for tests that need to dial it themselves.
func URL(t *testing.T) string {
	t.Helper()
	u, e := container(context.Background())
	if e != nil {
		t.Fatalf("%v", e)
	}
	return u
}

// Collect binds a temporary queue to the given events and streams what arrives.
//
// It is for asserting that something WAS published. It deliberately uses its own exclusive
// queue rather than the service's real one, so watching a stream in a test never steals messages
// from the consumer under test — a bug that presents as random, unreproducible message loss.
func Collect(t *testing.T, c *broker.Conn, eventNames ...string) <-chan events.Envelope {
	t.Helper()

	ch, e := c.Channel()
	if e != nil {
		t.Fatalf("amqptest: opening channel: %v", e)
	}

	q, e := ch.QueueDeclare("", false, true, true, false, nil)
	if e != nil {
		t.Fatalf("amqptest: declaring collector queue: %v", e)
	}
	for _, name := range eventNames {
		if e := ch.QueueBind(q.Name, name, broker.Exchange, false, nil); e != nil {
			t.Fatalf("amqptest: binding collector to %s: %v", name, e)
		}
	}

	deliveries, e := ch.Consume(q.Name, "", true, true, false, false, nil)
	if e != nil {
		t.Fatalf("amqptest: consuming collector queue: %v", e)
	}

	out := make(chan events.Envelope, 64)
	go func() {
		defer close(out)
		for d := range deliveries {
			env, parseErr := events.Parse(d.Body)
			if parseErr != nil {
				// Not t.Fatalf: this runs on a goroutine, where Fatalf is undefined behaviour.
				t.Errorf("amqptest: collector received an unparseable envelope: %v", parseErr)
				continue
			}
			out <- env
		}
	}()

	t.Cleanup(func() { _ = ch.Close() })
	return out
}

// Expect waits for one envelope of the given event, failing if it does not arrive in time.
//
// Always assert with a timeout rather than a bare receive: a bare receive on a channel that
// never delivers hangs until the whole suite's timeout, which reports as an unhelpful panic
// dump instead of naming the event that went missing.
func Expect(t *testing.T, stream <-chan events.Envelope, event string, within time.Duration) events.Envelope {
	t.Helper()

	deadline := time.After(within)
	for {
		select {
		case env, ok := <-stream:
			if !ok {
				t.Fatalf("amqptest: stream closed while waiting for %s", event)
			}
			if env.Event == event {
				return env
			}
		case <-deadline:
			t.Fatalf("amqptest: timed out after %s waiting for %s", within, event)
		}
	}
}

// ExpectNone asserts nothing arrives within the window — for proving a duplicate was suppressed
// or a filtered event was not republished.
func ExpectNone(t *testing.T, stream <-chan events.Envelope, within time.Duration) {
	t.Helper()

	select {
	case env, ok := <-stream:
		if ok {
			t.Fatalf("amqptest: expected no events, got %s (%s)", env.Event, env.ID)
		}
	case <-time.After(within):
	}
}

// QueueDepth reports how many messages are sitting in a queue — used to assert a message landed
// in a DLQ.
func QueueDepth(t *testing.T, c *broker.Conn, queue string) int {
	t.Helper()

	ch, e := c.Channel()
	if e != nil {
		t.Fatalf("amqptest: opening channel: %v", e)
	}
	defer func() { _ = ch.Close() }()

	// Passive: inspect without creating, so a typo in the queue name fails rather than silently
	// declaring an empty queue and reporting a depth of zero.
	q, e := ch.QueueDeclarePassive(queue, true, false, false, false, nil)
	if e != nil {
		t.Fatalf("amqptest: inspecting queue %s: %v", queue, e)
	}
	return q.Messages
}

// Terminate stops the shared container.
func Terminate(ctx context.Context) error {
	if ctr == nil {
		return nil
	}
	return ctr.Terminate(ctx)
}
