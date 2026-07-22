package domain

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeStore records the arguments it was called with and returns a canned result. It enforces no
// ordering or filtering — that is the database's job and the integration suite's subject. What it
// lets these unit tests pin is the service's own logic: the lookahead, the trim, and the cursor.
type fakeStore struct {
	products []Product
	err      error

	gotFilter Filter
	gotAfter  *Cursor
	gotLimit  int

	byID    Product
	byIDErr error
}

func (f *fakeStore) List(_ context.Context, filter Filter, after *Cursor, limit int) ([]Product, error) {
	f.gotFilter = filter
	f.gotAfter = after
	f.gotLimit = limit
	if f.err != nil {
		return nil, f.err
	}
	// Return at most `limit` rows, exactly as a real store would honour the cap.
	if len(f.products) > limit {
		return f.products[:limit], nil
	}
	return f.products, nil
}

func (f *fakeStore) ByID(context.Context, string) (Product, error) {
	if f.byIDErr != nil {
		return Product{}, f.byIDErr
	}
	return f.byID, nil
}

func products(t *testing.T, n int) []Product {
	t.Helper()
	out := make([]Product, n)
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	for i := range out {
		id, err := uuid.NewV7()
		if err != nil {
			t.Fatalf("uuid: %v", err)
		}
		// Newest first: earlier indexes are newer, matching the store's created_at DESC order.
		out[i] = Product{ID: id.String(), CreatedAt: base.Add(-time.Duration(i) * time.Minute)}
	}
	return out
}

// The service must ask the store for ONE more row than the page size. That lookahead is how it
// knows whether a next page exists without a second COUNT query, so if it ever asked for exactly
// the page size the last page and a full-but-final page would be indistinguishable.
func TestListAsksTheStoreForOneExtraRow(t *testing.T) {
	t.Parallel()

	store := &fakeStore{products: products(t, 3)}
	svc := NewService(store)

	if _, err := svc.List(context.Background(), ListQuery{Limit: 10}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if store.gotLimit != 11 {
		t.Errorf("store asked for %d rows, want limit+1 = 11", store.gotLimit)
	}
}

// A full lookahead row means there is another page: the page is trimmed to the requested size and
// NextCursor points at the LAST returned row, not the dropped lookahead row.
func TestListReturnsANextCursorWhenAFurtherPageExists(t *testing.T) {
	t.Parallel()

	all := products(t, 6) // limit 5 + 1 lookahead
	store := &fakeStore{products: all}
	svc := NewService(store)

	page, err := svc.List(context.Background(), ListQuery{Limit: 5})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(page.Products) != 5 {
		t.Fatalf("page has %d products, want 5 (the lookahead row must be dropped)", len(page.Products))
	}
	if page.NextCursor == nil {
		t.Fatal("NextCursor is nil, but a lookahead row proved a further page exists")
	}

	want := EncodeCursor(all[4]) // the last row actually returned, not all[5]
	if *page.NextCursor != want {
		t.Errorf("NextCursor points at the wrong boundary; the dropped lookahead row must not be the cursor")
	}
}

// No lookahead row means the last page: NextCursor is nil, which the contract renders as JSON null.
func TestListReturnsNilCursorOnTheLastPage(t *testing.T) {
	t.Parallel()

	store := &fakeStore{products: products(t, 4)} // fewer than limit+1
	svc := NewService(store)

	page, err := svc.List(context.Background(), ListQuery{Limit: 5})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Products) != 4 {
		t.Errorf("page has %d products, want 4", len(page.Products))
	}
	if page.NextCursor != nil {
		t.Errorf("NextCursor = %q on the last page, want nil", *page.NextCursor)
	}
}

// An out-of-range or zero limit is clamped rather than trusted: the API rejects bad values with a
// 400 before this point, so a zero here is an internal bug, and querying for zero+1 rows would
// silently return an always-empty page.
func TestListClampsTheLimit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		limit     int
		wantAsked int
	}{
		{"zero falls back to the default", 0, DefaultLimit + 1},
		{"over the max is capped", MaxLimit + 100, MaxLimit + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeStore{}
			svc := NewService(store)
			if _, err := svc.List(context.Background(), ListQuery{Limit: tc.limit}); err != nil {
				t.Fatalf("List: %v", err)
			}
			if store.gotLimit != tc.wantAsked {
				t.Errorf("store asked for %d rows, want %d", store.gotLimit, tc.wantAsked)
			}
		})
	}
}

// The filter and cursor are passed through to the store untouched — the service decides paging, the
// store decides matching.
func TestListForwardsFilterAndCursor(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	svc := NewService(store)

	typ := TypeYacht
	loc := "lake-geneva"
	cap := 6
	after := &Cursor{CreatedAt: time.Now().UTC(), ID: newUUID(t)}

	_, err := svc.List(context.Background(), ListQuery{
		Filter: Filter{Type: &typ, MinCapacity: &cap, Location: &loc},
		Cursor: after,
		Limit:  5,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if store.gotFilter.Type == nil || *store.gotFilter.Type != TypeYacht {
		t.Errorf("type filter not forwarded: %+v", store.gotFilter.Type)
	}
	if store.gotFilter.MinCapacity == nil || *store.gotFilter.MinCapacity != 6 {
		t.Errorf("min_capacity filter not forwarded: %+v", store.gotFilter.MinCapacity)
	}
	if store.gotFilter.Location == nil || *store.gotFilter.Location != loc {
		t.Errorf("location filter not forwarded: %+v", store.gotFilter.Location)
	}
	if store.gotAfter != after {
		t.Errorf("cursor not forwarded")
	}
}

// A store error propagates; a page is not fabricated from a failed read.
func TestListPropagatesStoreErrors(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("boom")
	svc := NewService(&fakeStore{err: sentinel})

	if _, err := svc.List(context.Background(), ListQuery{Limit: 5}); !errors.Is(err, sentinel) {
		t.Errorf("List error = %v, want %v", err, sentinel)
	}
}

// Get delegates to the store and surfaces ErrNotFound unchanged (AC4's domain half).
func TestGetSurfacesNotFound(t *testing.T) {
	t.Parallel()

	svc := NewService(&fakeStore{byIDErr: ErrNotFound})

	if _, err := svc.Get(context.Background(), newUUID(t)); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get error = %v, want ErrNotFound", err)
	}
}
