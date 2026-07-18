//go:build contract

package events_test

// These tests are the reason hand-writing the payload structs is safe (ADR-0009).
//
// Codegen's real guarantee is not "a machine typed it" — it is "this cannot drift from the
// spec". These tests provide that guarantee directly, and provide it in the direction that
// actually matters: TestNoFieldDriftBetweenStructsAndSpec fails when someone edits the AsyncAPI
// without touching the structs, which is exactly the case a generator would have caught.
//
// If you are tempted to delete or weaken one of these to make CI pass, don't — the structs stop
// being trustworthy the moment these stop being strict.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/morisempai/wakewake/shared/contracts"
	"github.com/morisempai/wakewake/shared/contracts/events"
)

// payloadSchema digs the payload schema for one event out of the AsyncAPI document. Messages are
// `allOf: [Envelope, {properties: {event: const, payload: {...}}}]`, so the payload lives in
// whichever allOf member declares it.
func payloadSchema(t *testing.T, doc map[string]any, event string) map[string]any {
	t.Helper()

	comps, _ := doc["components"].(map[string]any)
	msgs, _ := comps["messages"].(map[string]any)
	msg, ok := msgs[event].(map[string]any)
	if !ok {
		t.Fatalf("event %q has no message in the AsyncAPI spec", event)
	}
	payloadNode, _ := msg["payload"].(map[string]any)
	allOf, _ := payloadNode["allOf"].([]any)
	for _, member := range allOf {
		m, _ := member.(map[string]any)
		props, _ := m["properties"].(map[string]any)
		if p, ok := props["payload"].(map[string]any); ok {
			return p
		}
	}
	t.Fatalf("event %q: no payload schema found in allOf", event)
	return nil
}

func loadSpec(t *testing.T) map[string]any {
	t.Helper()
	raw, err := contracts.Specs.ReadFile(contracts.AsyncAPIPath)
	if err != nil {
		t.Fatalf("reading embedded asyncapi: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing asyncapi: %v", err)
	}
	return doc
}

// jsonTags returns the json field names of a struct, excluding "-".
func jsonTags(t *testing.T, v any) map[string]bool {
	t.Helper()
	rt := reflect.TypeOf(v)
	for rt.Kind() == reflect.Ptr {
		rt = rt.Elem()
	}
	if rt.Kind() != reflect.Struct {
		t.Fatalf("expected struct, got %s", rt.Kind())
	}
	out := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}

// TestEveryContractEventHasAPrototype fails when an event is added to the AsyncAPI spec but not
// to this package's registry. Without it, a new event would simply be absent from Go with no
// test anywhere going red.
func TestEveryContractEventHasAPrototype(t *testing.T) {
	doc := loadSpec(t)
	comps, _ := doc["components"].(map[string]any)
	msgs, _ := comps["messages"].(map[string]any)

	for name := range msgs {
		if _, ok := events.Prototype(name); !ok {
			t.Errorf("event %q exists in the AsyncAPI spec but has no Go payload struct", name)
		}
	}
	if len(msgs) != len(events.All) {
		t.Errorf("spec defines %d events, events.All lists %d", len(msgs), len(events.All))
	}
}

// TestNoFieldDriftBetweenStructsAndSpec is the load-bearing test. It compares the property set
// of each payload schema against the json tags of the corresponding struct, in BOTH directions,
// so a field added or removed on either side fails.
func TestNoFieldDriftBetweenStructsAndSpec(t *testing.T) {
	doc := loadSpec(t)

	for _, event := range events.All {
		t.Run(event, func(t *testing.T) {
			proto, ok := events.Prototype(event)
			if !ok {
				t.Fatalf("no prototype registered for %q", event)
			}
			schema := payloadSchema(t, doc, event)
			props, _ := schema["properties"].(map[string]any)

			specFields := map[string]bool{}
			for name := range props {
				specFields[name] = true
			}
			goFields := jsonTags(t, proto)

			for name := range specFields {
				if !goFields[name] {
					t.Errorf("spec has field %q but the Go struct does not", name)
				}
			}
			for name := range goFields {
				if !specFields[name] {
					t.Errorf("Go struct has field %q but the spec does not", name)
				}
			}
		})
	}
}

