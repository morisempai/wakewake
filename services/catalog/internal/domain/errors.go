package domain

import "errors"

// Domain error values. `api` maps each to the status and error code that
// contracts/openapi/catalog.yaml declares; `infra` translates driver errors into these so that no
// pgx type ever reaches a handler.
var (
	// ErrNotFound is an unknown product id. Contract: 404 product_not_found.
	ErrNotFound = errors.New("catalog: no such product")

	// ErrInvalidCursor is a cursor the service did not issue — malformed, truncated, or tampered.
	// Contract: 400 validation_failed on the `cursor` parameter.
	ErrInvalidCursor = errors.New("catalog: cursor is malformed")
)
