//go:build integration

// Integration tests for the sent_notification store, against a real Postgres (AC5).
//
// These cannot be unit tests: AC5 is the claim that the migration produced a live table matching
// what the code writes, and that event_id is a real uniqueness constraint enforcing idempotency —
// a fake backed by a map would only prove the fake works.
package infra

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/morisempai/wakewake/shared/platform/pgxx"
	"github.com/morisempai/wakewake/shared/testkit/pgtest"

	"github.com/morisempai/wakewake/services/notification/internal/domain"
)

func migrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolving migrations dir: %v", err)
	}
	return dir
}

func newID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return id.String()
}

func sampleRecord(t *testing.T) domain.SentRecord {
	t.Helper()
	return domain.SentRecord{
		EventID:           newID(t),
		BookingID:         newID(t),
		CustomerID:        newID(t),
		Channel:           domain.ChannelEmail,
		RecipientRedacted: "c***@example.test",
		Subject:           "Your booking is confirmed",
		CorrelationID:     "corr-" + newID(t),
	}
}

func countRows(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}

// The migration must produce a live sent_notification table with exactly the columns the store
// writes: if a column is missing or misnamed, this INSERT-and-read-back fails. This is the "live
// table matches" half of AC5.
func TestSentNotificationLiveTableMatchesWhatTheStoreWrites_Issue19_AC5(t *testing.T) {
	t.Parallel()

	pool := pgtest.Postgres(t, migrationsDir(t))
	store := NewStore()
	ctx := context.Background()
	rec := sampleRecord(t)

	err := pgxx.WithinTx(ctx, pool, func(tx pgx.Tx) error {
		inserted, err := store.RecordSent(ctx, tx, rec)
		if err != nil {
			return err
		}
		if !inserted {
			t.Error("first RecordSent reported the row already existed")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("recording: %v", err)
	}

	if n := countRows(t, pool, "sent_notification"); n != 1 {
		t.Fatalf("sent_notification holds %d rows, want 1", n)
	}

	// Read every column the code depends on back out by name. A drifted migration fails here.
	var (
		bookingID, customerID, channel, redacted, subject, correlationID string
	)
	if err := pool.QueryRow(ctx,
		`SELECT booking_id, customer_id, channel, recipient_redacted, subject, correlation_id
		   FROM sent_notification WHERE event_id = $1`, rec.EventID).
		Scan(&bookingID, &customerID, &channel, &redacted, &subject, &correlationID); err != nil {
		t.Fatalf("reading the row back by its columns: %v", err)
	}
	if bookingID != rec.BookingID || customerID != rec.CustomerID || channel != rec.Channel ||
		redacted != rec.RecipientRedacted || subject != rec.Subject || correlationID != rec.CorrelationID {
		t.Errorf("row read back does not match what was written: %+v", rec)
	}

	// The redacted-only rule: the stored recipient must not look like a full address (no '@' with a
	// local part that survives). This guards against a future edit persisting the raw address.
	if redacted != "c***@example.test" {
		t.Errorf("recipient_redacted = %q, expected the redacted form only", redacted)
	}
}

// event_id is the primary key, so a second record for the same envelope id is rejected — the
// idempotency the whole table exists to provide (AC5 / AC2 at the storage layer).
func TestRecordingTheSameEventIDTwiceIsIdempotent_Issue19_AC5(t *testing.T) {
	t.Parallel()

	pool := pgtest.Postgres(t, migrationsDir(t))
	store := NewStore()
	ctx := context.Background()
	rec := sampleRecord(t)

	record := func() bool {
		t.Helper()
		var inserted bool
		if err := pgxx.WithinTx(ctx, pool, func(tx pgx.Tx) error {
			var e error
			inserted, e = store.RecordSent(ctx, tx, rec)
			return e
		}); err != nil {
			t.Fatalf("recording: %v", err)
		}
		return inserted
	}

	if !record() {
		t.Fatal("first record reported the row already existed")
	}
	if record() {
		t.Error("second record with the same event_id reported a fresh insert; the PK must dedupe it")
	}
	if n := countRows(t, pool, "sent_notification"); n != 1 {
		t.Errorf("sent_notification holds %d rows after a duplicate record, want 1", n)
	}
}

// The processed_event table (migration 0002, pasted from shared/platform/inbox) must exist, or
// inbox.Process cannot run its dedupe insert. A trivial read proves the migration created it.
func TestProcessedEventTableExists_Issue19_AC5(t *testing.T) {
	t.Parallel()

	pool := pgtest.Postgres(t, migrationsDir(t))
	if n := countRows(t, pool, "processed_event"); n != 0 {
		t.Errorf("a fresh processed_event holds %d rows, want 0", n)
	}
}
