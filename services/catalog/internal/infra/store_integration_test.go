//go:build integration

// Integration tests for the product store, against a real Postgres.
//
// The filter and keyset behaviour is SQL, so a fake store backed by a map would only prove the
// fake works. These run the real migrations and the real queries: every filter is asserted by
// showing an excluded row is actually absent (AC2), the cursor is shown stable across an insert
// made mid-pagination (AC3), and the live table is checked against the DDL the store scans (AC5).
package infra

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/morisempai/wakewake/shared/testkit/pgtest"

	"github.com/morisempai/wakewake/services/catalog/internal/domain"
)

func migrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolving migrations dir: %v", err)
	}
	return dir
}

// newStore starts a Postgres with this service's real migrations applied. The migrations are the
// subject under test as much as the Go code is.
func newStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	pool := pgtest.Postgres(t, migrationsDir(t))
	return NewStore(pool), pool
}

func newID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("generating uuid: %v", err)
	}
	return id.String()
}

// seedable is a product with sensible defaults; tests override only the fields they exercise.
func product(t *testing.T) domain.Product {
	t.Helper()
	return domain.Product{
		ID:             newID(t),
		ResourceID:     newID(t),
		Type:           domain.TypeBoat,
		Name:           "Test Boat",
		Capacity:       4,
		Location:       "lake-geneva",
		BasePriceMinor: 120000,
		Currency:       "EUR",
		MediaURLs:      []string{},
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}
}

// seed inserts a product directly. Catalog has no write endpoint in this slice, so tests are the
// only writer — which is exactly the "seed data for tests" the story asks for (AC5).
func seed(t *testing.T, pool *pgxpool.Pool, p domain.Product) domain.Product {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO product
		   (id, resource_id, type, name, description, capacity, location,
		    base_price_minor, currency, media_urls, created_at, updated_at)
		 VALUES ($1,$2,$3::product_type,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		p.ID, p.ResourceID, string(p.Type), p.Name, p.Description, p.Capacity, p.Location,
		p.BasePriceMinor, p.Currency, p.MediaURLs, p.CreatedAt, p.UpdatedAt)
	if err != nil {
		t.Fatalf("seeding product: %v", err)
	}
	return p
}

func ids(products []domain.Product) map[string]bool {
	out := map[string]bool{}
	for _, p := range products {
		out[p.ID] = true
	}
	return out
}

// ---------------------------------------------------------------------------
// AC5 — the live table matches the DDL the store scans
// ---------------------------------------------------------------------------

// If a migration ever drops or renames a column the store reads, this fails here rather than as a
// mystifying scan error at runtime. It asserts the exact column set, type, and nullability.
func TestLiveTableMatchesTheDDL_Issue13_AC5(t *testing.T) {
	t.Parallel()

	_, pool := newStore(t)

	type col struct {
		dataType string
		nullable bool
	}
	want := map[string]col{
		"id":               {"uuid", false},
		"resource_id":      {"uuid", false},
		"type":             {"USER-DEFINED", false}, // the product_type enum
		"name":             {"text", false},
		"description":      {"text", true},
		"capacity":         {"integer", false},
		"location":         {"text", false},
		"base_price_minor": {"bigint", false},
		"currency":         {"text", false},
		"media_urls":       {"ARRAY", false},
		"created_at":       {"timestamp with time zone", false},
		"updated_at":       {"timestamp with time zone", false},
	}

	rows, err := pool.Query(context.Background(),
		`SELECT column_name, data_type, is_nullable
		   FROM information_schema.columns
		  WHERE table_name = 'product'`)
	if err != nil {
		t.Fatalf("introspecting columns: %v", err)
	}
	defer rows.Close()

	got := map[string]col{}
	for rows.Next() {
		var name, dataType, isNullable string
		if err := rows.Scan(&name, &dataType, &isNullable); err != nil {
			t.Fatalf("scanning column: %v", err)
		}
		got[name] = col{dataType: dataType, nullable: isNullable == "YES"}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading columns: %v", err)
	}

	for name, wantCol := range want {
		gotCol, ok := got[name]
		if !ok {
			t.Errorf("column %q is missing from the live table", name)
			continue
		}
		if gotCol != wantCol {
			t.Errorf("column %q = %+v, want %+v", name, gotCol, wantCol)
		}
	}
	for name := range got {
		if _, expected := want[name]; !expected {
			t.Errorf("unexpected column %q in the live table — the DDL and the store's projection have drifted", name)
		}
	}
}

