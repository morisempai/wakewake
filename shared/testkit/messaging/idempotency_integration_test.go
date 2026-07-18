//go:build integration

package messaging_test

// The Idempotency-Key contract, executed.
//
// Claim uses `(xmax = 0)` to distinguish a fresh INSERT from an ON CONFLICT hit. That is a real
// Postgres idiom, but it is obscure enough that it deserves a test rather than trust: if it
// reported "inserted" on a conflict, a replayed request would silently create a second booking,
// which is the exact failure the key exists to prevent.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/morisempai/wakewake/shared/platform/idempotency"
	"github.com/morisempai/wakewake/shared/platform/pgxx"
)

func idemDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := db(t)
	if _, err := pool.Exec(context.Background(), idempotency.MigrationSQL); err != nil {
		t.Fatalf("applying the embedded idempotency DDL: %v", err)
	}
	return pool
}

func TestFirstClaimIsNotAReplay(t *testing.T) {
	pool := idemDB(t)
	ctx := context.Background()
	resource := uuid.New().String()

	var got string
	var replayed bool
	if err := pgxx.WithinTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		got, replayed, err = idempotency.Claim(ctx, tx, "key-1", idempotency.Fingerprint([]byte(`{"a":1}`)), resource)
		return err
	}); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if replayed {
		t.Error("first use of a key reported as a replay — a fresh request would return someone else's result")
	}
	if got != resource {
		t.Errorf("resource = %q, want %q", got, resource)
	}
}

func TestSameKeySameBodyReplaysTheOriginal(t *testing.T) {
	pool := idemDB(t)
	ctx := context.Background()
	first := uuid.New().String()
	second := uuid.New().String()
	fp := idempotency.Fingerprint([]byte(`{"resource":"boat-1"}`))

	if err := pgxx.WithinTx(ctx, pool, func(tx pgx.Tx) error {
		_, _, err := idempotency.Claim(ctx, tx, "key-2", fp, first)
		return err
	}); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	var got string
	var replayed bool
	if err := pgxx.WithinTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		got, replayed, err = idempotency.Claim(ctx, tx, "key-2", fp, second)
		return err
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}

	if !replayed {
		t.Error("a repeated key with an identical body was not detected as a replay — this is how " +
			"a retried request becomes a second booking")
	}
	if got != first {
		t.Errorf("replay returned %q, want the ORIGINAL %q", got, first)
	}

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_key`).Scan(&rows); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if rows != 1 {
		t.Errorf("%d key rows, want 1", rows)
	}
}

func TestSameKeyDifferentBodyIsReuse(t *testing.T) {
	pool := idemDB(t)
	ctx := context.Background()

	if err := pgxx.WithinTx(ctx, pool, func(tx pgx.Tx) error {
		_, _, err := idempotency.Claim(ctx, tx, "key-3",
			idempotency.Fingerprint([]byte(`{"party_size":2}`)), uuid.New().String())
		return err
	}); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	err := pgxx.WithinTx(ctx, pool, func(tx pgx.Tx) error {
		_, _, err := idempotency.Claim(ctx, tx, "key-3",
			idempotency.Fingerprint([]byte(`{"party_size":8}`)), uuid.New().String())
		return err
	})
	if !errors.Is(err, idempotency.ErrKeyReuse) {
		t.Fatalf("expected ErrKeyReuse for a changed body, got %v — a client reusing a key for a "+
			"different request would silently get the wrong resource back", err)
	}
}

// TestConcurrentSameKeyRequestsSerialise is why the claim happens before the domain write.
//
// Both requests carry one key. They must not both proceed: one wins, the other either blocks and
// replays or is rejected. Exactly one key row must exist afterwards.
func TestConcurrentSameKeyRequestsSerialise(t *testing.T) {
	pool := idemDB(t)
	ctx := context.Background()
	fp := idempotency.Fingerprint([]byte(`{"same":"body"}`))

	const racers = 6
	var wg sync.WaitGroup
	results := make(chan struct {
		resource string
		replayed bool
		err      error
	}, racers)

	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var res string
			var rep bool
			err := pgxx.WithinTx(ctx, pool, func(tx pgx.Tx) error {
				var err error
				res, rep, err = idempotency.Claim(ctx, tx, "key-race", fp, uuid.New().String())
				return err
			})
			results <- struct {
				resource string
				replayed bool
				err      error
			}{res, rep, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var fresh int
	var replays int
	winners := map[string]bool{}
	for r := range results {
		if r.err != nil {
			t.Errorf("unexpected error from a racer: %v", r.err)
			continue
		}
		if r.replayed {
			replays++
		} else {
			fresh++
		}
		winners[r.resource] = true
	}

	if fresh != 1 {
		t.Errorf("%d racers claimed the key as fresh, want exactly 1 — concurrent same-key "+
			"requests are not serialising, so a retry storm would create several bookings", fresh)
	}
	if replays != racers-1 {
		t.Errorf("%d replays, want %d", replays, racers-1)
	}
	if len(winners) != 1 {
		t.Errorf("racers received %d different resource ids, want 1 — every caller with the same "+
			"key must be told about the same resource", len(winners))
	}

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_key`).Scan(&rows); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if rows != 1 {
		t.Errorf("%d key rows persisted, want 1", rows)
	}
}
