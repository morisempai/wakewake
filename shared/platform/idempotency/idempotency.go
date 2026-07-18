// Package idempotency implements the Idempotency-Key contract shared by every write endpoint.
//
// The semantics come straight from the OpenAPI specs and are identical in availability, booking,
// and payment: replaying a key with the SAME body returns the original result; replaying it with
// a DIFFERENT body is a 409 idempotency_key_reuse. That is contract-defined behaviour used
// three ways, and it is subtle enough — request fingerprinting, concurrent in-flight replays —
// that three implementations would mean three different bugs.
//
// Why it matters more here than in most systems: a retried POST /v1/reservations without working
// idempotency is a double booking, and a retried payment is a double charge. The client cannot
// tell a lost response from a failed request, so it will retry, and the server has to be the one
// that gets this right.
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrKeyReuse means the key was already used with a different request body. Callers map it to
// 409 idempotency_key_reuse.
var ErrKeyReuse = errors.New("idempotency: key reused with a different request body")

// Fingerprint hashes a request body so replays can be compared cheaply.
//
// The digest is stored rather than the body: it is fixed-width, it keeps the table small, and it
// means a replay check is an equality comparison rather than a JSON diff. It also avoids keeping
// a second copy of request payloads — which, for payment, could otherwise become an unintended
// store of customer data.
func Fingerprint(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Claim reserves a key inside the caller's transaction.
//
// It returns the previously-stored resource id and replayed=true when the key has been seen with
// an identical body. It returns ErrKeyReuse when the body differs.
//
// The claim MUST happen before the domain write, and the two must share a transaction. That
// ordering is load-bearing rather than stylistic: claiming first means two concurrent requests
// carrying the same key serialise on this table's primary key, so the second blocks until the
// first commits and can then replay its result. Insert the domain row first and both requests
// reach the exclusion constraint instead — at which point a client retrying its own request gets
// 409 reservation_overlap and is told the slot is taken by what was actually itself.
func Claim(ctx context.Context, tx pgx.Tx, key, fingerprint, resourceID string) (existing string, replayed bool, err error) {
	if key == "" {
		return "", false, fmt.Errorf("idempotency: key is empty")
	}

	const sql = `
INSERT INTO idempotency_key (key, request_fingerprint, resource_id)
VALUES ($1, $2, $3)
ON CONFLICT (key) DO UPDATE
   SET key = idempotency_key.key   -- no-op, so RETURNING sees the stored row
RETURNING request_fingerprint, resource_id, (xmax = 0) AS inserted`

	var storedFingerprint, storedResource string
	var inserted bool
	if err := tx.QueryRow(ctx, sql, key, fingerprint, resourceID).
		Scan(&storedFingerprint, &storedResource, &inserted); err != nil {
		return "", false, fmt.Errorf("idempotency: claiming %s: %w", key, err)
	}

	if inserted {
		return resourceID, false, nil
	}
	if storedFingerprint != fingerprint {
		return "", false, ErrKeyReuse
	}
	return storedResource, true, nil
}

// Lookup returns the resource a key already maps to, for read paths that want to short-circuit
// before starting a transaction.
func Lookup(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, key string) (resourceID string, found bool, err error) {
	const sql = `SELECT resource_id FROM idempotency_key WHERE key = $1`

	if err := q.QueryRow(ctx, sql, key).Scan(&resourceID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("idempotency: looking up %s: %w", key, err)
	}
	return resourceID, true, nil
}
