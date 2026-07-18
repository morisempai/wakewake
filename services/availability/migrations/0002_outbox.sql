-- 0002_outbox.sql — transactional outbox (ADR-0002).
--
-- Events are written here in the SAME transaction as the state change they describe, and a relay
-- forwards them to RabbitMQ afterwards. Publishing directly from the request handler would be a
-- dual write: the reservation could commit while the broker call fails, and `ReservationCreated`
-- would be lost with no trace — or the broker could accept an event for a transaction that then
-- rolled back, announcing a reservation that does not exist.

CREATE TABLE outbox (
  -- The envelope `id` from the AsyncAPI contract, generated at the same instant as the row.
  -- Consumers dedupe on it, so it must be stable across redeliveries: the relay republishes
  -- this same value rather than minting a fresh one.
  id             uuid PRIMARY KEY,
  event          text NOT NULL,
  version        integer NOT NULL CHECK (version >= 1),

  -- Contract: "when the fact happened (transaction commit), NOT when it was published".
  occurred_at    timestamptz NOT NULL,
  correlation_id text NOT NULL,
  payload        jsonb NOT NULL,

  published_at   timestamptz,
  attempts       integer NOT NULL DEFAULT 0,
  last_error     text
);

-- The relay's claim query: unpublished rows, oldest first. Partial, so the index stays small —
-- it holds only the backlog, not the entire event history.
CREATE INDEX outbox_unpublished_idx
  ON outbox (occurred_at)
  WHERE published_at IS NULL;
