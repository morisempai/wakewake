-- 0002_outbox.sql — transactional outbox (ADR-0002).
--
-- PaymentSucceeded / PaymentFailed are written here in the SAME transaction as the payment status
-- change they describe, and a relay forwards them to RabbitMQ afterwards. Publishing directly from
-- the webhook handler would be a dual write: the status could commit while the broker call fails
-- and the event would be lost with no trace, or the broker could accept an event for a transaction
-- that then rolled back. For a payment, a lost PaymentSucceeded is a booking that stays unconfirmed
-- after the customer was charged.
--
-- Canonical DDL from shared/platform/outbox/schema.sql, pasted verbatim. shared/ cannot own another
-- service's migrations (hard rule #6), so the one authoritative copy lives there as text and each
-- service pastes it. Do NOT hand-edit the columns: outbox.Enqueue and the relay write and read
-- exactly these names.

CREATE TABLE outbox (
  id             uuid PRIMARY KEY,

  event          text NOT NULL,
  version        integer NOT NULL DEFAULT 1 CHECK (version >= 1),

  aggregate_type text NOT NULL,
  aggregate_id   uuid NOT NULL,

  payload        jsonb NOT NULL,
  correlation_id text NOT NULL CHECK (correlation_id <> ''),

  -- Transaction time, defaulted by the database. Never written from the application clock: the
  -- contract defines occurred_at as when the fact happened, and the app clock drifts relative to
  -- the one ordering the database's own writes.
  occurred_at    timestamptz NOT NULL DEFAULT now(),

  -- NULL until the broker has CONFIRMED the publish. Set it before the confirm and a crash in
  -- between loses the event, breaking the RPO=0 claim; that ordering is not negotiable.
  published_at   timestamptz,

  attempts       integer NOT NULL DEFAULT 0,
  last_error     text,

  failed_at      timestamptz
);

CREATE INDEX outbox_pending_idx
  ON outbox (occurred_at, id)
  WHERE published_at IS NULL AND failed_at IS NULL;

CREATE INDEX outbox_published_idx
  ON outbox (published_at)
  WHERE published_at IS NOT NULL;

COMMENT ON TABLE outbox IS
  'ADR-0002 transactional outbox. Rows are written in the same transaction as the state change '
  'they describe. A relay publishes them and sets published_at only after a broker confirm.';
