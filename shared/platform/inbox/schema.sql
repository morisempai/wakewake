-- Consumer deduplication table (ADR-0002, ADR-0010).
--
-- Shipped as text, pasted by each consuming service into its own migration sequence — shared/
-- cannot own another service's migrations (hard rule #6).

CREATE TABLE processed_event (
  -- Which consumer processed it. Part of the key because the same event is legitimately
  -- delivered to several services: BookingConfirmed goes to notification and, later, to read
  -- models. Keying on event_id alone would let whichever consumer ran first suppress the event
  -- for all the others — a bug that only appears once a second consumer is added, long after
  -- the first one's tests were written.
  consumer     text NOT NULL,

  -- The envelope id. A redelivery carries the same value; that is what makes dedupe possible.
  event_id     uuid NOT NULL,

  -- Kept for operators reading this table during an incident. Not part of the key.
  event        text NOT NULL,

  processed_at timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (consumer, event_id)
);

-- Supports pruning. Retention is 30 days (ADR-0010): it must comfortably exceed the longest
-- plausible redelivery window, which is a human manually replaying a dead-letter queue. 30 days
-- matches Loki's log retention in docs/nfr.md, so any replay an operator can still investigate
-- is a replay that still dedupes.
CREATE INDEX processed_event_processed_at_idx ON processed_event (processed_at);

COMMENT ON TABLE processed_event IS
  'Consumer-side idempotency. The row is written in the SAME transaction as the handler''s '
  'writes — a separate transaction would either lose work or double-process it on a crash.';
