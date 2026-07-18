-- 0003_idempotency.sql — Idempotency-Key storage for POST /v1/reservations.
--
-- The contract makes the header mandatory: "Retries are at-least-once, so this is mandatory."
-- Replaying a key with the same body must return the original reservation; replaying it with a
-- different body is a 409 idempotency_key_reuse.

CREATE TABLE idempotency_key (
  key                  text PRIMARY KEY,

  -- SHA-256 of the canonicalised request body. Storing the digest rather than the body keeps
  -- this table small and means a replay comparison is a fixed-width equality check.
  request_fingerprint  text NOT NULL,

  -- Written BEFORE the reservation row exists, so the FK must not be checked until commit.
  -- The ordering is deliberate and load-bearing: claiming the key first is what makes two
  -- concurrent requests with the same key serialise. The second one blocks on this primary
  -- key's speculative insertion lock until the first commits or aborts, and then either
  -- replays its result or retries cleanly. Inserting the reservation first would let both
  -- reach the exclusion constraint, and a same-key retry would get 409 reservation_overlap —
  -- a caller colliding with its own earlier attempt.
  reservation_id       uuid NOT NULL REFERENCES reservation (id) DEFERRABLE INITIALLY DEFERRED,

  created_at           timestamptz NOT NULL DEFAULT now()
);

-- Keys are retained long enough to cover client retry windows, then swept. Retention is not
-- implemented here: it needs a number from docs/nfr.md that the stories never gave, and a
-- wrong guess either breaks idempotency for slow retries or grows this table forever.
-- Tracked separately rather than silently picked.
COMMENT ON TABLE idempotency_key IS
  'Idempotency-Key -> reservation, per contracts/openapi/availability.yaml. No retention sweep '
  'yet — see the idempotency-key retention issue.';
