//go:build integration

package pgtest_test

// These tests do two jobs.
//
// First, they prove the harness works: that pgtest actually starts Postgres, installs
// btree_gist, and applies migrations. Every service agent's integration tests depend on that, so
// a broken harness would block all of M2 with a confusing error.
//
// Second, and more importantly, they EXECUTE the claims in ADR-0003 and ADR-0011. Until this
// ran, "a Postgres exclusion constraint prevents double-booking" and "the constraint must be
// partial or released rows poison their window" were both prose. The second claim in particular
// is a correction to ADR-0003 that I made from reasoning alone — reasoning is not evidence, and
// an architecture note that turns out to be wrong is worse than no note.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/morisempai/wakewake/shared/platform/pgerr"
	"github.com/morisempai/wakewake/shared/testkit/pgtest"
)

const migrations = "testdata/migrations"

func window(t *testing.T, startHour, endHour int) (time.Time, time.Time) {
	t.Helper()
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	return base.Add(time.Duration(startHour) * time.Hour), base.Add(time.Duration(endHour) * time.Hour)
}

func insert(ctx context.Context, pool *pgxpool.Pool, resource uuid.UUID, from, to time.Time, status string) error {
	_, err := pool.Exec(ctx,
		`INSERT INTO fixture_reservation (id, resource_id, during, status)
		 VALUES ($1, $2, tstzrange($3, $4, '[)'), $5)`,
		uuid.New(), resource, from, to, status)
	return err
}

