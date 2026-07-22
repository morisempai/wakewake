package domain

import (
	"encoding/base64"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Cursor is the keyset boundary a listing resumes from: the (created_at, id) of the last product
// on the previous page.
//
// It is a keyset, not an offset, on purpose (AC3). An offset counts rows from the top, so a
// product inserted ahead of the boundary between two page fetches shifts every subsequent page by
// one — the client sees a row twice or misses one. A keyset names an exact row in the total order,
// so an insert ahead of it changes nothing about where the next page begins.
type Cursor struct {
	// CreatedAt is the boundary product's created_at, always UTC.
	CreatedAt time.Time
	// ID breaks ties when two products share a created_at, making the order total. Without it a
	// pair of same-instant rows could straddle a page boundary and be served twice or skipped.
	ID string
}

// cursorSeparator joins the two fields inside the opaque token. '|' cannot appear in an RFC 3339
// timestamp or a UUID, so a single split recovers both fields unambiguously.
const cursorSeparator = "|"

// EncodeCursor renders a product's sort position as the opaque token the contract returns as
// next_cursor.
//
// The token is base64url of "<created_at RFC3339Nano>|<id>". Opaque means the client must not
// parse it, which is exactly why it is encoded rather than returned as a readable pair: a client
// that started constructing its own cursors would couple itself to the sort key and break the
// first time it changed. RFC3339Nano preserves the full timestamp precision the database stores,
// so the boundary compares identically on the way back in.
func EncodeCursor(p Product) string {
	raw := p.CreatedAt.UTC().Format(time.RFC3339Nano) + cursorSeparator + p.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses a token produced by EncodeCursor, returning ErrInvalidCursor for anything it
// did not produce.
//
// Every malformed shape maps to the same error so the API answers a tampered or stale cursor with
// a single 400 validation_failed rather than leaking which part failed. The id is parsed as a UUID
// because it is interpolated into a keyset comparison against a uuid column; a non-UUID here is a
// client error to reject, not a query to run.
func DecodeCursor(token string) (Cursor, error) {
	if token == "" {
		return Cursor{}, ErrInvalidCursor
	}

	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}

	parts := strings.SplitN(string(raw), cursorSeparator, 2)
	if len(parts) != 2 {
		return Cursor{}, ErrInvalidCursor
	}

	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	if _, err := uuid.Parse(parts[1]); err != nil {
		return Cursor{}, ErrInvalidCursor
	}

	return Cursor{CreatedAt: createdAt.UTC(), ID: parts[1]}, nil
}
