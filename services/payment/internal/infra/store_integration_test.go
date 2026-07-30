//go:build integration

// Integration tests for the payment store, against a real Postgres.
//
// These cannot be unit tests: AC2 is a claim about the LIVE table's columns, AC3 is idempotency
// enforced by Postgres primary/unique keys, and AC4 is the claim that a status change and its
// outbox row commit together — none of which a fake backed by a map can demonstrate.
package infra

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/morisempai/wakewake/shared/contracts/events"
	"github.com/morisempai/wakewake/shared/platform/correlation"
	"github.com/morisempai/wakewake/shared/testkit/eventtest"
	"github.com/morisempai/wakewake/shared/testkit/pgtest"

	"github.com/morisempai/wakewake/services/payment/internal/domain"
)

func migrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolving migrations dir: %v", err)
	}
	return dir
}

func newStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	pool := pgtest.Postgres(t, migrationsDir(t))
	return NewStore(pool, nil), pool
}

func newID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return id.String()
}

func claim(t *testing.T) domain.Claim {
	t.Helper()
	return domain.Claim{Key: "idem-" + newID(t), Fingerprint: "fp-" + newID(t)}
}

func pendingPayment(t *testing.T, bookingID string) domain.Payment {
	t.Helper()
	return domain.NewPayment(newID(t), bookingID, 12000, "EUR", "pi_"+newID(t), time.Now().UTC())
}

func countRows(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}

// ---------------------------------------------------------------------------
// AC2 — no raw card data at rest
// ---------------------------------------------------------------------------

