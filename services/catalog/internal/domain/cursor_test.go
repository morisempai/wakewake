package domain

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"
)

// encodeRaw base64url-encodes an already-formed cursor payload, so a test can craft a token whose
// inner shape is deliberately wrong (bad timestamp, missing separator) rather than random bytes.
func encodeRaw(payload string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

func productAt(t *testing.T, createdAt time.Time) Product {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("generating uuid: %v", err)
	}
	return Product{ID: id.String(), CreatedAt: createdAt}
}

// A cursor must survive an encode/decode round-trip byte-for-byte on the fields that matter, or a
// client paging through the catalog would resume from the wrong boundary.
func TestEncodeDecodeCursorRoundTrips(t *testing.T) {
	t.Parallel()

	// A timestamp with sub-second precision, because the database stores microseconds and a cursor
	// that dropped them would compare as a different, earlier boundary and re-serve rows.
	created := time.Date(2026, 7, 22, 13, 30, 45, 123456000, time.UTC)
	p := productAt(t, created)

	got, err := DecodeCursor(EncodeCursor(p))
	if err != nil {
		t.Fatalf("DecodeCursor after EncodeCursor: %v", err)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %s, want %s", got.CreatedAt, created)
	}
	if got.ID != p.ID {
		t.Errorf("ID = %s, want %s", got.ID, p.ID)
	}
}

// A non-UTC created_at must encode to the same boundary as its UTC form: the keyset comparison
// runs against a timestamptz, and two tokens for the same instant must be identical on the way in.
func TestEncodeCursorNormalisesToUTC(t *testing.T) {
	t.Parallel()

	loc := time.FixedZone("UTC+2", 2*60*60)
	instant := time.Date(2026, 7, 22, 15, 30, 45, 0, loc)
	p := productAt(t, instant)

	got, err := DecodeCursor(EncodeCursor(p))
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if !got.CreatedAt.Equal(instant) {
		t.Errorf("decoded instant = %s, want the same instant as %s", got.CreatedAt, instant)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Errorf("decoded location = %s, want UTC", got.CreatedAt.Location())
	}
}

// Every malformed token maps to ErrInvalidCursor, so the API answers a stale or tampered cursor
// with one 400 rather than a 500 or a leak of which part failed.
func TestDecodeCursorRejectsMalformedTokens(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"not base64", "!!!not-base64!!!"},
		{"no separator", encodeRaw("2026-07-22T13:30:45Z")},
		{"bad timestamp", encodeRaw("not-a-time|" + newUUID(t))},
		{"bad uuid", encodeRaw("2026-07-22T13:30:45Z|not-a-uuid")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := DecodeCursor(tc.token); err != ErrInvalidCursor {
				t.Errorf("DecodeCursor(%q) error = %v, want ErrInvalidCursor", tc.token, err)
			}
		})
	}
}

func newUUID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("generating uuid: %v", err)
	}
	return id.String()
}
