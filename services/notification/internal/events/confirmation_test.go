package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/morisempai/wakewake/shared/contracts/events"
	"github.com/morisempai/wakewake/shared/platform/correlation"
	"github.com/morisempai/wakewake/shared/platform/logging"

	"github.com/morisempai/wakewake/services/notification/internal/domain"
)

var errSMTP = errors.New("smtp: connection refused")

// --- fakes -----------------------------------------------------------------
//
// The handler depends only on ports (Recorder, RecipientResolver, Mailer), so its logic is
// exercised here with no Postgres, no SMTP, and a nil transaction — the recorder fake ignores it.
// The integration tests cover the real database and real Mailhog (AC1, AC2, AC5).

type fakeRecorder struct {
	mu       sync.Mutex
	records  []domain.SentRecord
	inserted bool // what RecordSent reports; true = a fresh insert
	err      error
}

func (f *fakeRecorder) RecordSent(_ context.Context, _ pgx.Tx, r domain.SentRecord) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, r)
	if f.err != nil {
		return false, f.err
	}
	return f.inserted, nil
}

type fakeResolver struct{ addr domain.Recipient }

func (f fakeResolver) Resolve(context.Context, string) (domain.Recipient, error) { return f.addr, nil }

type fakeMailer struct {
	mu   sync.Mutex
	sent []domain.Message
	err  error
}

func (f *fakeMailer) Send(_ context.Context, msg domain.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, msg)
	return nil
}

