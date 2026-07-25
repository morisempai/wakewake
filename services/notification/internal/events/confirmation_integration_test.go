//go:build integration

// End-to-end consumer tests against a real Postgres (AC2, AC3) and, when a Mailhog is reachable,
// a real SMTP delivery read back through Mailhog's HTTP API (AC1).
//
// The dedupe and unknown-field guarantees are driven through the REAL shared/platform/inbox
// transaction and the REAL sent_notification store, so "a duplicate delivery sends exactly one
// email" is proven by the plumbing that ships, not a fake of it. The Mailhog-delivery assertion
// needs a real Mailhog container; shared/testkit has no helper for that yet (issue #20), so it
// runs only when SMTP_HOST/SMTP_PORT/MAILHOG_API_URL point at one (e.g. the compose stack) and
// otherwise skips with that issue linked.
package events

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/morisempai/wakewake/shared/contracts/events"
	"github.com/morisempai/wakewake/shared/platform/inbox"
	"github.com/morisempai/wakewake/shared/testkit/eventtest"
	"github.com/morisempai/wakewake/shared/testkit/pgtest"

	"github.com/morisempai/wakewake/services/notification/internal/infra"
)

// consumerName scopes the inbox dedupe rows, matching config.ServiceName.
const consumerName = "notification"

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func migrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolving migrations: %v", err)
	}
	return dir
}

func countRows(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}

// confirmedRaw builds a valid BookingConfirmed envelope carrying customerID, validated against the
// AsyncAPI spec so the test payload can never drift from the contract.
func confirmedRaw(t *testing.T, customerID, correlationID string) []byte {
	t.Helper()
	start := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Hour)
	payload := events.BookingConfirmedPayload{
		BookingID:     newUUID(t),
		CustomerID:    customerID,
		ProductID:     newUUID(t),
		ResourceID:    newUUID(t),
		ReservationID: newUUID(t),
		PaymentID:     newUUID(t),
		StartsAt:      start,
		EndsAt:        start.Add(time.Hour),
		TotalMinor:    15000,
		Currency:      "EUR",
	}
	raw := eventtest.Envelope(t, events.BookingConfirmed, payload, correlationID)
	// The envelope's payload must satisfy the contract; catches a drifted test fixture.
	var env struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("splitting envelope: %v", err)
	}
	eventtest.AssertValidAgainstSpec(t, events.BookingConfirmed, env.Payload)
	return raw
}

func atoiOrFatal(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("SMTP_PORT %q is not an integer: %v", s, err)
	}
	return n
}

// process runs one delivery through the real inbox transaction against pool.
func process(t *testing.T, pool *pgxpool.Pool, h *ConfirmationHandler, raw []byte) bool {
	t.Helper()
	env, err := events.Parse(raw)
	if err != nil {
		t.Fatalf("parsing envelope: %v", err)
	}
	processed, err := inbox.Process(context.Background(), pool, consumerName, env, h.Handle)
	if err != nil {
		t.Fatalf("inbox.Process: %v", err)
	}
	return processed
}

// ---------------------------------------------------------------------------
// AC2 — a duplicate delivery sends exactly one email
// ---------------------------------------------------------------------------

