-- 0006_stripe_event.sql — webhook deduplication, keyed on the Stripe event id.
--
-- payment.yaml: "Stripe retries on non-2xx and may deliver duplicates, so handling is idempotent on
-- the Stripe event id." This is a DIFFERENT dedup namespace from processed_event (which keys on our
-- AsyncAPI envelope id): the webhook is an inbound HTTP call from Stripe, not a broker delivery.
--
-- The row is inserted in the SAME transaction as the payment status change and the outbox event, so
-- a duplicate webhook that finds the id already present makes no state change and emits no second
-- event — exactly-once processing of each Stripe event, not merely at-least-once.

CREATE TABLE stripe_event (
  -- The Stripe event id (evt_...). Primary key: the second delivery of the same event conflicts
  -- here and is acknowledged (200) without re-applying the outcome.
  event_id     text PRIMARY KEY,

  -- The Stripe event type (payment_intent.succeeded, ...), kept for operators reading this table.
  type         text NOT NULL,

  processed_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX stripe_event_processed_at_idx ON stripe_event (processed_at);

COMMENT ON TABLE stripe_event IS
  'Stripe webhook idempotency, keyed on the Stripe event id. Inserted in the same transaction as '
  'the payment status change and outbox row, so a duplicate webhook mutates nothing and emits nothing.';
