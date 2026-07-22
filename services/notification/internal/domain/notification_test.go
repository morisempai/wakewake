package domain

import (
	"strings"
	"testing"
	"time"
)

// AC4 (no PII in logs) rests on this: a recipient address must have a redacted form that keeps
// enough to correlate ("which domain, roughly which mailbox") while never revealing the address.
func TestRecipientRedactHidesTheLocalPartButKeepsTheDomain(t *testing.T) {
	cases := []struct {
		name string
		in   Recipient
		want string
	}{
		{"ordinary address", "customer-42@example.test", "c***@example.test"},
		{"single char local", "a@example.test", "a***@example.test"},
		{"no at sign is fully hidden", "not-an-address", "***"},
		{"empty is fully hidden", "", "***"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.Redact()
			if got != tc.want {
				t.Errorf("Redact() = %q, want %q", got, tc.want)
			}
			// The whole point: the raw address must not survive redaction.
			if tc.in.Address() != "" && strings.Contains(got, tc.in.Address()) {
				t.Errorf("Redact() = %q still contains the raw address %q", got, tc.in.Address())
			}
		})
	}
}

func TestComposeConfirmationEmailCarriesTheBookingFacts(t *testing.T) {
	to := Recipient("customer-7@example.test")
	start := time.Date(2026, 8, 1, 18, 30, 0, 0, time.UTC)
	conf := Confirmation{
		BookingID:  "b-123",
		CustomerID: "cust-7",
		StartsAt:   start,
		EndsAt:     start.Add(time.Hour),
		TotalMinor: 15000,
		Currency:   "EUR",
	}

	msg := ComposeConfirmation(conf, to)

	if msg.To != to {
		t.Errorf("To = %q, want %q", msg.To, to)
	}
	if !strings.Contains(strings.ToLower(msg.Subject), "confirmed") {
		t.Errorf("Subject %q does not say the booking is confirmed", msg.Subject)
	}
	// The body must let the customer identify which booking and when, and what they paid.
	for _, want := range []string{"b-123", "150.00", "EUR"} {
		if !strings.Contains(msg.Body, want) {
			t.Errorf("Body does not contain %q:\n%s", want, msg.Body)
		}
	}
}

// The composed message must never embed the customer_id or any resolver internals in a way that
// leaks — but more importantly, composition is deterministic so the tests above are stable.
func TestComposeConfirmationIsDeterministic(t *testing.T) {
	to := Recipient("customer-7@example.test")
	conf := Confirmation{BookingID: "b-1", StartsAt: time.Unix(0, 0).UTC(), EndsAt: time.Unix(3600, 0).UTC(), TotalMinor: 100, Currency: "USD"}

	if ComposeConfirmation(conf, to) != ComposeConfirmation(conf, to) {
		t.Error("ComposeConfirmation is not deterministic for identical input")
	}
}

func TestFormatMinorRendersTwoDecimalPlaces(t *testing.T) {
	cases := []struct {
		minor    int64
		currency string
		want     string
	}{
		{15000, "EUR", "150.00 EUR"},
		{5, "USD", "0.05 USD"},
		{0, "GBP", "0.00 GBP"},
		{100, "EUR", "1.00 EUR"},
	}
	for _, tc := range cases {
		got := FormatMinor(tc.minor, tc.currency)
		if got != tc.want {
			t.Errorf("FormatMinor(%d, %q) = %q, want %q", tc.minor, tc.currency, got, tc.want)
		}
	}
}
