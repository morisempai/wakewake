package domain

import "context"

// Service is the catalog read use-case layer: it decides which rows a page contains and how the
// next cursor is formed, and the store fetches them.
//
// It holds no state beyond its dependencies, so it is safe for concurrent use by every HTTP
// handler at once.
type Service struct {
	store Store
}

// NewService wires the service.
func NewService(store Store) *Service {
	return &Service{store: store}
}

// List returns one page of products, newest first, plus the cursor for the next page.
//
// The "is there a next page?" question is answered without a second query: the store is asked for
// one more product than the page needs, and the presence of that extra row is what sets
// NextCursor. The extra row is then dropped, so a client never sees it twice — it reappears as the
// first row of the next page, fetched from the cursor built off the last row actually returned.
//
// Limit is clamped to the contract's bounds defensively. The API rejects out-of-range values with
// a 400 before reaching here, so a bad value at this point would be an internal caller's bug; the
// clamp keeps that from turning into a query for zero or a million rows.
func (s *Service) List(ctx context.Context, q ListQuery) (Page, error) {
	limit := q.Limit
	if limit < MinLimit {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}

	// One extra row is the lookahead: if it comes back, there is at least one more page.
	products, err := s.store.List(ctx, q.Filter, q.Cursor, limit+1)
	if err != nil {
		return Page{}, err
	}

	if len(products) <= limit {
		// The lookahead row did not materialise, so this is the last page.
		return Page{Products: products, NextCursor: nil}, nil
	}

	products = products[:limit]
	next := EncodeCursor(products[len(products)-1])
	return Page{Products: products, NextCursor: &next}, nil
}

// Get returns one product, or ErrNotFound.
func (s *Service) Get(ctx context.Context, id string) (Product, error) {
	return s.store.ByID(ctx, id)
}
