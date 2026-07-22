// Package domain holds the catalog's read model: what a product is, how a page of products is
// selected and ordered, and the opaque cursor that walks that order.
//
// It imports nothing but the standard library and google/uuid (a pure value type). No pgx, no
// amqp, no otel — CI enforces this (ADR-0009). Everything that needs a driver is a narrow
// interface declared in ports.go and implemented in internal/infra. Keeping the pagination and
// filter logic here is what lets it be reasoned about and unit-tested without a database, while
// the SQL that actually applies the filters is proven separately against a real Postgres.
package domain

import "time"

// Pagination bounds, mirroring contracts/openapi/catalog.yaml's Limit parameter
// (minimum 1, maximum 200, default 50). The contract is the source of truth; these restate it so
// the service can clamp and the API can reject out-of-range values with a 400.
const (
	MinLimit     = 1
	MaxLimit     = 200
	DefaultLimit = 50
)

// ProductType is the kind of thing the business rents, matching the ProductType enum in
// contracts/openapi/catalog.yaml. Adding a value means adding it both here and to the
// product_type enum in migrations/0001_product.sql.
type ProductType string

const (
	TypeBoat            ProductType = "boat"
	TypeYacht           ProductType = "yacht"
	TypeWakesurfSession ProductType = "wakesurf_session"
)

// Valid reports whether t is one of the contract's product types.
//
// Checked rather than assumed because the value arrives as a raw query-string parameter that the
// generated binder accepts as any string. An unrecognised type must become a 400 validation_failed,
// not a query that silently matches nothing and looks like an empty catalog.
func (t ProductType) Valid() bool {
	switch t {
	case TypeBoat, TypeYacht, TypeWakesurfSession:
		return true
	default:
		return false
	}
}

// Product is one item in the catalog. Prices are display-only (Payment computes the authoritative
// charge) and all timestamps are UTC.
//
// BasePriceMinor is int64 rather than int: it is a monetary amount in a currency's minor unit, and
// a 32-bit column would cap a high-value asset priced in cents well below where real yachts sit.
type Product struct {
	ID          string
	ResourceID  string
	Type        ProductType
	Name        string
	Description *string
	Capacity    int
	Location    string

	BasePriceMinor int64
	Currency       string
	MediaURLs      []string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Filter narrows a product listing. Every field is optional; a nil field is "do not filter on
// this". The zero Filter returns the whole catalog, newest first.
//
// Type is a pointer to ProductType rather than a string so an invalid value is caught at the edge
// (ProductType.Valid) before it reaches the store, and a nil pointer is unambiguously "no type
// filter" rather than the empty string, which is a value the enum does not contain.
type Filter struct {
	// Type restricts to one product type.
	Type *ProductType
	// MinCapacity keeps only products seating at least this many people (the party-size filter).
	MinCapacity *int
	// Location is an exact-match location slug, e.g. "lake-geneva".
	Location *string
}

// ListQuery is a fully validated request for a page of products: the filters to apply, the cursor
// to resume from (nil for the first page), and how many products to return.
type ListQuery struct {
	Filter Filter
	Cursor *Cursor
	Limit  int
}

// Page is one page of products and the cursor to fetch the next.
//
// NextCursor is nil on the last page — the contract renders that as a JSON null, meaning "no more
// pages" — and otherwise carries the opaque handle a client passes back as `cursor`.
type Page struct {
	Products   []Product
	NextCursor *string
}
