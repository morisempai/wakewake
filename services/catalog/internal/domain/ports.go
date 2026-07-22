package domain

import "context"

// The ports this domain needs, declared HERE rather than in internal/infra.
//
// The interface belongs to the consumer, not the implementation (service-template's layering
// rule). That is what keeps this package free of pgx and lets the service be unit-tested with a
// plain fake — and CI enforces it with a guard that refuses driver-carrying imports anywhere under
// internal/domain (ADR-0009).

// Store reads products. Catalog owns its database exclusively (hard rule #6); this is the only way
// the domain reaches it.
type Store interface {
	// List returns products newest-first (created_at DESC, id DESC), applying every non-nil field
	// of filter. When after is non-nil, only products strictly older than that boundary in the
	// sort order are returned, which is what makes the paging a keyset walk rather than an offset.
	//
	// It returns at most limit rows. The service asks for one more than the page size so it can
	// tell whether a further page exists without a second COUNT query.
	List(ctx context.Context, filter Filter, after *Cursor, limit int) ([]Product, error)

	// ByID loads one product, or ErrNotFound.
	ByID(ctx context.Context, id string) (Product, error)
}
