-- 0003_idempotency.sql — Idempotency-Key storage for POST /v1/payments.
--
-- payment.yaml makes the header mandatory ("a retried charge without one is a double-charge") and
-- passes it through to Stripe's own idempotency. This table is the LOCAL half: it maps a key to the
-- payment it produced so a replay returns the original, and it detects a key reused with a different
-- body (409 idempotency_key_reuse).
--
-- Columns are the canonical set from shared/platform/idempotency/schema.sql — idempotency.Claim
-- writes exactly these names. The column is named resource_id for parity with the shared DDL; here
-- it holds the payment id. The foreign key is DEFERRABLE INITIALLY DEFERRED because the key is
-- claimed in the same transaction as, and just before, the payment row is inserted.

CREATE TABLE idempotency_key (
  -- The client-supplied Idempotency-Key header. Primary key, and that is the whole mechanism: two
  -- concurrent requests carrying the same key collide here on the speculative insertion lock, so
  -- the second waits for the first to commit or abort rather than racing past it into a second row.
  key                 text PRIMARY KEY,

  -- SHA-256 of the request body. A digest rather than the body: fixed width, keeps the table small,
  -- makes a replay check an equality comparison, and — for payment specifically — avoids keeping a
  -- second copy of request payloads anywhere near customer data.
  request_fingerprint text NOT NULL,

  -- The payment id this key produced, returned verbatim on a replay. Written before the payment row
  -- exists (same transaction), so the FK must not be checked until commit — hence DEFERRABLE.
  resource_id         uuid NOT NULL
                        REFERENCES payment (id) DEFERRABLE INITIALLY DEFERRED,

  created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idempotency_key_created_at_idx ON idempotency_key (created_at);

COMMENT ON TABLE idempotency_key IS
  'Idempotency-Key -> payment, per contracts/openapi/payment.yaml. Claimed in the same transaction '
  'as the payment insert, so concurrent same-key requests serialise here. The column is named '
  'resource_id for parity with the shared DDL; here it holds the payment id. No retention sweep yet.';
