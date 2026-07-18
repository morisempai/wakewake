// Package outbox implements the transactional outbox from ADR-0002.
//
// The problem it solves: a service that writes its database and then publishes to RabbitMQ is
// performing a dual write. If it crashes between the two, the state change is durable and the
// event is gone forever — no retry will produce it, because nothing remembers it was owed. If it
// publishes first and the transaction then rolls back, it has announced a fact that never
// happened. Neither is compatible with the RPO=0 target in docs/nfr.md.
//
// The fix: write the event into an outbox table in the SAME transaction as the state change, so
// the two commit or roll back together. A relay then reads unpublished rows and forwards them.
// The event may be published more than once — hence at-least-once delivery and the contractual
// requirement that consumers dedupe on the envelope id — but it can never be lost, and it can
// never describe a transaction that was rolled back.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/morisempai/wakewake/shared/contracts/events"
	"github.com/morisempai/wakewake/shared/platform/correlation"
)

// Record is one event to be staged for publication.
type Record struct {
	// Event is the event name and the AMQP routing key, e.g. events.ReservationCreated.
	Event string

	// Version defaults to events.SchemaVersion when zero.
	Version int

	// AggregateType and AggregateID identify what the event is about ("reservation", uuid).
	// They are not part of the wire envelope — they exist so an operator debugging a stuck
	// outbox can ask "what happened to booking X" without parsing every payload.
	AggregateType string
	AggregateID   string

	// Payload is marshalled to jsonb. Pass the typed struct from shared/contracts/events.
	Payload any

	// CorrelationID defaults to the one in the context when empty.
	CorrelationID string
}

// Meta is the envelope identity of a staged event, returned so callers can log it and tests can
// assert on it.
type Meta struct {
	// ID is the envelope id consumers will dedupe on.
	ID string

	// OccurredAt is the database transaction timestamp, not the app clock.
	OccurredAt time.Time
}

// Tx is the narrow surface Enqueue needs. pgx.Tx satisfies it.
//
// It is an interface rather than a concrete *pgx.Tx so a caller can pass a wrapper (a test
// double, an instrumented tx) without this package caring.
type Tx interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const insertSQL = `
INSERT INTO outbox (id, event, version, aggregate_type, aggregate_id, payload, correlation_id, occurred_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, now())
RETURNING id, occurred_at`

// Enqueue stages one event inside the caller's transaction.
//
// It MUST be called with a real transaction that also carries the state change. Passing a pool
// works and the event will publish, but the atomicity guarantee — the entire reason this package
// exists — is silently gone. There is no way for this function to detect that, which is why it
// is stated here loudly.
//
// occurred_at comes from the database's now(), which inside a transaction is the transaction
// timestamp. This is deliberate and load-bearing:
//
//   - The AsyncAPI contract defines occurred_at as when the fact happened, not when it was
//     published. Relay lag can be seconds; the two are genuinely different instants.
//   - Using time.Now() in Go would read a different clock than the one ordering the database's
//     own writes, and those clocks drift. Cross-service event ordering built on a drifting clock
//     produces timelines that are subtly, unreproducibly wrong.
//   - Every row staged in one transaction therefore shares one occurred_at, which is the correct
//     reading of "these facts happened when this transaction happened".
func Enqueue(ctx context.Context, tx Tx, rec Record) (Meta, error) {
	if !events.IsKnown(rec.Event) {
		return Meta{}, fmt.Errorf("outbox: %q is not an event in the AsyncAPI contract", rec.Event)
	}
	if rec.AggregateID == "" {
		return Meta{}, fmt.Errorf("outbox: %s has no aggregate_id", rec.Event)
	}

	version := rec.Version
	if version == 0 {
		version = events.SchemaVersion
	}

	corrID := rec.CorrelationID
	if corrID == "" {
		corrID = correlation.FromContext(ctx)
	}
	if corrID == "" {
		// Better a fresh ID than an empty column: the envelope requires a non-empty
		// correlation_id, and a missing one would fail validation at publish time — i.e. after
		// the transaction has already committed, where nothing can be done about it.
		corrID = correlation.NewID()
	}

	payload, err := json.Marshal(rec.Payload)
	if err != nil {
		return Meta{}, fmt.Errorf("outbox: marshalling %s payload: %w", rec.Event, err)
	}

	// UUIDv7 so the outbox's natural ordering matches time, which is what makes the relay's
	// (occurred_at, id) claim query a clean index scan.
	id, err := uuid.NewV7()
	if err != nil {
		return Meta{}, fmt.Errorf("outbox: generating event id: %w", err)
	}

	var m Meta
	row := tx.QueryRow(ctx, insertSQL,
		id.String(), rec.Event, version, rec.AggregateType, rec.AggregateID, payload, corrID)
	if err := row.Scan(&m.ID, &m.OccurredAt); err != nil {
		return Meta{}, fmt.Errorf("outbox: staging %s: %w", rec.Event, err)
	}
	return m, nil
}