// TestRequiredSpecFieldsAreNonPointer checks that a field the spec marks required is not modelled
// as a pointer, and that an optional/nullable one is. Getting this backwards is how a null
// reservation_id silently becomes "" — the distinction availability's compensation path needs.
func TestRequiredSpecFieldsAreNonPointer(t *testing.T) {
	doc := loadSpec(t)

	for _, event := range events.All {
		t.Run(event, func(t *testing.T) {
			proto, _ := events.Prototype(event)
			schema := payloadSchema(t, doc, event)

			required := map[string]bool{}
			if rs, ok := schema["required"].([]any); ok {
				for _, r := range rs {
					required[r.(string)] = true
				}
			}

			rt := reflect.TypeOf(proto).Elem()
			for i := 0; i < rt.NumField(); i++ {
				f := rt.Field(i)
				name := strings.Split(f.Tag.Get("json"), ",")[0]
				if name == "" || name == "-" {
					continue
				}
				isPtr := f.Type.Kind() == reflect.Ptr
				if required[name] && isPtr {
					t.Errorf("field %q is required by the spec but is a pointer in Go", name)
				}
				if !required[name] && !isPtr {
					t.Errorf("field %q is optional/nullable in the spec but is not a pointer in Go", name)
				}
			}
		})
	}
}

// TestUnknownPayloadFieldsAreIgnored pins the forward-compatibility rule: a producer must be able
// to add an optional field without a coordinated deploy. If PayloadOf ever gains
// DisallowUnknownFields, this fails.
func TestUnknownPayloadFieldsAreIgnored(t *testing.T) {
	raw := []byte(`{
		"reservation_id":"01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d8e",
		"resource_id":"01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d8f",
		"booking_id":"01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d80",
		"starts_at":"2026-08-01T10:00:00Z",
		"ends_at":"2026-08-01T11:00:00Z",
		"expires_at":"2026-08-01T09:15:00Z",
		"a_field_from_the_future":"should be ignored"
	}`)

	e := events.Envelope{
		Event:         events.ReservationCreated,
		Version:       events.SchemaVersion,
		ID:            "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d8e",
		OccurredAt:    time.Now().UTC(),
		CorrelationID: "test-correlation",
		Payload:       json.RawMessage(raw),
	}

	got, err := events.PayloadOf[events.ReservationCreatedPayload](e)
	if err != nil {
		t.Fatalf("unknown field caused a decode error, breaking forward compatibility: %v", err)
	}
	if got.ReservationID != "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d8e" {
		t.Errorf("known fields decoded wrong: %+v", got)
	}
}

// TestEnvelopeRoundTrip pins the flat wire shape. If someone nests payload under an envelope
// object, the marshalled keys change and this fails.
func TestEnvelopeRoundTrip(t *testing.T) {
	payload := events.ReservationCreatedPayload{
		ReservationID: "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d8e",
		ResourceID:    "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d8f",
		BookingID:     "01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d80",
		StartsAt:      time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
		EndsAt:        time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC),
		ExpiresAt:     time.Date(2026, 8, 1, 9, 15, 0, 0, time.UTC),
	}

	e, err := events.New(
		events.ReservationCreated,
		"01912d5a-7f3e-7c1a-9b2e-3f4a5b6c7d8e",
		time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC),
		"corr-1",
		payload,
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var flat map[string]json.RawMessage
	if err := json.Unmarshal(raw, &flat); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	for _, key := range []string{"event", "version", "id", "occurred_at", "correlation_id", "payload"} {
		if _, ok := flat[key]; !ok {
			t.Errorf("envelope is missing top-level key %q — the wire shape must stay flat", key)
		}
	}
	if len(flat) != 6 {
		t.Errorf("envelope has %d top-level keys, want exactly 6: %v", len(flat), flat)
	}

	back, err := events.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got, err := events.PayloadOf[events.ReservationCreatedPayload](back)
	if err != nil {
		t.Fatalf("PayloadOf: %v", err)
	}
	if !reflect.DeepEqual(got, payload) {
		t.Errorf("round trip changed the payload:\n got %+v\nwant %+v", got, payload)
	}
}
