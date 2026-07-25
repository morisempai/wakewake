-- 0001_sent_notification.sql — the record of every confirmation email this service sent.
--
-- Two jobs in one table (issue #19):
--
--   1. Idempotency. event_id is the envelope id and the PRIMARY KEY, so a second attempt to
--      record the same delivery is rejected by the unique constraint. Combined with the inbox's
--      processed_event row written in the SAME transaction (0002), a duplicate BookingConfirmed
--      results in exactly one email — see shared/platform/inbox for why one transaction, not two,
--      is the only correct arrangement.
--   2. Audit. Which booking, which customer, when, and what subject — enough to answer "did the
--      customer who paid actually get their confirmation?" during an incident.
--
-- No raw recipient address is stored. BookingConfirmed carries no PII by construction (contract
-- x-notes, NFR-4); the address is derived at send time by the RecipientResolver, and persisting
-- it here would copy PII back into a database that GDPR erasure would then have to reach. Only the
-- REDACTED form is kept, which is also the only form allowed in logs (AC4).

CREATE TABLE sent_notification (
  -- The envelope id of the BookingConfirmed that caused this email. THE idempotency key: a
  -- redelivery carries the same value, and this being the primary key makes a second send a
  -- constraint violation rather than a duplicate email.
  event_id           uuid PRIMARY KEY,

  -- What the email was about. booking_id lets an operator find the confirmation for a booking;
  -- customer_id is the (non-PII) handle the recipient was resolved from, and is enough to
  -- re-derive the address without storing it.
  booking_id         uuid NOT NULL,
  customer_id        uuid NOT NULL,

  -- The delivery channel. 'email' for now; kept explicit so a future SMS/push channel is an
  -- added value, not a schema change. Marketing/campaign channels are deferred (CLAUDE.md).
  channel            text NOT NULL DEFAULT 'email',

  -- The recipient in redacted form ONLY (e.g. 'c***@example.test'). Never the raw address — see
  -- the table note above.
  recipient_redacted text NOT NULL,

  subject            text NOT NULL,

  -- Propagated unchanged from the BookingConfirmed envelope, so this row joins to the trace and
  -- logs of the one booking flow that produced it.
  correlation_id     text NOT NULL,

  sent_at            timestamptz NOT NULL DEFAULT now()
);

-- Supports "what did we send recently" scans and future retention pruning without a table scan.
CREATE INDEX sent_notification_sent_at_idx ON sent_notification (sent_at);

COMMENT ON TABLE sent_notification IS
  'One row per confirmation email sent. event_id (the envelope id) is the idempotency key; the '
  'row is written in the SAME transaction as the processed_event dedupe row. No raw recipient '
  'address is stored — only its redacted form.';
