// Package domain holds the notification service's business logic: what a booking-confirmation
// email contains, and how a recipient address is redacted before it reaches a log line.
//
// It imports only the standard library. In particular it must NOT import shared/platform (the
// service's hard rule) nor any driver (pgx, amqp) nor the event envelope: SMTP, Postgres, the
// broker, and the wire format are all infrastructure the outer layers supply through the ports
// declared here. Keeping the domain driver-free is what makes it unit-testable with plain fakes
// and keeps the composition and redaction rules in exactly one place.
package domain

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Recipient is an email address. It is a distinct type, not a bare string, so that a mistaken
// attempt to log the whole thing is at least visible in review — and so Redact lives right next
// to the value it protects.
//
// The rule for this service (CLAUDE.md, AC4): the raw address may travel to the mailer, but it
// must NEVER appear in a structured log line. Log Redact(), never Address().
type Recipient string

// Address returns the raw address, for the mailer only.
func (r Recipient) Address() string { return string(r) }

// Redact returns a form safe to log: the first character of the local part, then "***", then the
// domain — enough to tell "roughly which mailbox at which provider" during an incident without
// disclosing the address. Anything without a single '@' is fully hidden rather than guessed at.
func (r Recipient) Redact() string {
	at := strings.LastIndex(string(r), "@")
	if at <= 0 {
		return "***"
	}
	local, domain := string(r)[:at], string(r)[at+1:]
	if domain == "" {
		return "***"
	}
	// One rune of the local part is retained. Using the rune boundary rather than a byte keeps a
	// multibyte first character intact instead of emitting half of it.
	first := []rune(local)[0]
	return fmt.Sprintf("%c***@%s", first, domain)
}

// Confirmation is the set of booking facts needed to compose the email. It is decoded by the
// events layer from the BookingConfirmed payload; the domain never sees the envelope itself.
//
// It deliberately does NOT carry an email address: the recipient is resolved separately through
// RecipientResolver, because BookingConfirmed carries no PII (contract x-notes, NFR-4).
type Confirmation struct {
	BookingID  string
	CustomerID string
	StartsAt   time.Time
	EndsAt     time.Time
	TotalMinor int64
	Currency   string
}

// Message is a composed email ready for the mailer.
type Message struct {
	To      Recipient
	Subject string
	Body    string
}

// ChannelEmail is the only delivery channel in the slice. SMS/push are deferred; marketing
// channels are out of scope entirely (CLAUDE.md).
const ChannelEmail = "email"

// SentRecord is the audit/idempotency row written when a confirmation is sent, keyed on the
// envelope id (EventID). It carries the recipient in REDACTED form only — never the raw address —
// so persisting it does not put PII at rest in the owned database (NFR-4). It is a plain value
// with no driver types, so the domain stays import-clean; infra maps it onto the SQL row.
type SentRecord struct {
	EventID           string
	BookingID         string
	CustomerID        string
	Channel           string
	RecipientRedacted string
	Subject           string
	CorrelationID     string
}

// RecipientResolver turns a customer id into an address to email.
//
// It is a PORT with no real implementation in this slice: BookingConfirmed carries no email and
// there is no customer/identity service to ask (KNOWN GAP, issue #19). infra ships a clearly
// labelled DEV stub; a production resolver is out of scope and flagged in the PR.
type RecipientResolver interface {
	Resolve(ctx context.Context, customerID string) (Recipient, error)
}

// Mailer delivers a composed message. infra implements it against SMTP (Mailhog in dev).
type Mailer interface {
	Send(ctx context.Context, msg Message) error
}

// ComposeConfirmation builds the booking-confirmation email. It is a pure function of its inputs
// so its output is stable and testable, and it holds the one copy of the transactional email's
// wording. Marketing/campaign content is explicitly out of scope (CLAUDE.md).
func ComposeConfirmation(c Confirmation, to Recipient) Message {
	var b strings.Builder
	fmt.Fprintf(&b, "Your booking %s is confirmed.\n\n", c.BookingID)
	fmt.Fprintf(&b, "When:  %s\n", formatWindow(c.StartsAt, c.EndsAt))
	fmt.Fprintf(&b, "Total: %s\n\n", FormatMinor(c.TotalMinor, c.Currency))
	b.WriteString("Thank you for booking with us.\n")

	return Message{
		To:      to,
		Subject: "Your booking is confirmed",
		Body:    b.String(),
	}
}

// formatWindow renders the reservation window in a fixed, unambiguous UTC form. A friendlier,
// locale-aware format is a presentation concern that belongs with a real templating story, not
// this slice — kept plain and deterministic on purpose.
func formatWindow(start, end time.Time) string {
	const layout = "2006-01-02 15:04 MST"
	return fmt.Sprintf("%s – %s", start.UTC().Format(layout), end.UTC().Format(layout))
}

// FormatMinor renders an integer count of a currency's minor unit as a two-decimal major amount,
// e.g. 15000 EUR → "150.00 EUR". Money is never a float in this system (see the contract's
// payload notes); the formatting here is integer arithmetic, not a float conversion.
//
// ASSUMPTION (flagged in the PR): a 2-digit minor unit. That holds for EUR/USD/GBP, the currencies
// this slice uses, but not for zero-decimal currencies like JPY. A currency-aware exponent is a
// follow-up, not something to guess at here.
func FormatMinor(minor int64, currency string) string {
	neg := minor < 0
	if neg {
		minor = -minor
	}
	major := minor / 100
	frac := minor % 100
	sign := ""
	if neg {
		sign = "-"
	}
	return fmt.Sprintf("%s%d.%02d %s", sign, major, frac, currency)
}