func TestDuplicateDeliverySendsExactlyOneEmail_Issue19_AC2(t *testing.T) {
	t.Parallel()

	pool := pgtest.Postgres(t, migrationsDir(t))
	mail := &fakeMailer{}
	h := NewConfirmationHandler(infra.NewStore(), infra.NewDevRecipientResolver(), mail, discardLogger())

	raw := confirmedRaw(t, newUUID(t), "corr-"+newUUID(t))

	if !process(t, pool, h, raw) {
		t.Fatal("first delivery reported as a duplicate")
	}
	if process(t, pool, h, raw) {
		t.Error("redelivery ran the handler again; the envelope id must suppress it")
	}

	if got := mail.count(); got != 1 {
		t.Errorf("mailer sent %d messages across a redelivery, want exactly 1", got)
	}
	if n := countRows(t, pool, "sent_notification"); n != 1 {
		t.Errorf("sent_notification holds %d rows, want 1", n)
	}
	if n := countRows(t, pool, "processed_event"); n != 1 {
		t.Errorf("processed_event holds %d rows, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// AC3 — unknown payload fields are ignored, end to end
// ---------------------------------------------------------------------------

func TestUnknownPayloadFieldIsIgnoredEndToEnd_Issue19_AC3(t *testing.T) {
	t.Parallel()

	pool := pgtest.Postgres(t, migrationsDir(t))
	mail := &fakeMailer{}
	h := NewConfirmationHandler(infra.NewStore(), infra.NewDevRecipientResolver(), mail, discardLogger())

	raw := eventtest.WithUnknownField(t, confirmedRaw(t, newUUID(t), "corr-"+newUUID(t)), "loyalty_tier", "gold")

	if !process(t, pool, h, raw) {
		t.Fatal("the delivery was treated as a duplicate")
	}
	if got := mail.count(); got != 1 {
		t.Errorf("mailer sent %d messages, want 1 — the unknown field should have been ignored", got)
	}
}

// ---------------------------------------------------------------------------
// AC1 — a BookingConfirmed delivers exactly one email to Mailhog
// ---------------------------------------------------------------------------

func TestBookingConfirmedDeliversOneEmailToMailhog_Issue19_AC1(t *testing.T) {
	smtpHost := os.Getenv("SMTP_HOST")
	smtpPort := os.Getenv("SMTP_PORT")
	apiURL := os.Getenv("MAILHOG_API_URL")
	if smtpHost == "" || smtpPort == "" || apiURL == "" {
		t.Skip("no Mailhog reachable: set SMTP_HOST, SMTP_PORT, MAILHOG_API_URL (e.g. compose stack). A testkit helper is tracked in #20") //nolint:staticcheck // #20
	}

	pool := pgtest.Postgres(t, migrationsDir(t))
	port := atoiOrFatal(t, smtpPort)
	mailer := infra.NewSMTPMailer(smtpHost, port, "no-reply@bookings.example.test")
	h := NewConfirmationHandler(infra.NewStore(), infra.NewDevRecipientResolver(), mailer, discardLogger())

	// A unique customer id makes the resolved recipient unique, so this assertion is not disturbed
	// by other messages sitting in a shared Mailhog.
	customerID := newUUID(t)
	wantTo := "customer-" + customerID + "@example.test"

	if !process(t, pool, h, confirmedRaw(t, customerID, "corr-"+newUUID(t))) {
		t.Fatal("delivery reported as a duplicate")
	}

	msgs := mailhogMessagesTo(t, apiURL, wantTo)
	if len(msgs) != 1 {
		t.Fatalf("Mailhog holds %d messages for %s, want exactly 1", len(msgs), wantTo)
	}
	if subj := msgs[0].subject(); subj != "Your booking is confirmed" {
		t.Errorf("subject = %q, want %q", subj, "Your booking is confirmed")
	}
}

// --- Mailhog HTTP API reader ----------------------------------------------

type mailhogMessage struct {
	Content struct {
		Headers map[string][]string `json:"Headers"`
	} `json:"Content"`
}

func (m mailhogMessage) subject() string {
	if v := m.Content.Headers["Subject"]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func (m mailhogMessage) to() []string { return m.Content.Headers["To"] }

func mailhogMessagesTo(t *testing.T, apiURL, addr string) []mailhogMessage {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, apiURL+"/api/v2/messages?limit=200", nil)
	if err != nil {
		t.Fatalf("building Mailhog request: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("querying Mailhog: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	var body struct {
		Items []mailhogMessage `json:"items"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decoding Mailhog response: %v", err)
	}

	var matched []mailhogMessage
	for _, m := range body.Items {
		for _, to := range m.to() {
			if to == addr {
				matched = append(matched, m)
			}
		}
	}
	return matched
}
