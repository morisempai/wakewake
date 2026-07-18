-- 0003_idempotency.sql — Idempotency-Key storage for POST /v1/reservations.
--
-- The contract makes the header mandatory: "Retries are at-least-once, so this is mandatory."
-- Replaying a key with the same body must return the original reservation; replaying it with a
-- different body is a 409 idempotency_key_reuse.
--
-- Columns are the canonical set from shared/platform/idempotency/schema.sql — idempotency.Claim
-- writes exactly these names. The foreign key is deliberately absent from that shared copy,
-- because the table it references differs per service; availability's is `reservation`.

CREATE TABLE idempotency_key (
  -- The client-supplied Idempotency-Key header. Primary key, and that is the whole mechanism:
  -- two concurrent requests carrying the same key collide here, on the speculative insertion
  -- lock, so the second waits for the first to commit or abort rather than racing past it.
  key                 text PRIMARY KEY,

  -- SHA-256 of the request body. Stored as a digest rather than the body: fixed width, keeps the
  -- table small, makes a replay check an equality comparison, and avoids keeping a second copy
  -- of request payloads (which for payment would be an unintended store of customer data).
  request_fingerprint text NOT NULL,

  -- The id this key produced, returned verbatim on a replay.
  --
  -- Written BEFORE the reservation row exists, so the FK must not be checked until commit —
  -- hence DEFERRABLE INITIALLY DEFERRED. The ordering is deliberate and load-bearing: claiming
  -- the key first is what makes two concurrent requests with the same key serialise. The second
  -- one blocks on this primary key's speculative insertion lock until the first commits or
  -- aborts, and then either replays its result or retries cleanly. Inserting the reservation
  -- first would let both reach the exclusion constraint, and a same-key retry would get
  -- 409 reservation_overlap — a caller colliding with its own earlier attempt.
  resource_id         uuid NOT NULL
                        REFERENCES reservation (id) DEFERRABLE INITIALLY DEFERRED,

  created_at          timestamptz NOT NULL DEFAULT now()
);

-- Supports the retention sweep.
CREATE INDEX idempotency_key_created_at_idx ON idempotency_key (created_at);

-- Keys are retained long enough to cover client retry windows, then swept. Retention is not
-- implemented here: it needs a number from docs/nfr.md that the stories never gave, and a
-- wrong guess either breaks idempotency for slow retries or grows this table forever.
-- Tracked separately rather than silently picked.
COMMENT ON TABLE idempotency_key IS
  'Idempotency-Key -> reservation, per contracts/openapi/availability.yaml. Claimed BEFORE the '
  'domain write and in the same transaction, so concurrent same-key requests serialise here '
  'rather than colliding on the exclusion constraint and reporting a caller conflict with its '
  'own earlier attempt. No retention sweep yet — see the idempotency-key retention issue.';
