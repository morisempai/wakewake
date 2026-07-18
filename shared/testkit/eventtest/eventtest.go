// Package eventtest provides the assertions every consumer test needs.
//
// testing-standards requires EVERY consumer test to cover duplicate delivery (idempotency),
// unknown payload fields (forward compatibility), and a dependency being down. Five service
// agents hand-rolling those means five subtly different and mostly weaker versions — and
// "weaker" here means the idempotency guarantee is untested while appearing tested.
//
// AssertValidAgainstSpec is the one that makes hand-written payload structs safe (ADR-0009): it
// validates real emitted JSON against the real AsyncAPI schema, rather than against another
// hand-written expectation that shares the same mistake.
package eventtest

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/morisempai/wakewake/shared/contracts"
	"github.com/morisempai/wakewake/shared/contracts/events"
)

// AssertEnvelope checks the envelope invariants and the fields a test usually cares about.
func AssertEnvelope(t *testing.T, raw []byte, wantEvent, wantCorrelationID string) events.Envelope {
	t.Helper()

	e, err := events.Parse(raw)
	if err != nil {
		t.Fatalf("envelope did not parse: %v\nraw: %s", err, raw)
	}
	if e.Event != wantEvent {
		t.Errorf("event = %q, want %q", e.Event, wantEvent)
	}
	if wantCorrelationID != "" && e.CorrelationID != wantCorrelationID {
		t.Errorf("correlation_id = %q, want %q — it must propagate unchanged",
			e.CorrelationID, wantCorrelationID)
	}
	if e.OccurredAt.IsZero() {
		t.Error("occurred_at is zero")
	}
	if e.OccurredAt.After(time.Now().Add(time.Minute)) {
		// A far-future occurred_at usually means the app clock was used instead of the database
		// transaction time, which the contract forbids.
		t.Errorf("occurred_at %s is in the future — is this the app clock rather than the DB's?",
			e.OccurredAt)
	}
	return e
}

// AssertValidAgainstSpec validates a payload against the AsyncAPI schema for that event.
//
// It checks required properties are present and that no property appears which the spec does not
// define. That second direction is the one that catches a producer inventing a field, which then
// silently becomes load-bearing for a consumer.
func AssertValidAgainstSpec(t *testing.T, event string, payload []byte) {
	t.Helper()

	schema := payloadSchema(t, event)

	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("payload is not a JSON object: %v\nraw: %s", err, payload)
	}

	props, _ := schema["properties"].(map[string]any)
	if required, ok := schema["required"].([]any); ok {
		for _, r := range required {
			name, _ := r.(string)
			if _, present := got[name]; !present {
				t.Errorf("payload for %s is missing required field %q", event, name)
			}
		}
	}
	for name := range got {
		if _, defined := props[name]; !defined {
			t.Errorf("payload for %s has field %q that the AsyncAPI spec does not define", event, name)
		}
	}
}

// WithUnknownField injects an extra key into a payload, for the forward-compatibility case.
//
// A consumer that rejects this would force every additive producer change into a coordinated
// deploy across services — which is exactly what the contract's "consumers MUST ignore unknown
// payload fields" rule exists to prevent.
func WithUnknownField(t *testing.T, raw []byte, key string, value any) []byte {
	t.Helper()

	var env map[string]json.RawMessage
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("eventtest: envelope is not JSON: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(env["payload"], &payload); err != nil {
		t.Fatalf("eventtest: payload is not a JSON object: %v", err)
	}
	payload[key] = value

	updated, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("eventtest: re-marshalling payload: %v", err)
	}
	env["payload"] = updated

	out, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("eventtest: re-marshalling envelope: %v", err)
	}
	return out
}

// Envelope builds a valid envelope around a payload, for tests that need to feed a consumer.
func Envelope(t *testing.T, event string, payload any, correlationID string) []byte {
	t.Helper()

	if correlationID == "" {
		correlationID = "test-correlation-id"
	}
	e, err := events.New(event, newUUIDv7(t), time.Now().UTC(), correlationID, payload)
	if err != nil {
		t.Fatalf("eventtest: building envelope: %v", err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("eventtest: marshalling envelope: %v", err)
	}
	return raw
}

// payloadSchema extracts one event's payload schema from the embedded AsyncAPI document.
func payloadSchema(t *testing.T, event string) map[string]any {
	t.Helper()

	raw, err := contracts.Specs.ReadFile(contracts.AsyncAPIPath)
	if err != nil {
		t.Fatalf("eventtest: reading embedded asyncapi: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("eventtest: parsing asyncapi: %v", err)
	}

	comps, _ := doc["components"].(map[string]any)
	msgs, _ := comps["messages"].(map[string]any)
	msg, ok := msgs[event].(map[string]any)
	if !ok {
		t.Fatalf("eventtest: %q has no message in the AsyncAPI spec", event)
	}
	node, _ := msg["payload"].(map[string]any)
	allOf, _ := node["allOf"].([]any)
	for _, member := range allOf {
		m, _ := member.(map[string]any)
		props, _ := m["properties"].(map[string]any)
		if p, ok := props["payload"].(map[string]any); ok {
			return p
		}
	}
	t.Fatalf("eventtest: %q: no payload schema found", event)
	return nil
}

func newUUIDv7(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("eventtest: generating uuid: %v", err)
	}
	return id.String()
}
