package stripe

import (
	"encoding/json"
	"fmt"
)

// Event names payment cares about. Any other type is acknowledged and ignored (payment.yaml).
const (
	EventPaymentIntentSucceeded = "payment_intent.succeeded"
	EventPaymentIntentFailed    = "payment_intent.payment_failed"
)

// Event is the subset of a Stripe Event the webhook acts on: its id (for dedup), its type, the
// PaymentIntent id it concerns, and, on a failure, the decline code. Everything else in the Stripe
// payload — including anything that could resemble card data — is ignored by construction.
type Event struct {
	ID              string
	Type            string
	PaymentIntentID string
	FailureCode     string
}

// wireEvent mirrors only the fields we read. Unknown fields are ignored (no DisallowUnknownFields),
// matching the contract's forward-compatibility rule.
type wireEvent struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		Object struct {
			ID               string `json:"id"`
			LastPaymentError *struct {
				Code string `json:"code"`
			} `json:"last_payment_error"`
		} `json:"object"`
	} `json:"data"`
}

// ParseEvent decodes a verified Stripe event body. It is called only AFTER the signature has been
// verified, so the bytes are trusted; a decode failure here is a malformed event, mapped to 400.
func ParseEvent(payload []byte) (Event, error) {
	var w wireEvent
	if err := json.Unmarshal(payload, &w); err != nil {
		return Event{}, fmt.Errorf("stripe: malformed event json: %w", err)
	}
	if w.ID == "" || w.Type == "" {
		return Event{}, fmt.Errorf("stripe: event missing id or type")
	}

	e := Event{
		ID:              w.ID,
		Type:            w.Type,
		PaymentIntentID: w.Data.Object.ID,
	}
	if w.Data.Object.LastPaymentError != nil {
		e.FailureCode = w.Data.Object.LastPaymentError.Code
	}
	return e, nil
}

// IsPaymentOutcome reports whether this event type is one payment turns into a PaymentSucceeded or
// PaymentFailed. Everything else is acknowledged with 200 and ignored.
func (e Event) IsPaymentOutcome() bool {
	return e.Type == EventPaymentIntentSucceeded || e.Type == EventPaymentIntentFailed
}

// Succeeded reports whether this is the success outcome.
func (e Event) Succeeded() bool {
	return e.Type == EventPaymentIntentSucceeded
}