// The live payment table's columns are limited to ids, tokens, status, and amounts. A column that
// could hold a PAN, CVV, expiry, or cardholder name is a PCI incident; this asserts the schema
// stays that way by inspecting the real table, so a future migration adding such a column fails.
func TestPaymentTableHoldsNoCardData_Issue18_AC2(t *testing.T) {
	t.Parallel()

	_, pool := newStore(t)

	rows, err := pool.Query(context.Background(),
		`SELECT column_name FROM information_schema.columns
		  WHERE table_schema = 'public' AND table_name = 'payment'`)
	if err != nil {
		t.Fatalf("reading payment columns: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scanning column name: %v", err)
		}
		got = append(got, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading columns: %v", err)
	}
	sort.Strings(got)

	want := []string{
		"amount_minor", "booking_id", "correlation_id", "created_at", "currency", "failure_code",
		"id", "provider", "provider_payment_id", "status", "updated_at",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("payment columns = %v\nwant exactly %v — an unexpected column is a PCI risk", got, want)
	}

	// Belt and braces: no column name may resemble card data, whatever the exact set becomes.
	for _, name := range got {
		for _, forbidden := range []string{"card", "pan", "cvv", "cvc", "expir", "number", "holder"} {
			if strings.Contains(strings.ToLower(name), forbidden) {
				t.Errorf("payment has a column %q that looks like card data — never store card data", name)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// AC3 — createPayment idempotency (store half)
// ---------------------------------------------------------------------------

func TestReplayingAnIdempotencyKeyReturnsTheOriginalAndWritesNothing_Issue18_AC3(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	ctx := context.Background()
	c := claim(t)
	p := pendingPayment(t, newID(t))

	first, err := store.InsertPayment(ctx, c, p)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	second, err := store.InsertPayment(ctx, c, p)
	if err != nil {
		t.Fatalf("replay returned %v, want the original payment", err)
	}
	if second.ID != first.ID {
		t.Errorf("replay returned id %s, want the original %s", second.ID, first.ID)
	}
	if n := countRows(t, pool, "payment"); n != 1 {
		t.Errorf("payment table holds %d rows after a replay, want 1", n)
	}
}

func TestReusingAnIdempotencyKeyWithADifferentBodyIsRejected_Issue18_AC3(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	ctx := context.Background()
	c := claim(t)

	if _, err := store.InsertPayment(ctx, c, pendingPayment(t, newID(t))); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	reused := domain.Claim{Key: c.Key, Fingerprint: "a-completely-different-body"}
	_, err := store.InsertPayment(ctx, reused, pendingPayment(t, newID(t)))
	if err == nil || err != domain.ErrIdempotencyKeyReuse {
		t.Fatalf("error = %v, want ErrIdempotencyKeyReuse", err)
	}
	if n := countRows(t, pool, "payment"); n != 1 {
		t.Errorf("payment table holds %d rows, want 1 — a rejected reuse writes nothing", n)
	}
}

// A second payment for the same booking, under a different key, is rejected as
// payment_already_exists by the unique constraint.
func TestASecondPaymentForOneBookingIsRejected_Issue18_AC3(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	ctx := context.Background()
	bookingID := newID(t)

	if _, err := store.InsertPayment(ctx, claim(t), pendingPayment(t, bookingID)); err != nil {
		t.Fatalf("first payment: %v", err)
	}
	_, err := store.InsertPayment(ctx, claim(t), pendingPayment(t, bookingID))
	if err != domain.ErrPaymentAlreadyExists {
		t.Fatalf("error = %v, want ErrPaymentAlreadyExists", err)
	}
}

func TestConcurrentSameKeyInsertsProduceOnePayment_Issue18_AC3(t *testing.T) {
	t.Parallel()

	const concurrency = 4
	store, pool := newStore(t)
	ctx := context.Background()
	c := claim(t)
	shared := pendingPayment(t, newID(t))

	var (
		release = make(chan struct{})
		ready   sync.WaitGroup
		done    sync.WaitGroup
		mu      sync.Mutex
		ids     = map[string]int{}
		errs    []error
	)
	ready.Add(concurrency)
	done.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func() {
			defer done.Done()
			ready.Done()
			<-release
			got, err := store.InsertPayment(ctx, c, shared)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			ids[got.ID]++
		}()
	}

	ready.Wait()
	close(release)
	done.Wait()

	for _, err := range errs {
		t.Errorf("same-key request failed with %v; every one should have returned the winner's payment", err)
	}
	if len(ids) != 1 {
		t.Errorf("same-key requests produced %d distinct payments, want 1: %v", len(ids), ids)
	}
	if n := countRows(t, pool, "payment"); n != 1 {
		t.Errorf("payment table holds %d rows, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// AC4 — the webhook outcome writes the status change and the outbox row in one transaction
// ---------------------------------------------------------------------------

func succeed() domain.Transition {
	return func(p domain.Payment) (domain.Payment, []domain.Emission, error) {
		return p.MarkSucceeded(time.Now().UTC())
	}
}

func fail(code string) domain.Transition {
	return func(p domain.Payment) (domain.Payment, []domain.Emission, error) {
		return p.MarkFailed(code, time.Now().UTC())
	}
}

func TestRecordOutcomeSuccessStagesPaymentSucceededInOneTransaction_Issue18_AC4(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	ctx := context.Background()
	p, err := store.InsertPayment(ctx, claim(t), pendingPayment(t, newID(t)))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	res, err := store.RecordOutcome(ctx, "evt_"+newID(t), "payment_intent.succeeded", p.ProviderPaymentID, succeed())
	if err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	if !res.Found || !res.Changed || res.Payment.Status != domain.StatusSucceeded {
		t.Fatalf("result = %+v, want found+changed+succeeded", res)
	}

	// Exactly one PaymentSucceeded, staged and unpublished, validating against the AsyncAPI spec.
	var payload []byte
	var publishedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT payload, published_at FROM outbox WHERE event = $1`, events.PaymentSucceeded).
		Scan(&payload, &publishedAt); err != nil {
		t.Fatalf("reading outbox: %v", err)
	}
	if publishedAt != nil {
		t.Error("published_at is already set; the relay must set it only after a broker confirm")
	}
	eventtest.AssertValidAgainstSpec(t, events.PaymentSucceeded, payload)

	var decoded events.PaymentSucceededPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if decoded.PaymentID != p.ID || decoded.BookingID != p.BookingID ||
		decoded.AmountMinor != 12000 || decoded.Currency != "EUR" || decoded.ProviderPaymentID != p.ProviderPaymentID {
		t.Errorf("PaymentSucceeded payload wrong: %+v", decoded)
	}
}

func TestRecordOutcomeFailureStagesPaymentFailed_Issue18_AC4(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	ctx := context.Background()
	p, err := store.InsertPayment(ctx, claim(t), pendingPayment(t, newID(t)))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	if _, err := store.RecordOutcome(ctx, "evt_"+newID(t), "payment_intent.payment_failed", p.ProviderPaymentID, fail("card_declined")); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}

	reloaded, err := store.ByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Status != domain.StatusFailed || reloaded.FailureCode == nil || *reloaded.FailureCode != "card_declined" {
		t.Errorf("payment = %+v, want failed with card_declined", reloaded)
	}

	payload := outboxPayload(t, pool, events.PaymentFailed)
	eventtest.AssertValidAgainstSpec(t, events.PaymentFailed, payload)
	var decoded events.PaymentFailedPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if decoded.FailureCode != "card_declined" {
		t.Errorf("failure_code = %q, want card_declined", decoded.FailureCode)
	}
}

// A duplicate Stripe event (same event id) records nothing new and emits nothing — exactly-once
// processing, not merely at-least-once.
func TestDuplicateWebhookEventIsSuppressed_Issue18_AC4(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	ctx := context.Background()
	p, err := store.InsertPayment(ctx, claim(t), pendingPayment(t, newID(t)))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	eventID := "evt_" + newID(t)

	if _, err := store.RecordOutcome(ctx, eventID, "payment_intent.succeeded", p.ProviderPaymentID, succeed()); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	res, err := store.RecordOutcome(ctx, eventID, "payment_intent.succeeded", p.ProviderPaymentID, succeed())
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if !res.Duplicate {
		t.Error("the redelivery was not reported as a duplicate")
	}
	if res.Changed {
		t.Error("the redelivery changed state; it must be suppressed by the stripe event id")
	}
	if n := countRows(t, pool, "outbox"); n != 1 {
		t.Errorf("outbox holds %d rows after a duplicate webhook, want 1", n)
	}
}

// A webhook for a PaymentIntent this service does not know is reported as not found, and records
// nothing — the handler acks it so Stripe stops retrying.
func TestRecordOutcomeForUnknownIntentFindsNothing_Issue18_AC4(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	res, err := store.RecordOutcome(context.Background(), "evt_"+newID(t), "payment_intent.succeeded", "pi_unknown", succeed())
	if err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	if res.Found {
		t.Error("an unknown intent was reported as found")
	}
	if n := countRows(t, pool, "outbox"); n != 0 {
		t.Errorf("outbox holds %d rows, want 0 for an unknown intent", n)
	}
}

// ---------------------------------------------------------------------------
// Issue #23 — the correlation id survives the Stripe webhook
// ---------------------------------------------------------------------------

// The load-bearing test for issue #23. A payment created under correlation id X, whose Stripe webhook
// then arrives bearing a DIFFERENT correlation id (as correlation.Middleware would have minted for an
// external callback carrying no X-Correlation-Id), must stage a PaymentSucceeded that carries the
// ORIGINAL id X — not the webhook's fresh one. That is what keeps the customer journey a single trace
// instead of two islands joined only by booking_id.
func TestWebhookOutcomeCarriesTheOriginalCreatePaymentCorrelationId_Issue23(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)

	const originalCorrelationID = "corr-from-create-payment-0123456789"

	// A payment created carrying the createPayment request's correlation id (the service layer copies
	// it off the context onto the aggregate; here we set it directly, as that is the store's input).
	p := pendingPayment(t, newID(t))
	p.CorrelationID = originalCorrelationID

	stored, err := store.InsertPayment(context.Background(), claim(t), p)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if stored.CorrelationID != originalCorrelationID {
		t.Fatalf("stored correlation_id = %q, want it persisted as %q", stored.CorrelationID, originalCorrelationID)
	}

	// The webhook is an external callback: its context carries a freshly minted id, unrelated to the
	// original request. The fix must prefer the stored id over this one.
	webhookCtx := correlation.WithID(context.Background(), "corr-webhook-minted-fresh-9876543210")

	res, err := store.RecordOutcome(webhookCtx, "evt_"+newID(t), "payment_intent.succeeded", p.ProviderPaymentID, succeed())
	if err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}
	if !res.Found || !res.Changed {
		t.Fatalf("result = %+v, want found+changed", res)
	}

	var gotCorrelationID string
	if err := pool.QueryRow(context.Background(),
		`SELECT correlation_id FROM outbox WHERE event = $1`, events.PaymentSucceeded).
		Scan(&gotCorrelationID); err != nil {
		t.Fatalf("reading staged outbox correlation_id: %v", err)
	}
	if gotCorrelationID != originalCorrelationID {
		t.Fatalf("PaymentSucceeded carried correlation_id %q, want the original %q from createPayment "+
			"(the webhook's minted id must not win)", gotCorrelationID, originalCorrelationID)
	}
}

// A legacy payment — one created before 0007, so its correlation_id is NULL — must not be regressed:
// the webhook falls back to the outbox's mint-on-empty rather than stamping an empty id. The staged
// event still carries a non-empty correlation id (just not a threaded one).
func TestWebhookOutcomeForLegacyPaymentFallsBackToAMintedId_Issue23(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)

	// No CorrelationID set: correlationArg sends NULL, mirroring a row inserted before the feature.
	p := pendingPayment(t, newID(t))
	if _, err := store.InsertPayment(context.Background(), claim(t), p); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if _, err := store.RecordOutcome(context.Background(), "evt_"+newID(t),
		"payment_intent.succeeded", p.ProviderPaymentID, succeed()); err != nil {
		t.Fatalf("RecordOutcome: %v", err)
	}

	var gotCorrelationID string
	if err := pool.QueryRow(context.Background(),
		`SELECT correlation_id FROM outbox WHERE event = $1`, events.PaymentSucceeded).
		Scan(&gotCorrelationID); err != nil {
		t.Fatalf("reading staged outbox correlation_id: %v", err)
	}
	if gotCorrelationID == "" {
		t.Fatal("staged PaymentSucceeded has an empty correlation_id; the legacy fallback must mint one")
	}
}

func outboxPayload(t *testing.T, pool *pgxpool.Pool, event string) []byte {
	t.Helper()
	var payload []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT payload FROM outbox WHERE event = $1`, event).Scan(&payload); err != nil {
		t.Fatalf("reading %s from outbox: %v", event, err)
	}
	return payload
}