// TestHarnessStartsPostgresWithBtreeGist is the smoke test for the harness itself.
func TestHarnessStartsPostgresWithBtreeGist(t *testing.T) {
	pool := pgtest.Postgres(t, migrations)
	ctx := context.Background()

	var installed bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'btree_gist')`).Scan(&installed); err != nil {
		t.Fatalf("querying pg_extension: %v", err)
	}
	if !installed {
		t.Fatal("btree_gist is not installed — the exclusion constraint cannot exist without it")
	}
}

// TestOverlappingReservationIsRejected is ADR-0003's invariant, executed.
func TestOverlappingReservationIsRejected(t *testing.T) {
	pool := pgtest.Postgres(t, migrations)
	ctx := context.Background()
	resource := uuid.New()

	from, to := window(t, 10, 12)
	if err := insert(ctx, pool, resource, from, to, "held"); err != nil {
		t.Fatalf("first reservation should succeed: %v", err)
	}

	overlapFrom, overlapTo := window(t, 11, 13)
	err := insert(ctx, pool, resource, overlapFrom, overlapTo, "held")
	if err == nil {
		t.Fatal("an overlapping reservation was accepted — the invariant this architecture rests on does not hold")
	}
	if !pgerr.Is(err, pgerr.ExclusionViolation) {
		t.Fatalf("expected SQLSTATE %s (exclusion violation), got %q: %v",
			pgerr.ExclusionViolation, pgerr.Code(err), err)
	}
	// The contract maps this specific constraint to 409 reservation_overlap, so the name matters.
	if name := pgerr.ConstraintName(err); name != "fixture_no_overlap" {
		t.Errorf("constraint name = %q, want fixture_no_overlap", name)
	}
}

// TestBackToBackReservationsAreAllowed pins the half-open range choice. A closed range would
// reject 10:00-11:00 followed by 11:00-12:00 and silently lose revenue on every adjacent slot.
func TestBackToBackReservationsAreAllowed(t *testing.T) {
	pool := pgtest.Postgres(t, migrations)
	ctx := context.Background()
	resource := uuid.New()

	from1, to1 := window(t, 10, 11)
	if err := insert(ctx, pool, resource, from1, to1, "held"); err != nil {
		t.Fatalf("first reservation: %v", err)
	}

	from2, to2 := window(t, 11, 12)
	if err := insert(ctx, pool, resource, from2, to2, "held"); err != nil {
		t.Fatalf("back-to-back reservation was rejected — half-open ranges are not working: %v", err)
	}
}

// TestReleasedReservationDoesNotBlockItsWindow is ADR-0011, executed.
//
// This is the one that matters most, because it is the claim I made against ADR-0003 from
// reasoning alone. If the partial WHERE clause did not behave as argued, ADR-0011 would be
// confidently wrong and the TTL sweeper would leak inventory in production.
func TestReleasedReservationDoesNotBlockItsWindow(t *testing.T) {
	pool := pgtest.Postgres(t, migrations)
	ctx := context.Background()
	resource := uuid.New()
	from, to := window(t, 14, 16)

	if err := insert(ctx, pool, resource, from, to, "held"); err != nil {
		t.Fatalf("initial hold: %v", err)
	}

	// The TTL sweeper releasing an abandoned hold.
	if _, err := pool.Exec(ctx,
		`UPDATE fixture_reservation SET status = 'released' WHERE resource_id = $1`, resource); err != nil {
		t.Fatalf("releasing: %v", err)
	}

	if err := insert(ctx, pool, resource, from, to, "held"); err != nil {
		t.Fatalf("the window was not rebookable after release — ADR-0011's partial constraint is "+
			"not working, and every expired hold would poison its slot permanently: %v", err)
	}
}

// TestConfirmedReservationStillBlocks guards the other direction: the partial clause must exclude
// only released rows. If it accidentally excluded confirmed ones, paid bookings could be
// double-sold — a far worse failure than the one ADR-0011 fixes.
func TestConfirmedReservationStillBlocks(t *testing.T) {
	pool := pgtest.Postgres(t, migrations)
	ctx := context.Background()
	resource := uuid.New()
	from, to := window(t, 9, 10)

	if err := insert(ctx, pool, resource, from, to, "confirmed"); err != nil {
		t.Fatalf("confirmed reservation: %v", err)
	}

	err := insert(ctx, pool, resource, from, to, "held")
	if err == nil {
		t.Fatal("a confirmed reservation did not block its window — paid bookings could be double-sold")
	}
	if !pgerr.Is(err, pgerr.ExclusionViolation) {
		t.Fatalf("expected exclusion violation, got %q: %v", pgerr.Code(err), err)
	}
}

// TestEmptyRangeIsRejected closes the hole ADR-0011 describes: an equal-bounds '[)' range
// normalises to `empty`, and an empty range overlaps nothing — so without the CHECK it would
// slip past the exclusion constraint entirely.
func TestEmptyRangeIsRejected(t *testing.T) {
	pool := pgtest.Postgres(t, migrations)
	ctx := context.Background()
	resource := uuid.New()
	at, _ := window(t, 10, 10)

	err := insert(ctx, pool, resource, at, at, "held")
	if err == nil {
		t.Fatal("an empty time window was accepted — it overlaps nothing and bypasses the invariant")
	}
	if !pgerr.Is(err, pgerr.CheckViolation) {
		t.Fatalf("expected SQLSTATE %s (check violation), got %q: %v",
			pgerr.CheckViolation, pgerr.Code(err), err)
	}
}

// TestConcurrentReservationsExactlyOneWins is BOOK-2's acceptance criterion and the
// load-bearing test for the whole architecture: N callers race for one slot, exactly one wins.
//
// A fake repository cannot demonstrate this. An in-memory map guarded by a mutex would pass
// while proving only that the mutex works — the guarantee under test belongs to Postgres.
func TestConcurrentReservationsExactlyOneWins(t *testing.T) {
	pool := pgtest.Postgres(t, migrations)
	ctx := context.Background()
	resource := uuid.New()
	from, to := window(t, 18, 20)

	const racers = 8
	start := make(chan struct{})
	results := make(chan error, racers)

	for i := 0; i < racers; i++ {
		go func() {
			<-start // line them all up so they collide rather than queue
			results <- insert(ctx, pool, resource, from, to, "held")
		}()
	}
	close(start)

	var won int
	var lost int
	for i := 0; i < racers; i++ {
		err := <-results
		switch {
		case err == nil:
			won++
		case pgerr.Is(err, pgerr.ExclusionViolation):
			lost++
		default:
			t.Errorf("unexpected error from a racer: %v", err)
		}
	}

	if won != 1 {
		t.Fatalf("%d of %d concurrent reservations succeeded, want exactly 1 — "+
			"this is the double-booking invariant failing", won, racers)
	}
	if lost != racers-1 {
		t.Fatalf("%d racers were cleanly rejected, want %d", lost, racers-1)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM fixture_reservation WHERE resource_id = $1`, resource).Scan(&count); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("%d rows persisted for the contested slot, want 1", count)
	}
}
