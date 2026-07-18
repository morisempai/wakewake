// Package events is the wire form of every event in contracts/asyncapi/booking-events.yaml.
//
// This package is the one place the envelope shape is defined. It exists in shared/ because the
// producer and the consumer of any given event live in *different services* — there is no single
// service that could own it, and five independent definitions would differ in ways that pass
// every local test and fail only on the bus.
//
// It depends on nothing but the standard library and google/uuid, deliberately: internal/domain
// is allowed to import this package (so a domain can build an event payload) but is forbidden
// from importing shared/platform, which drags in pgx, amqp, and otel. Keep it that way.
package events

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SchemaVersion is the current envelope version. Per the AsyncAPI contract, adding an optional
// field does NOT bump this; removing a field, tightening a type, or changing a meaning does, and
// that is a breaking change requiring a new version and a deprecation window.
const SchemaVersion = 1

// maxCorrelationIDLen mirrors the contract's `maxLength: 128`.
const maxCorrelationIDLen = 128

// Envelope is the wire form shared by all seven events.
//
// The shape is FLAT: five envelope fields with `payload` as a sibling key, not nested under an
// "envelope" object. This matches the AsyncAPI `allOf: [Envelope, {payload}]` composition. Do not
// reorder or rename the json tags — they are the contract.
type Envelope struct {
	// Event is the event name and doubles as the AMQP routing key.
	Event string `json:"event"`

	// Version is the envelope schema version, >= 1.
	Version int `json:"version"`

	// ID is a UUIDv7 and is THE idempotency key: consumers dedupe on it, and a redelivery
	// carries the same value. The relay republishes this id rather than minting a fresh one.
	ID string `json:"id"`

	// OccurredAt is when the fact happened — the database transaction time — NOT when the event
	// was published or received. Outbox relay lag means publish time can be much later, so these
	// are genuinely different clocks and the contract picks this one.
	OccurredAt time.Time `json:"occurred_at"`

	// CorrelationID is propagated unchanged across every call, event, log line, and span.
	CorrelationID string `json:"correlation_id"`

	// Payload is left raw so a consumer can validate and decode only the events it handles.
	Payload json.RawMessage `json:"payload"`
}

// Validate checks the envelope invariants the contract states. It deliberately does not look at
// the payload: that is the concern of PayloadOf and of each event's own schema.
func (e Envelope) Validate() error {
	if e.Event == "" {
		return fmt.Errorf("envelope: event is empty")
	}
	if !IsKnown(e.Event) {
		return fmt.Errorf("envelope: unknown event %q", e.Event)
	}
	if e.Version < 1 {
		return fmt.Errorf("envelope: version must be >= 1, got %d", e.Version)
	}
	if _, err := uuid.Parse(e.ID); err != nil {
		return fmt.Errorf("envelope: id %q is not a uuid: %w", e.ID, err)
	}
	if e.OccurredAt.IsZero() {
		return fmt.Errorf("envelope: occurred_at is zero")
	}
	if e.CorrelationID == "" {
		return fmt.Errorf("envelope: correlation_id is empty")
	}
	if len(e.CorrelationID) > maxCorrelationIDLen {
		return fmt.Errorf("envelope: correlation_id exceeds %d chars", maxCorrelationIDLen)
	}
	if len(e.Payload) == 0 {
		return fmt.Errorf("envelope: payload is empty")
	}
	return nil
}

// Parse decodes a raw message body and validates it.
//
// A body that fails here is malformed, not merely unhandled: retrying it will never succeed, so
// callers should dead-letter it rather than requeue.
func Parse(body []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(body, &e); err != nil {
		return Envelope{}, fmt.Errorf("envelope: malformed json: %w", err)
	}
	if err := e.Validate(); err != nil {
		return Envelope{}, err
	}
	return e, nil
}

// PayloadOf decodes the envelope payload into T.
//
// It deliberately does NOT use DisallowUnknownFields. The contract requires consumers to ignore
// unknown payload fields so that a producer can add an optional field without a coordinated
// deploy — rejecting them here would turn every additive, non-breaking change into an outage.
func PayloadOf[T any](e Envelope) (T, error) {
	var v T
	if err := json.Unmarshal(e.Payload, &v); err != nil {
		return v, fmt.Errorf("envelope: decoding %s payload: %w", e.Event, err)
	}
	return v, nil
}

// New builds an envelope around a payload. The caller supplies id and occurredAt rather than this
// package generating them, because both come from the database in the outbox path: occurredAt is
// the transaction timestamp, and generating it here from the app clock would be a different,
// drifting clock than the one the contract specifies.
func New(event, id string, occurredAt time.Time, correlationID string, payload any) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("envelope: marshalling %s payload: %w", event, err)
	}
	e := Envelope{
		Event:         event,
		Version:       SchemaVersion,
		ID:            id,
		OccurredAt:    occurredAt.UTC(),
		CorrelationID: correlationID,
		Payload:       raw,
	}
	if err := e.Validate(); err != nil {
		return Envelope{}, err
	}
	return e, nil
}