func (f *fakeMailer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// confirmedEnvelope builds a BookingConfirmed envelope, optionally injecting an unknown payload
// field. It avoids shared/testkit on purpose: importing it into a non-integration test would drag
// testcontainers into the default go.sum.
func confirmedEnvelope(t *testing.T, customerID, correlationID string, extra map[string]any) events.Envelope {
	t.Helper()

	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	start := time.Date(2026, 8, 1, 18, 0, 0, 0, time.UTC)

	payload := map[string]any{
		"booking_id":     newUUID(t),
		"customer_id":    customerID,
		"product_id":     newUUID(t),
		"resource_id":    newUUID(t),
		"reservation_id": newUUID(t),
		"payment_id":     newUUID(t),
		"starts_at":      start.Format(time.RFC3339),
		"ends_at":        start.Add(time.Hour).Format(time.RFC3339),
		"total_minor":    15000,
		"currency":       "EUR",
	}
	for k, v := range extra {
		payload[k] = v
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	return events.Envelope{
		Event:         events.BookingConfirmed,
		Version:       events.SchemaVersion,
		ID:            id.String(),
		OccurredAt:    time.Now().UTC(),
		CorrelationID: correlationID,
		Payload:       payloadBytes,
	}
}

func newUUID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return id.String()
}

func newHandler(t *testing.T, out *bytes.Buffer, rec *fakeRecorder, mail *fakeMailer, addr domain.Recipient) *ConfirmationHandler {
	t.Helper()
	log := logging.New(logging.Options{Service: "notification", Out: out})
	return NewConfirmationHandler(rec, fakeResolver{addr: addr}, mail, log)
}

// ---------------------------------------------------------------------------
// AC3 — unknown payload fields are ignored (forward-compat)
// ---------------------------------------------------------------------------

func TestUnknownPayloadFieldIsIgnoredAndEmailStillSent_Issue19_AC3(t *testing.T) {
	rec := &fakeRecorder{inserted: true}
	mail := &fakeMailer{}
	h := newHandler(t, &bytes.Buffer{}, rec, mail, "customer-7@example.test")

	env := confirmedEnvelope(t, "cust-7", "corr-1", map[string]any{"loyalty_tier": "gold"})

	if err := h.Handle(context.Background(), nil, env); err != nil {
		t.Fatalf("an unknown payload field was rejected: %v", err)
	}
	if mail.count() != 1 {
		t.Errorf("mailer sent %d messages, want 1", mail.count())
	}
}

// ---------------------------------------------------------------------------
// AC4 — no PII in logs; correlation_id present on the path
// ---------------------------------------------------------------------------

func TestSendLogsRedactTheRecipientAndCarryCorrelationID_Issue19_AC4(t *testing.T) {
	const (
		rawAddr = domain.Recipient("customer-secret-123@example.test")
		corr    = "corr-abc-123"
	)
	var out bytes.Buffer
	rec := &fakeRecorder{inserted: true}
	mail := &fakeMailer{}
	h := newHandler(t, &out, rec, mail, rawAddr)

	// The consumer loop puts the correlation id on the context before calling the handler; mirror
	// that here so the shared logger enriches the line.
	ctx := correlation.WithID(context.Background(), corr)
	if err := h.Handle(ctx, nil, confirmedEnvelope(t, "cust-secret", corr, nil)); err != nil {
		t.Fatalf("handle: %v", err)
	}

	logs := out.String()
	if logs == "" {
		t.Fatal("handler emitted no logs on the success path")
	}
	// The raw address must never appear in ANY log line.
	if strings.Contains(logs, rawAddr.Address()) {
		t.Errorf("raw recipient address leaked into logs:\n%s", logs)
	}
	// The redacted form should be there so an operator can still correlate.
	if !strings.Contains(logs, rawAddr.Redact()) {
		t.Errorf("redacted recipient %q not found in logs:\n%s", rawAddr.Redact(), logs)
	}
	// correlation_id must be on the line (AC4).
	if !strings.Contains(logs, corr) {
		t.Errorf("correlation_id %q not propagated to logs:\n%s", corr, logs)
	}
	// Defensive: the audit record must also hold only the redacted form, never the raw address.
	for _, r := range rec.records {
		if r.RecipientRedacted != rawAddr.Redact() {
			t.Errorf("recorded recipient = %q, want the redacted form %q", r.RecipientRedacted, rawAddr.Redact())
		}
	}
}

// ---------------------------------------------------------------------------
// dedupe belt-and-braces: a record that already exists must not send a second email
// ---------------------------------------------------------------------------

func TestAlreadyRecordedConfirmationDoesNotResend_Issue19_AC2(t *testing.T) {
	rec := &fakeRecorder{inserted: false} // RecordSent reports the row already existed
	mail := &fakeMailer{}
	h := newHandler(t, &bytes.Buffer{}, rec, mail, "customer-7@example.test")

	if err := h.Handle(context.Background(), nil, confirmedEnvelope(t, "cust-7", "corr-1", nil)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if mail.count() != 0 {
		t.Errorf("mailer sent %d messages for an already-recorded confirmation, want 0", mail.count())
	}
}

// A send failure must surface as an error so inbox.Process rolls back and the delivery is retried
// (the dedupe row is discarded with it), rather than being swallowed and the email lost.
func TestSendFailureIsReturnedSoTheDeliveryRetries_Issue19_AC1(t *testing.T) {
	rec := &fakeRecorder{inserted: true}
	mail := &fakeMailer{err: errSMTP}
	h := newHandler(t, &bytes.Buffer{}, rec, mail, "customer-7@example.test")

	if err := h.Handle(context.Background(), nil, confirmedEnvelope(t, "cust-7", "corr-1", nil)); err == nil {
		t.Fatal("a failed send returned nil; the delivery would be marked done and the email lost")
	}
}

func TestWrongEventIsRejected(t *testing.T) {
	rec := &fakeRecorder{inserted: true}
	h := newHandler(t, &bytes.Buffer{}, rec, &fakeMailer{}, "customer-7@example.test")

	env := confirmedEnvelope(t, "cust-7", "corr-1", nil)
	env.Event = events.PaymentSucceeded
	if err := h.Handle(context.Background(), nil, env); err == nil {
		t.Fatal("handler accepted an event it does not consume")
	}
}
