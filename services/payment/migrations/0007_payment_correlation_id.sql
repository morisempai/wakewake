-- 0007_payment_correlation_id.sql — thread the request correlation id across the Stripe webhook.
--
-- The Stripe webhook is an external callback with no X-Correlation-Id header, so correlation.Middleware
-- mints a BRAND-NEW id on every delivery. The PaymentSucceeded/PaymentFailed events it stages (and the
-- notification email downstream) then carry that fresh id, disconnected from the original createPayment
-- request — the customer journey splits into two trace "islands" joined only by booking_id (issue #23).
--
-- The fix is to remember, on the payment aggregate, the correlation id captured when the PaymentIntent
-- was created (the id the gateway forwarded on createPayment). The webhook loads the payment by its
-- provider_payment_id anyway; with the id on the row it can re-hydrate the context before staging events,
-- so the outcome carries the ORIGINAL id rather than a minted one.
--
-- Nullable, no default, forward-only: this is a plain reference id, NOT card data (the PCI column
-- allowlist test 0001/AC2 still holds). Nullable because rows created before this migration predate the
-- feature — they carry NULL, and RecordOutcome falls back to the outbox's mint-on-empty behaviour for
-- them rather than stamping an empty id.

ALTER TABLE payment ADD COLUMN correlation_id text;

COMMENT ON COLUMN payment.correlation_id IS
  'The correlation id captured at createPayment time (the id the gateway forwarded). Re-hydrated onto '
  'the context when the Stripe webhook records the outcome, so PaymentSucceeded/PaymentFailed carry the '
  'original request id rather than one minted for the external callback. Nullable: legacy rows predate it.';
