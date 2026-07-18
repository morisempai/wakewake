-- Idempotency-Key storage. Pasted by each service into its own migration sequence.
--
-- NOTE for the service pasting this: the foreign key is intentionally left out here, because the
-- table it should reference differs per service (reservation, booking, payment). Add it in your
-- own migration as DEFERRABLE INITIALLY DEFERRED — the key row is written BEFORE the domain row
-- exists, so a non-deferred FK would reject the insert that makes concurrent same-key requests
-- serialise.

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
  resource_id         uuid NOT NULL,

  created_at          timestamptz NOT NULL DEFAULT now()
);

-- Supports the retention sweep.
CREATE INDEX idempotency_key_created_at_idx ON idempotency_key (created_at);

COMMENT ON TABLE idempotency_key IS
  'Idempotency-Key -> resource, per the OpenAPI specs. Claimed BEFORE the domain write and in '
  'the same transaction, so concurrent same-key requests serialise here rather than colliding '
  'on a domain constraint and reporting a caller conflict with its own earlier attempt.';
