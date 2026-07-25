//go:build integration

// Consumer integration tests against a real Postgres (AC5).
//
// The BookingHeld consumer is driven through the real shared/platform/inbox, so "idempotent, keyed
// on the envelope id" is demonstrated rather than claimed: a duplicate delivery must not write a
// second context row, and an unknown payload field must not break the handler.
package events

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/morisempai/wakewake/shared/contracts/events"
	"github.com/morisempai/wakewake/shared/platform/inbox"
	"github.com/morisempai/wakewake/shared/testkit/eventtest"
	"github.com/morisempai/wakewake/shared/testkit/pgtest"

	"github.com/morisempai/wakewake/services/payment/internal/infra"
)

const consumerName = "payment"

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func newID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return id.String()
}

func newFixture(t *testing.T) (*infra.Store, *pgxpool.Pool, *BookingHeldHandler) {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolving migrations: %v", err)
	}
	pool := pgtest.Postgres(t, dir)
	store := infra.NewStore(pool, nil)
	return store, pool, NewBookingHeldHandler(store, discardLogger())
}

func bookingHeld(t *testing.T, bookingID string, amount int64) []byte {
	t.Helper()
	return eventtest.Envelope(t, events.BookingHeld, events.BookingHeldPayload{
		BookingID:     bookingID,
		CustomerID:    newID(t),
		ProductID:     newID(t),
		ResourceID:    newID(t),
		ReservationID: newID(t),
		StartsAt:      time.Now().UTC().Add(24 * time.Hour),
		EndsAt:        time.Now().UTC().Add(25 * time.Hour),
		HoldExpiresAt: time.Now().UTC().Add(15 * time.Minute),
		TotalMinor:    amount,
		Currency:      "EUR",
	}, "corr-"+newID(t))
}

func process(t *testing.T, pool *pgxpool.Pool, raw []byte, h inbox.Handler) bool {
	t.Helper()
	env, err := events.Parse(raw)
	if err != nil {
		t.Fatalf("parsing envelope: %v", err)
	}
	processed, err := inbox.Process(context.Background(), pool, consumerName, env, h)
	if err != nil {
		t.Fatalf("inbox.Process: %v", err)
	}
	return processed
}

func contextAmount(t *testing.T, pool *pgxpool.Pool, bookingID string) (int64, bool) {
	t.Helper()
	var amount int64
	err := pool.QueryRow(context.Background(),
		`SELECT amount_minor FROM booking_context WHERE booking_id = $1`, bookingID).Scan(&amount)
	if err != nil {
		return 0, false
	}
	return amount, true
}

func countRows(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}

// A BookingHeld prepares the payment context, and a redelivery of the same envelope is suppressed
// by the dedupe row — one context row, not two.
func TestBookingHeldPreparesContextAndIsIdempotent_Issue18_AC5(t *testing.T) {
	t.Parallel()

	_, pool, h := newFixture(t)
	bookingID := newID(t)
	raw := bookingHeld(t, bookingID, 12000)

	if !process(t, pool, raw, h.Handle) {
		t.Fatal("first delivery reported as a duplicate")
	}
	amount, ok := contextAmount(t, pool, bookingID)
	if !ok || amount != 12000 {
		t.Fatalf("booking context amount = %d (found=%v), want 12000", amount, ok)
	}

	if process(t, pool, raw, h.Handle) {
		t.Error("the redelivery ran the handler again; it must be suppressed by the envelope id")
	}
	if n := countRows(t, pool, "booking_context"); n != 1 {
		t.Errorf("booking_context holds %d rows after a redelivery, want 1", n)
	}
	if n := countRows(t, pool, "processed_event"); n != 1 {
		t.Errorf("processed_event holds %d rows, want 1", n)
	}
}

// An unknown payload field must not break the handler — that is what lets booking add an optional
// field without a coordinated deploy.
func TestBookingHeldIgnoresUnknownFields_Issue18_AC5(t *testing.T) {
	t.Parallel()

	_, pool, h := newFixture(t)
	bookingID := newID(t)
	raw := eventtest.WithUnknownField(t, bookingHeld(t, bookingID, 9900), "loyalty_tier", "gold")

	if !process(t, pool, raw, h.Handle) {
		t.Fatal("an event with an unknown field was not processed")
	}
	if amount, ok := contextAmount(t, pool, bookingID); !ok || amount != 9900 {
		t.Errorf("booking context amount = %d (found=%v), want 9900 — the event should have been handled normally", amount, ok)
	}
}
