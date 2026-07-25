-- 0002_processed_event.sql — consumer deduplication (ADR-0002, ADR-0010).
--
-- Notification consumes BookingConfirmed, and the AsyncAPI contract makes delivery at-least-once
-- with consumers required to dedupe on the envelope id. shared/platform/inbox.Process writes a row
-- here in the SAME transaction as the handler's writes (the sent_notification row and the email
-- side effect it guards); see the inbox package doc for why any arrangement of two separate
-- transactions either loses the email or sends it twice.
--
-- Canonical DDL from shared/platform/inbox/schema.sql, pasted verbatim (hard rule #6: shared/
-- cannot own this service's migration sequence).

CREATE TABLE processed_event (
  -- Which consumer processed it. Part of the key because the same event is legitimately
  -- delivered to several services: BookingConfirmed goes to notification and, later, to read
  -- models. Keying on event_id alone would let whichever consumer ran first suppress the event
  -- for all the others.
  consumer     text NOT NULL,

  -- The envelope id. A redelivery carries the same value; that is what makes dedupe possible.
  event_id     uuid NOT NULL,

  -- Kept for operators reading this table during an incident. Not part of the key.
  event        text NOT NULL,

  processed_at timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (consumer, event_id)
);

-- Supports pruning. Retention is 30 days (ADR-0010): it must comfortably exceed the longest
-- plausible redelivery window, which is a human manually replaying a dead-letter queue.
CREATE INDEX processed_event_processed_at_idx ON processed_event (processed_at);

COMMENT ON TABLE processed_event IS
  'Consumer-side idempotency. The row is written in the SAME transaction as the handler''s '
  'writes — a separate transaction would either lose work or double-process it on a crash.';
