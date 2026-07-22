package domain

import (
	"testing"

	"github.com/morisempai/wakewake/shared/contracts/events"
)

func pendingPayment() Payment {
	return Payment{
		ID: testPaymentID, BookingID: testBookingID, Status: StatusPending,
		AmountMinor: 12000, Currency: "EUR", Provider: providerStripe, ProviderPaymentID: testIntentID,
	}
}

func TestMarkSucceededPromotesAndEmitsPaymentSucceeded_Issue18_AC4(t *testing.T) {
	t.Parallel()

	next, emissions, err := pendingPayment().MarkSucceeded(now)
	if err != nil {
		t.Fatalf("MarkSucceeded returned %v", err)
	}
	if next.Status != StatusSucceeded {
		t.Errorf("status = %q, want succeeded", next.Status)
	}
	if next.FailureCode != nil {
		t.Errorf("a succeeded payment must carry no failure code, got %v", *next.FailureCode)
	}
	if len(emissions) != 1 || emissions[0].Event != events.PaymentSucceeded {
		t.Fatalf("emissions = %+v, want one PaymentSucceeded", emissions)
	}

	payload, ok := emissions[0].Payload.(events.PaymentSucceededPayload)
	if !ok {
		t.Fatalf("payload type = %T, want PaymentSucceededPayload", emissions[0].Payload)
	}
	if payload.PaymentID != testPaymentID || payload.BookingID != testBookingID ||
		payload.AmountMinor != 12000 || payload.Currency != "EUR" || payload.ProviderPaymentID != testIntentID {
		t.Errorf("PaymentSucceeded payload wrong: %+v", payload)
	}
}

func TestMarkFailedFailsAndEmitsPaymentFailed_Issue18_AC4(t *testing.T) {
	t.Parallel()

	next, emissions, err := pendingPayment().MarkFailed("card_declined", now)
	if err != nil {
		t.Fatalf("MarkFailed returned %v", err)
	}
	if next.Status != StatusFailed {
		t.Errorf("status = %q, want failed", next.Status)
	}
	if next.FailureCode == nil || *next.FailureCode != "card_declined" {
		t.Errorf("failure_code = %v, want card_declined", next.FailureCode)
	}
	if len(emissions) != 1 || emissions[0].Event != events.PaymentFailed {
		t.Fatalf("emissions = %+v, want one PaymentFailed", emissions)
	}
	payload := emissions[0].Payload.(events.PaymentFailedPayload)
	if payload.FailureCode != "card_declined" || payload.PaymentID != testPaymentID {
		t.Errorf("PaymentFailed payload wrong: %+v", payload)
	}
}

// The contract requires a non-empty failure_code; a webhook that omits one still describes a real
// failure, so a stable fallback is used rather than staging an event that fails validation.
func TestMarkFailedSuppliesAFallbackFailureCode_Issue18(t *testing.T) {
	t.Parallel()

	next, emissions, _ := pendingPayment().MarkFailed("", now)
	if next.FailureCode == nil || *next.FailureCode == "" {
		t.Fatal("failed payment has no failure code")
	}
	if emissions[0].Payload.(events.PaymentFailedPayload).FailureCode == "" {
		t.Error("PaymentFailed payload has an empty failure_code, which the contract forbids")
	}
}

// Terminal states are idempotent: a duplicate success emits nothing a second time, and a
// contradictory outcome for a settled payment is accepted as a no-op rather than flipping it.
func TestTerminalPaymentsAreIdempotent_Issue18_AC4(t *testing.T) {
	t.Parallel()

	succeeded := pendingPayment()
	succeeded.Status = StatusSucceeded

	if _, emissions, _ := succeeded.MarkSucceeded(now); len(emissions) != 0 {
		t.Errorf("re-succeeding emitted %d events, want 0", len(emissions))
	}
	if next, emissions, _ := succeeded.MarkFailed("card_declined", now); next.Status != StatusSucceeded || len(emissions) != 0 {
		t.Errorf("a failure for a succeeded payment changed it (status %q, %d emissions); want an accepted no-op", next.Status, len(emissions))
	}
}
