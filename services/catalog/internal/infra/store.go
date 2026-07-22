// Package infra implements the ports internal/domain declares, using pgx directly.
//
// No repository abstraction and no ORM (service-template): the queries are small and the filter
// combinations are few, so a query builder would add a layer to hide two-line SQL behind. The
// listing query is assembled by appending conditions because the filters are independently
// optional, not because it is dynamic in any deeper sense.
package infra

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/morisempai/wakewake/services/catalog/internal/domain"
)

// Store is the Postgres implementation of domain.Store.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wires the store.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

var _ domain.Store = (*Store)(nil)

// columns is the projection every read uses. type is cast to text because the Go side models it as
// a string constant; registering the enum OID with pgx would buy nothing and make the mapping
// invisible.
const columns = `
  id, resource_id, type::text, name, description,
  capacity, location, base_price_minor, currency, media_urls,
  created_at, updated_at`

const selectByIDSQL = `SELECT` + columns + ` FROM product WHERE id = $1`

// List returns products newest-first, applying every non-nil filter and resuming after the cursor.
//
// Ordering is `created_at DESC, id DESC` and the resume clause is the row-wise comparison
// `(created_at, id) < ($boundary)`. That pairing is the whole of the keyset scheme (AC3): in a
// descending order the rows AFTER a boundary are exactly those whose (created_at, id) tuple is
// strictly smaller, and because id (a UUIDv7) makes the order total, the boundary names one row
// and one row only. A row inserted ahead of the boundary between two pages is newer, so it sorts
// before this page's window and cannot shift it — which an OFFSET could not promise.
func (s *Store) List(ctx context.Context, filter domain.Filter, after *domain.Cursor, limit int) ([]domain.Product, error) {
	var (
		conds []string
		args  []any
	)
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	if filter.Type != nil {
		// Cast the parameter, not the column, so product_type_idx stays usable. The API has already
		// rejected any value outside the enum (ProductType.Valid), so this cast cannot fail on
		// user input.
		conds = append(conds, "type = "+arg(string(*filter.Type))+"::product_type")
	}
	if filter.MinCapacity != nil {
		conds = append(conds, "capacity >= "+arg(*filter.MinCapacity))
	}
	if filter.Location != nil {
		conds = append(conds, "location = "+arg(*filter.Location))
	}
	if after != nil {
		// The keyset boundary. Both halves are cast so Postgres binds them as timestamptz and uuid
		// rather than inferring from an untyped parameter inside the row comparison.
		conds = append(conds, fmt.Sprintf("(created_at, id) < (%s::timestamptz, %s::uuid)",
			arg(after.CreatedAt), arg(after.ID)))
	}

	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	query := fmt.Sprintf(
		`SELECT%s FROM product %s ORDER BY created_at DESC, id DESC LIMIT %s`,
		columns, where, arg(limit))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing products: %w", err)
	}
	defer rows.Close()

	// Non-nil so an empty result is an empty slice rather than nil: the API's `data` is a required
	// array and a nil slice marshals to null, which fails the schema.
	out := make([]domain.Product, 0, limit)
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, fmt.Errorf("catalog: scanning product: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: reading products: %w", err)
	}
	return out, nil
}

// ByID loads one product, or domain.ErrNotFound.
func (s *Store) ByID(ctx context.Context, id string) (domain.Product, error) {
	p, err := scanProduct(s.pool.QueryRow(ctx, selectByIDSQL, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Product{}, domain.ErrNotFound
		}
		return domain.Product{}, fmt.Errorf("catalog: loading product %s: %w", id, err)
	}
	return p, nil
}

// scanProduct reads the standard projection into a domain.Product. It accepts pgx.Row so the same
// mapping serves both the single-row Get and the multi-row List.
func scanProduct(row pgx.Row) (domain.Product, error) {
	var (
		p       domain.Product
		typeStr string
	)
	if err := row.Scan(
		&p.ID, &p.ResourceID, &typeStr, &p.Name, &p.Description,
		&p.Capacity, &p.Location, &p.BasePriceMinor, &p.Currency, &p.MediaURLs,
		&p.CreatedAt, &p.UpdatedAt,
	); err != nil {
		return domain.Product{}, err
	}
	p.Type = domain.ProductType(typeStr)
	p.CreatedAt = p.CreatedAt.UTC()
	p.UpdatedAt = p.UpdatedAt.UTC()
	return p, nil
}