// A product survives a store round-trip with every field intact, including a null description and
// the media array. This is what makes the DDL-match test meaningful: the columns are not just
// present, they carry the values the API serves back.
func TestByIDRoundTripsEveryField(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	desc := "A roomy cruiser."
	want := product(t)
	want.Type = domain.TypeYacht
	want.Description = &desc
	want.MediaURLs = []string{"https://cdn.example.com/a.jpg", "https://cdn.example.com/b.jpg"}
	seed(t, pool, want)

	got, err := store.ByID(context.Background(), want.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	if got.ID != want.ID || got.ResourceID != want.ResourceID || got.Type != want.Type ||
		got.Name != want.Name || got.Capacity != want.Capacity || got.Location != want.Location ||
		got.BasePriceMinor != want.BasePriceMinor || got.Currency != want.Currency {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	if got.Description == nil || *got.Description != desc {
		t.Errorf("description = %v, want %q", got.Description, desc)
	}
	if len(got.MediaURLs) != 2 {
		t.Errorf("media_urls = %v, want 2 entries", got.MediaURLs)
	}
}

func TestByIDReturnsErrNotFoundForAnUnknownProduct_Issue13_AC4(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)

	_, err := store.ByID(context.Background(), newID(t))

	if err != domain.ErrNotFound {
		t.Errorf("error = %v, want domain.ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// AC2 — each filter proven by asserting an excluded row is absent
// ---------------------------------------------------------------------------

func TestTypeFilterExcludesOtherTypes_Issue13_AC2(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)

	yacht := product(t)
	yacht.Type = domain.TypeYacht
	seed(t, pool, yacht)

	boat := product(t)
	boat.Type = domain.TypeBoat
	seed(t, pool, boat)

	typ := domain.TypeYacht
	got, err := store.List(context.Background(), domain.Filter{Type: &typ}, nil, 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	found := ids(got)
	if !found[yacht.ID] {
		t.Errorf("the yacht was not returned by type=yacht")
	}
	// The load-bearing assertion: drop the type filter and this boat comes back.
	if found[boat.ID] {
		t.Errorf("the boat was returned by type=yacht — the type filter is not being applied")
	}
}

func TestMinCapacityFilterExcludesSmallerProducts_Issue13_AC2(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)

	small := product(t)
	small.Capacity = 2
	seed(t, pool, small)

	big := product(t)
	big.Capacity = 10
	seed(t, pool, big)

	// Exactly-at-the-boundary must be included: min_capacity is "at least this many".
	exact := product(t)
	exact.Capacity = 8
	seed(t, pool, exact)

	min := 8
	got, err := store.List(context.Background(), domain.Filter{MinCapacity: &min}, nil, 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	found := ids(got)
	if !found[big.ID] || !found[exact.ID] {
		t.Errorf("min_capacity=8 dropped a product that seats 8 or more")
	}
	if found[small.ID] {
		t.Errorf("the 2-seat product was returned by min_capacity=8 — the filter is not being applied")
	}
}

func TestLocationFilterExcludesOtherLocations_Issue13_AC2(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)

	geneva := product(t)
	geneva.Location = "lake-geneva"
	seed(t, pool, geneva)

	como := product(t)
	como.Location = "lake-como"
	seed(t, pool, como)

	loc := "lake-geneva"
	got, err := store.List(context.Background(), domain.Filter{Location: &loc}, nil, 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	found := ids(got)
	if !found[geneva.ID] {
		t.Errorf("the lake-geneva product was not returned")
	}
	if found[como.ID] {
		t.Errorf("the lake-como product was returned by location=lake-geneva — the filter is not being applied")
	}
}

// ---------------------------------------------------------------------------
// Ordering + AC3 — newest first, and stable across an insert made mid-pagination
// ---------------------------------------------------------------------------

func TestListReturnsNewestFirst(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)

	base := time.Now().UTC().Truncate(time.Second)
	var want []string // newest-first order
	for i := 0; i < 4; i++ {
		p := product(t)
		p.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		seed(t, pool, p)
		want = append([]string{p.ID}, want...) // prepend: later inserts are newer
	}

	got, err := store.List(context.Background(), domain.Filter{}, nil, 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d products, want 4", len(got))
	}
	for i, p := range got {
		if p.ID != want[i] {
			t.Errorf("position %d = %s, want %s — order is not newest-first", i, p.ID, want[i])
		}
	}
}

// AC3: a keyset cursor must not shift when a row is inserted ahead of the page boundary mid-walk.
//
// Five products p1..p5 (oldest..newest). Page one (size 2) returns [p5, p4]. THEN a newer product
// p6 is inserted — it sorts ahead of the whole first page. Page two, fetched with the cursor from
// page one, must continue at [p3, p2] exactly: p6 must not appear, no row is skipped, and none is
// served twice. An OFFSET-based scheme would slide the window and re-serve p4 or skip p3.
func TestCursorIsStableAcrossAnInsertMidPagination_Issue13_AC3(t *testing.T) {
	t.Parallel()

	store, pool := newStore(t)
	svc := domain.NewService(store)
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Second)
	p := make([]domain.Product, 5) // p[0]=p1 oldest .. p[4]=p5 newest
	for i := range p {
		prod := product(t)
		prod.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		p[i] = seed(t, pool, prod)
	}

	// Page one.
	page1, err := svc.List(ctx, domain.ListQuery{Limit: 2})
	if err != nil {
		t.Fatalf("page one: %v", err)
	}
	if len(page1.Products) != 2 || page1.Products[0].ID != p[4].ID || page1.Products[1].ID != p[3].ID {
		t.Fatalf("page one = %v, want [p5, p4]", ids(page1.Products))
	}
	if page1.NextCursor == nil {
		t.Fatal("page one has no next_cursor but more products exist")
	}

	// A newer product arrives between page one and page two — the exact race AC3 is about.
	p6 := product(t)
	p6.CreatedAt = base.Add(10 * time.Minute)
	seed(t, pool, p6)

	// Page two resumes from page one's cursor, decoded exactly as the API would.
	cursor, err := domain.DecodeCursor(*page1.NextCursor)
	if err != nil {
		t.Fatalf("decoding page one cursor: %v", err)
	}
	page2, err := svc.List(ctx, domain.ListQuery{Cursor: &cursor, Limit: 2})
	if err != nil {
		t.Fatalf("page two: %v", err)
	}

	if len(page2.Products) != 2 || page2.Products[0].ID != p[2].ID || page2.Products[1].ID != p[1].ID {
		t.Fatalf("page two = %v, want [p3, p2] — the boundary shifted", ids(page2.Products))
	}
	if found := ids(page2.Products); found[p6.ID] {
		t.Error("the product inserted mid-pagination appeared on page two; the cursor is not a stable keyset")
	}

	// Page three completes the walk: [p1], then the end.
	cursor2, err := domain.DecodeCursor(*page2.NextCursor)
	if err != nil {
		t.Fatalf("decoding page two cursor: %v", err)
	}
	page3, err := svc.List(ctx, domain.ListQuery{Cursor: &cursor2, Limit: 2})
	if err != nil {
		t.Fatalf("page three: %v", err)
	}
	if len(page3.Products) != 1 || page3.Products[0].ID != p[0].ID {
		t.Fatalf("page three = %v, want [p1]", ids(page3.Products))
	}
	if page3.NextCursor != nil {
		t.Errorf("page three still has a next_cursor; the walk should be over")
	}
}

// An empty result is an empty slice, never nil: the API's `data` is a required array and a nil
// slice marshals to null, which fails the schema.
func TestListReturnsEmptySliceNotNil(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)

	got, err := store.List(context.Background(), domain.Filter{}, nil, 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got == nil {
		t.Error("List returned nil; an empty result must be an empty slice")
	}
	if len(got) != 0 {
		t.Errorf("List returned %d products against an empty table", len(got))
	}
}
