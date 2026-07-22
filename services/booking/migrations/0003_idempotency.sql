-- 0003_idempotency.sql — Idempotency-Key storage for POST /v1/bookings.
--
-- The contract makes the header mandatory: "A retried request with the same key and body returns
-- the original booking instead of creating a second hold on the same slot." Replaying a key with
-- the same body must return the original booking; replaying it with a different body is a
-- 409 idempotency_key_reuse.
--
-- Columns are the canonical set from shared/platform/idempotency/schema.sql — idempotency.Claim
-- writes exactly these names. The foreign key is added here because the table it references is
-- this service's own `booking`, and it is DEFERRABLE INITIALLY DEFERRED because the key is claimed
-- BEFORE the booking row exists, in the same transaction.

CREATE TABLE idempotency_key (
  -- The client-supplied Idempotency-Key header. Primary key, and that is the whole mechanism:
  -- two concurrent requests carrying the same key collide here, on the speculative insertion
  -- lock, so the second waits for the first to commit or abort rather than racing past it.
  key                 text PRIMARY KEY,

  -- SHA-256 of the request body. Stored as a digest rather than the body: fixed width, keeps the
  -- table small, makes a replay check an equality comparison, and avoids keeping a second copy
  -- of request payloads.
  request_fingerprint text NOT NULL,

  -- The booking id this key produced, returned verbatim on a replay.
  --
  -- Written BEFORE the booking row exists, so the FK must not be checked until commit — hence
  -- DEFERRABLE INITIALLY DEFERRED. The ordering is deliberate and load-bearing: claiming the key
  -- first is what makes two concurrent requests with the same key serialise on this primary key,
  -- so the second blocks until the first commits and can then replay its result instead of
  -- creating a second hold on the same slot.
  resource_id         uuid NOT NULL
                        REFERENCES booking (id) DEFERRABLE INITIALLY DEFERRED,

  created_at          timestamptz NOT NULL DEFAULT now()
);

-- Supports the retention sweep.
CREATE INDEX idempotency_key_created_at_idx ON idempotency_key (created_at);

-- Retention is not implemented here: it needs a number from docs/nfr.md the stories never gave,
-- and a wrong guess either breaks idempotency for slow retries or grows this table forever.
COMMENT ON TABLE idempotency_key IS
  'Idempotency-Key -> booking, per contracts/openapi/booking.yaml. Claimed BEFORE the booking '
  'insert and in the same transaction, so concurrent same-key requests serialise here rather '
  'than each creating a hold. The column is named resource_id for parity with the shared DDL; '
  'here it holds the booking id. No retention sweep yet — see the idempotency-key retention issue.';
