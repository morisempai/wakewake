// Package broker owns the AMQP connection and the exchange/queue topology from ADR-0010.
//
// Topology names are wire-level and operationally load-bearing: renaming a durable queue in a
// running environment is a migration, not an edit, because the old queue keeps accumulating
// messages nobody is reading. Both sides of every event must agree on them, so they are computed
// here rather than written out per service.
package broker

import (
	"context"
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/morisempai/wakewake/shared/contracts/events"
)

// Topology names (ADR-0010).
const (
	// Exchange is the topic exchange every event is published to, fixed by the AsyncAPI server
	// binding. Routing key is the event name.
	Exchange = events.ExchangeName

	// DeadLetterExchange receives messages that exhausted their retries. A separate exchange, so
	// DLQ routing is independent of the original routing key.
	DeadLetterExchange = events.ExchangeName + ".dlx"
)

// QueueName returns the durable queue for one consumer and one event, e.g. "payment.BookingHeld".
//
// One queue per (service, event) rather than one per service: a poison message on one event type
// would otherwise block every other event that service consumes, turning a narrow bug into a
// total outage for that consumer.
func QueueName(service, event string) string {
	return service + "." + event
}

// DLQName returns the dead-letter queue for a queue.
func DLQName(queue string) string {
	return queue + ".dlq"
}

// Conn is a broker connection plus the channel used to declare topology.
type Conn struct {
	conn *amqp.Connection
	url  string
}

// Dial opens a connection and declares the exchanges. It does not declare queues — those belong
// to whichever consumer binds them.
func Dial(ctx context.Context, url string) (*Conn, error) {
	if url == "" {
		return nil, fmt.Errorf("broker: amqp url is empty")
	}

	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, fmt.Errorf("broker: dialling %s: %w", redact(url), err)
	}

	c := &Conn{conn: conn, url: url}
	if err := c.declareExchanges(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *Conn) declareExchanges() error {
	ch, err := c.conn.Channel()
	if err != nil {
		return fmt.Errorf("broker: opening channel: %w", err)
	}
	defer func() { _ = ch.Close() }()

	for _, name := range []string{Exchange, DeadLetterExchange} {
		// durable=true so the topology survives a broker restart. An exchange that vanishes on
		// restart takes every binding with it, and publishes then succeed into nothing.
		if err := ch.ExchangeDeclare(name, amqp.ExchangeTopic, true, false, false, false, nil); err != nil {
			return fmt.Errorf("broker: declaring exchange %s: %w", name, err)
		}
	}
	return nil
}

// Channel opens a fresh AMQP channel. Channels are not safe for concurrent use, so each
// publisher and each consumer gets its own.
func (c *Conn) Channel() (*amqp.Channel, error) {
	ch, err := c.conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("broker: opening channel: %w", err)
	}
	return ch, nil
}

// DeclareConsumerQueue declares a durable queue for one consumer/event pair, its dead-letter
// queue, and the bindings for both.
func (c *Conn) DeclareConsumerQueue(service, event string) (string, error) {
	if !events.IsKnown(event) {
		return "", fmt.Errorf("broker: %q is not an event in the AsyncAPI contract", event)
	}

	ch, err := c.Channel()
	if err != nil {
		return "", err
	}
	defer func() { _ = ch.Close() }()

	queue := QueueName(service, event)
	dlq := DLQName(queue)

	// The DLQ is bound by the queue name rather than the event name, so a message arriving in a
	// DLQ unambiguously identifies which consumer failed on it — the same event failing in two
	// services lands in two distinguishable places.
	if _, err := ch.QueueDeclare(dlq, true, false, false, false, nil); err != nil {
		return "", fmt.Errorf("broker: declaring dlq %s: %w", dlq, err)
	}
	if err := ch.QueueBind(dlq, queue, DeadLetterExchange, false, nil); err != nil {
		return "", fmt.Errorf("broker: binding dlq %s: %w", dlq, err)
	}

	args := amqp.Table{
		"x-dead-letter-exchange": DeadLetterExchange,
		// Explicit, rather than inheriting the original routing key: without this the message
		// would arrive at the DLX keyed by event name and fan out to every service's DLQ for
		// that event, not just the one that actually failed.
		"x-dead-letter-routing-key": queue,
	}
	if _, err := ch.QueueDeclare(queue, true, false, false, false, args); err != nil {
		return "", fmt.Errorf("broker: declaring queue %s: %w", queue, err)
	}
	if err := ch.QueueBind(queue, event, Exchange, false, nil); err != nil {
		return "", fmt.Errorf("broker: binding queue %s to %s: %w", queue, event, err)
	}
	return queue, nil
}

