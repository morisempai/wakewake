-- 0004_processed_event.sql — consumer deduplication (ADR-0002, ADR-0010).
--
-- Payment consumes BookingHeld, and the AsyncAPI contract makes delivery at-least-once with
-- consumers required to dedupe on the envelope id. inbox.Process writes a row here in the SAME
-- transaction as the handler's writes; see the inbox package doc for why any arrangement of two
-- separate transactions either loses work or double-processes it.
--
-- Canonical DDL from shared/platform/inbox/schema.sql, pasted verbatim (hard rule #6: shared/
-- cannot own this service's migration sequence).

CREATE TABLE processed_event (
  -- Which consumer processed it. Part of the key because the same event is legitimately delivered
  -- to several services; keying on event_id alone would let whichever consumer ran first suppress
  -- the event for all the others.
  consumer     text NOT NULL,

  event_id     uuid NOT NULL,
  event        text NOT NULL,

  processed_at timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (consumer, event_id)
);

CREATE INDEX processed_event_processed_at_idx ON processed_event (processed_at);

COMMENT ON TABLE processed_event IS
  'Consumer-side idempotency for the BookingHeld consumer. The row is written in the SAME '
  'transaction as the handler''s writes — a separate transaction would lose or double-process work.';
