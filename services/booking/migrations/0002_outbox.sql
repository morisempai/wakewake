-- 0002_outbox.sql — transactional outbox (ADR-0002).
--
-- BookingHeld / BookingConfirmed / BookingCancelled are written here in the SAME transaction as
-- the state change they describe, and a relay forwards them to RabbitMQ afterwards. Publishing
-- directly from the request handler or the consumer would be a dual write: the booking could
-- commit while the broker call fails and the event would be lost with no trace, or the broker
-- could accept an event for a transaction that then rolled back.
--
-- Canonical DDL from shared/platform/outbox/schema.sql, pasted verbatim. shared/ cannot own
-- another service's migrations (hard rule #6), so the one authoritative copy lives there as text
-- and each service pastes it. Do NOT hand-edit the columns: outbox.Enqueue and the relay write
-- and read exactly these names.

CREATE TABLE outbox (
  -- The envelope id from the AsyncAPI contract. UUIDv7, so ordering by id approximates ordering
  -- by time. Consumers dedupe on this, so the relay MUST republish this same value on a retry
  -- rather than minting a fresh one — a new id on every attempt would defeat consumer dedupe
  -- entirely and turn at-least-once delivery into at-least-once *processing*.
  id             uuid PRIMARY KEY,

  event          text NOT NULL,
  version        integer NOT NULL DEFAULT 1 CHECK (version >= 1),

  -- What the event is about. Not part of the wire envelope; present so an operator can ask
  -- "what happened to booking X" without parsing every payload.
  aggregate_type text NOT NULL,
  aggregate_id   uuid NOT NULL,

  payload        jsonb NOT NULL,
  correlation_id text NOT NULL CHECK (correlation_id <> ''),

  -- Transaction time, defaulted by the database. Never written from the application clock:
  -- the contract defines occurred_at as when the fact happened, and the app clock drifts
  -- relative to the one ordering the database's own writes.
  occurred_at    timestamptz NOT NULL DEFAULT now(),

  -- NULL until the broker has CONFIRMED the publish. Set it before the confirm and a crash in
  -- between loses the event, breaking the RPO=0 claim; that ordering is not negotiable.
  published_at   timestamptz,

  attempts       integer NOT NULL DEFAULT 0,
  last_error     text,

  -- Set when attempts are exhausted. The row is then skipped rather than retried forever, so a
  -- single poison event cannot wedge the relay behind it while the rest of the backlog waits.
  failed_at      timestamptz
);

-- The relay's claim query: unpublished, unfailed, oldest first. Partial, so the index holds only
-- the backlog rather than the entire event history — which is what keeps it small on a table
-- that only ever grows between prunes.
CREATE INDEX outbox_pending_idx
  ON outbox (occurred_at, id)
  WHERE published_at IS NULL AND failed_at IS NULL;

-- Supports pruning published rows (docs/nfr.md: 7 days after the relay confirms publish).
CREATE INDEX outbox_published_idx
  ON outbox (published_at)
  WHERE published_at IS NOT NULL;

COMMENT ON TABLE outbox IS
  'ADR-0002 transactional outbox. Rows are written in the same transaction as the state change '
  'they describe. A relay publishes them and sets published_at only after a broker confirm.';
