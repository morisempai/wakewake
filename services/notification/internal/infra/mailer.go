package infra

import (
	"context"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/morisempai/wakewake/services/notification/internal/domain"
)

// SMTPMailer delivers messages over plain SMTP. In dev the sink is Mailhog, which accepts mail on
// port 1025 with no authentication and no TLS and exposes it over an HTTP API for the integration
// test to read. A real relay would add auth and STARTTLS; those are config, not a code change, and
// are out of scope for this slice (flagged in the PR).
type SMTPMailer struct {
	addr string // host:port
	from string
}

// NewSMTPMailer builds a mailer targeting host:port, sending from `from`.
func NewSMTPMailer(host string, port int, from string) *SMTPMailer {
	return &SMTPMailer{
		addr: net.JoinHostPort(host, strconv.Itoa(port)),
		from: from,
	}
}

// Send transmits one message.
//
// net/smtp.SendMail has no context parameter, so cancellation is honoured only up to the point of
// dialling: a ctx already done short-circuits before we open a connection. This is acceptable for
// a fast local relay; a context-aware SMTP client is a follow-up if this ever talks to a slow
// remote MTA.
func (m *SMTPMailer) Send(ctx context.Context, msg domain.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw := m.render(msg)
	if err := smtp.SendMail(m.addr, nil, m.from, []string{msg.To.Address()}, raw); err != nil {
		// The error may include the recipient address; wrap with the REDACTED form so a logged
		// send failure never leaks the address (AC4).
		return fmt.Errorf("infra: sending mail to %s via %s: %w", msg.To.Redact(), m.addr, err)
	}
	return nil
}

// render builds the RFC 5322 message bytes. CRLF line endings, a plain-text body, and a Date
// header — the minimum a well-formed message needs for Mailhog (and a real MTA) to accept it.
func (m *SMTPMailer) render(msg domain.Message) []byte {
	var b strings.Builder
	writeHeader(&b, "From", m.from)
	writeHeader(&b, "To", msg.To.Address())
	writeHeader(&b, "Subject", msg.Subject)
	writeHeader(&b, "Date", time.Now().UTC().Format(time.RFC1123Z))
	writeHeader(&b, "MIME-Version", "1.0")
	writeHeader(&b, "Content-Type", "text/plain; charset=utf-8")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(msg.Body, "\n", "\r\n"))
	return []byte(b.String())
}

func writeHeader(b *strings.Builder, key, value string) {
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteString("\r\n")
}
