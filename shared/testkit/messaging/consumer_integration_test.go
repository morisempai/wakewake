//go:build integration

package messaging_test

// The consumer loop's retry and dead-letter path, executed.
//
// This is the part of ADR-0010 that is easiest to get wrong in a way nothing catches: `requeue`
// on a nack. Pass true after exhausting retries and RabbitMQ puts the message back at the head
// of the queue, where it is immediately redelivered, fails again, and spins — burning CPU and
// log volume at full rate while making no progress. Every test that only checks the happy path
// passes. The failure shows up as an unexplained production incident.
//
// So these tests assert not just "the message reached the DLQ" but "the main queue is empty
// afterwards", which is the half that distinguishes dead-lettering from spinning.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/morisempai/wakewake/shared/contracts/events"
	"github.com/morisempai/wakewake/shared/platform/broker"
	"github.com/morisempai/wakewake/shared/platform/consumer"
	"github.com/morisempai/wakewake/shared/platform/correlation"
)

// The infra-wait windows below (waitForQueue 30s, waitFor 90s) are deliberately generous. They are
// poll-based and return the instant the condition holds, so a healthy run is not slowed — the
// headroom only absorbs RabbitMQ container contention when several integration packages start their
// own broker at once under CI load, which was flaking these tests (issue #45). The windows bound
// failure latency, not success latency; the assertions are unchanged.
//
// fastRetries keeps the suite quick. The production values (200ms, 1s, 5s) are ADR-0010's; the
// behaviour under test is the retry-then-dead-letter sequence, not the specific durations.
var fastRetries = []time.Duration{20 * time.Millisecond, 40 * time.Millisecond, 60 * time.Millisecond}

// TestConsumerHandlesAPublishedEvent is the happy path through the real loop: declare the queue,
// consume, dedupe, ack.
func TestConsumerHandlesAPublishedEvent(t *testing.T) {
	conn := amqpConn(t)
	pool := db(t)
	ctx, cancel := context.WithCancel(correlation.WithID(context.Background(), "corr-consume"))
	defer cancel()

	service := "svc" + shortID()

	var handled atomic.Int32
	seen := make(chan events.Envelope, 1)
	handler := func(ctx context.Context, tx pgx.Tx, e events.Envelope) error {
		handled.Add(1)
		select {
		case seen <- e:
		default:
		}
		return nil
	}

	go func() {
		_ = consumer.Run(ctx, conn, pool, quietLogger(), consumer.Options{
			Service: service,
			Events:  []string{events.ReservationCreated},
			Backoff: fastRetries,
		}, handler)
	}()

	// Give the consumer time to declare and bind before publishing, or the message is routed
	// before a queue exists and is silently dropped by the exchange.
	waitForQueue(t, conn, broker.QueueName(service, events.ReservationCreated), 30*time.Second)

	payload := samplePayload()
	publishDirect(t, conn, events.ReservationCreated, payload, "corr-consume")

	select {
	case got := <-seen:
		if got.Event != events.ReservationCreated {
			t.Errorf("event = %s, want %s", got.Event, events.ReservationCreated)
		}
		if got.CorrelationID != "corr-consume" {
			t.Errorf("correlation_id = %q — it must propagate from the envelope into the handler",
				got.CorrelationID)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("consumer never handled the published event")
	}
}

// TestFailingHandlerDeadLettersAndStopsRetrying is the one that matters.
//
// After MaxAttempts the message must land in the DLQ AND leave the main queue empty. A
// requeue=true nack would satisfy "not in the main queue" only momentarily while spinning
// forever, so both halves are asserted.
func TestFailingHandlerDeadLettersAndStopsRetrying(t *testing.T) {
	conn := amqpConn(t)
	pool := db(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	service := "svc" + shortID()
	queue := broker.QueueName(service, events.ReservationCreated)
	dlq := broker.DLQName(queue)

	var attempts atomic.Int32
	alwaysFails := func(ctx context.Context, tx pgx.Tx, e events.Envelope) error {
		attempts.Add(1)
		return errors.New("dependency is down")
	}

	go func() {
		_ = consumer.Run(ctx, conn, pool, quietLogger(), consumer.Options{
			Service:     service,
			Events:      []string{events.ReservationCreated},
			MaxAttempts: 3,
			Backoff:     fastRetries,
		}, alwaysFails)
	}()

	waitForQueue(t, conn, queue, 30*time.Second)
	publishDirect(t, conn, events.ReservationCreated, samplePayload(), "corr-dlq")

	// The message should arrive in the DLQ once retries are exhausted.
	waitFor(t, 90*time.Second, func() bool {
		return amqpDepth(t, conn, dlq) == 1
	}, "message never reached the dead-letter queue")

	if got := attempts.Load(); got != 3 {
		t.Errorf("handler ran %d times, want exactly 3 (MaxAttempts) — fewer means retries are "+
			"not happening, more means the message is being redelivered rather than dead-lettered",
			got)
	}

	// The half that distinguishes dead-lettering from spinning: the main queue must be empty and
	// stay empty. If requeue were true, the message would be back here immediately.
	time.Sleep(500 * time.Millisecond)
	if depth := amqpDepth(t, conn, queue); depth != 0 {
		t.Errorf("main queue holds %d message(s) after dead-lettering — the message is being "+
			"requeued and will spin at full rate making no progress", depth)
	}

	before := attempts.Load()
	time.Sleep(time.Second)
	if after := attempts.Load(); after != before {
		t.Errorf("handler ran %d more times after the message was dead-lettered — it is still "+
			"being redelivered", after-before)
	}
}

// TestMalformedMessageSkipsRetriesEntirely: bytes that cannot be parsed will not parse on the
// second attempt either, so retrying them only delays the inevitable while holding a slot.
func TestMalformedMessageSkipsRetriesEntirely(t *testing.T) {
	conn := amqpConn(t)
	pool := db(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	service := "svc" + shortID()
	queue := broker.QueueName(service, events.ReservationCreated)
	dlq := broker.DLQName(queue)

	var handled atomic.Int32
	handler := func(ctx context.Context, tx pgx.Tx, e events.Envelope) error {
		handled.Add(1)
		return nil
	}

	go func() {
		_ = consumer.Run(ctx, conn, pool, quietLogger(), consumer.Options{
			Service: service,
			Events:  []string{events.ReservationCreated},
			Backoff: fastRetries,
		}, handler)
	}()

	waitForQueue(t, conn, queue, 30*time.Second)
	publishRaw(t, conn, events.ReservationCreated, []byte(`{"not":"an envelope"}`))

	waitFor(t, 90*time.Second, func() bool {
		return amqpDepth(t, conn, dlq) == 1
	}, "malformed message never reached the dead-letter queue")

	if got := handled.Load(); got != 0 {
		t.Errorf("handler ran %d times on an unparseable message — it should never have been "+
			"invoked", got)
	}
}