// Close shuts the connection down.
func (c *Conn) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// IsClosed reports whether the underlying connection has gone away, so a caller can decide to
// reconnect rather than publishing into a dead socket.
func (c *Conn) IsClosed() bool {
	return c == nil || c.conn == nil || c.conn.IsClosed()
}

// Publisher publishes envelopes with publisher confirms enabled.
type Publisher struct {
	ch          *amqp.Channel
	confirms    chan amqp.Confirmation
	confirmWait time.Duration
}

// NewPublisher opens a confirm-mode channel.
//
// Confirms are mandatory, not a tuning option. The outbox relay may only mark a row published
// after the broker has acknowledged it; marking on a bare `Publish` call — which is fire and
// forget — would lose events whenever the broker dropped one, and the RPO=0 claim in
// docs/nfr.md would be false.
func NewPublisher(c *Conn) (*Publisher, error) {
	ch, err := c.Channel()
	if err != nil {
		return nil, err
	}
	if err := ch.Confirm(false); err != nil {
		_ = ch.Close()
		return nil, fmt.Errorf("broker: enabling publisher confirms: %w", err)
	}
	return &Publisher{
		ch:          ch,
		confirms:    ch.NotifyPublish(make(chan amqp.Confirmation, 1)),
		confirmWait: 10 * time.Second,
	}, nil
}

// Publish sends one envelope and blocks until the broker confirms it.
//
// Synchronous confirm-per-message costs throughput against a pipelined approach. That is an
// accepted trade: the relay publishes in batches off the request path, so the latency lands
// where nobody is waiting, and the alternative is bookkeeping that correlates confirms to rows
// asynchronously — more moving parts guarding the one guarantee that must not break.
func (p *Publisher) Publish(ctx context.Context, e events.Envelope, body []byte) error {
	pub := amqp.Publishing{
		ContentType: "application/json",
		// Persistent, or the broker discards it on restart and the durable queue was pointless.
		DeliveryMode:  amqp.Persistent,
		MessageId:     e.ID,
		Type:          e.Event,
		Timestamp:     e.OccurredAt,
		CorrelationId: e.CorrelationID,
		Body:          body,
	}

	// mandatory=true: fail loudly if no queue is bound, rather than discarding silently. An
	// event with no consumer bound is a topology bug, and the quiet version of that bug is
	// discovered days later by someone asking why no emails were sent.
	if err := p.ch.PublishWithContext(ctx, Exchange, e.Event, true, false, pub); err != nil {
		return fmt.Errorf("broker: publishing %s %s: %w", e.Event, e.ID, err)
	}

	select {
	case confirm, ok := <-p.confirms:
		if !ok {
			return fmt.Errorf("broker: confirm channel closed while publishing %s %s", e.Event, e.ID)
		}
		if !confirm.Ack {
			return fmt.Errorf("broker: nacked %s %s", e.Event, e.ID)
		}
		return nil
	case <-time.After(p.confirmWait):
		return fmt.Errorf("broker: timed out waiting for confirm of %s %s", e.Event, e.ID)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close releases the publisher's channel.
func (p *Publisher) Close() error {
	if p == nil || p.ch == nil {
		return nil
	}
	return p.ch.Close()
}

// redact strips credentials from an AMQP URL so a connection failure can be logged without
// putting the broker password in the log aggregator.
func redact(url string) string {
	at := -1
	for i := len(url) - 1; i >= 0; i-- {
		if url[i] == '@' {
			at = i
			break
		}
	}
	if at < 0 {
		return url
	}
	scheme := 0
	for i := 0; i+2 < len(url); i++ {
		if url[i] == ':' && url[i+1] == '/' && url[i+2] == '/' {
			scheme = i + 3
			break
		}
	}
	return url[:scheme] + "***@" + url[at+1:]
}
