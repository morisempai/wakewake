//go:build integration

package messaging_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/morisempai/wakewake/shared/contracts/events"
	"github.com/morisempai/wakewake/shared/platform/broker"
	"github.com/morisempai/wakewake/shared/testkit/amqptest"
)

// amqpConn returns a broker connection for a test.
func amqpConn(t *testing.T) *broker.Conn {
	t.Helper()
	return amqptest.Conn(t)
}

// shortID gives each test its own consumer/service name.
//
// Without this, tests sharing the broker would also share queues: one test's consumer would eat
// another's messages, producing failures that move around between runs and look like flakes
// rather than the collision they are.
func shortID() string {
	return uuid.New().String()[:8]
}

// publishDirect publishes a well-formed envelope straight to the exchange, bypassing the outbox.
//
// Deliberate: these tests are about the CONSUMER's behaviour, and routing them through the relay
// as well would mean a consumer failure and a relay failure look identical.
func publishDirect(t *testing.T, c *broker.Conn, event string, payload any, correlationID string) string {
	t.Helper()

	e, err := events.New(event, uuid.New().String(), time.Now().UTC(), correlationID, payload)
	if err != nil {
		t.Fatalf("building envelope: %v", err)
	}
	body, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshalling envelope: %v", err)
	}
	publishRaw(t, c, event, body)
	return e.ID
}

// publishRaw publishes arbitrary bytes, for testing what a consumer does with input it cannot
// parse.
func publishRaw(t *testing.T, c *broker.Conn, routingKey string, body []byte) {
	t.Helper()

	ch, err := c.Channel()
	if err != nil {
		t.Fatalf("opening channel: %v", err)
	}
	defer func() { _ = ch.Close() }()

	if err := ch.PublishWithContext(context.Background(), broker.Exchange, routingKey, false, false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Body:         body,
		}); err != nil {
		t.Fatalf("publishing: %v", err)
	}
}

// waitForQueue blocks until a queue exists.
//
// Necessary because consumer.Run declares its queue asynchronously. Publishing first would route
// the message when no queue is bound, and a topic exchange silently discards those — the test
// would then fail with "consumer never handled the event", pointing at the wrong thing entirely.
func waitForQueue(t *testing.T, c *broker.Conn, queue string, within time.Duration) {
	t.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		ch, err := c.Channel()
		if err != nil {
			t.Fatalf("opening channel: %v", err)
		}
		_, err = ch.QueueDeclarePassive(queue, true, false, false, false, nil)
		_ = ch.Close()
		if err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("queue %s was never declared", queue)
}

// amqpDepth reports how many messages are sitting in a queue.
func amqpDepth(t *testing.T, c *broker.Conn, queue string) int {
	t.Helper()
	return amqptest.QueueDepth(t, c, queue)
}
