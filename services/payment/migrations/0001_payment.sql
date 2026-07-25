-- 0001_payment.sql — the payment aggregate.
--
-- PCI is the whole design constraint here (contracts/openapi/payment.yaml, service CLAUDE.md):
-- raw card data NEVER reaches this service and is NEVER stored. The customer's browser sends the
-- card straight to Stripe and gets back a token; we persist only Stripe ids, the amount (read from
-- the booking, never the client), the currency, and the outcome. There is no column here for a PAN,
-- a CVV, an expiry, or a cardholder name — and a test (AC2) asserts that this stays true by
-- inspecting the live table's columns, so a future migration that adds one fails loudly.
--
-- `provider_payment_id` is a Stripe PaymentIntent id (pi_...). It is a REFERENCE to a charge, not an
-- instrument — it cannot be replayed to move money and it carries no card data.

CREATE TYPE payment_status AS ENUM ('pending', 'succeeded', 'failed');

CREATE TABLE payment (
  id                  uuid PRIMARY KEY,

  -- The held booking being paid for. UNIQUE (named constraint, so infra can match the violation by
  -- name) so a booking has at most one payment in flight: a second createPayment for the same
  -- booking under a different Idempotency-Key collides here and is surfaced as 409
  -- payment_already_exists rather than opening a second charge.
  booking_id          uuid NOT NULL,

  -- pending → PaymentIntent created, outcome unknown; succeeded/failed are terminal and set only
  -- by a verified Stripe webhook (payment.yaml: the webhook is the authoritative outcome).
  status              payment_status NOT NULL DEFAULT 'pending',

  -- The amount to charge, in the currency's minor unit. Read from the booking (via the BookingHeld
  -- context), NEVER from the request — a client-supplied amount would let a customer set their own
  -- price. Integer, never a float: this is a payments system and 0.1 + 0.2 != 0.3 in binary.
  amount_minor        bigint NOT NULL CHECK (amount_minor >= 0),
  currency            text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),

  -- Only Stripe is supported in this slice; the constant is pinned so a stray value is rejected.
  provider            text NOT NULL DEFAULT 'stripe' CHECK (provider = 'stripe'),

  -- The Stripe PaymentIntent id. A reference, not an instrument (see the file header). Indexed
  -- below because the webhook looks the payment up by it.
  provider_payment_id text NOT NULL,

  -- Stripe's decline code (card_declined, expired_card, ...) when the charge failed. Set exactly
  -- when status is failed — the biconditional below enforces it, so a succeeded/pending row can
  -- never carry a spurious failure and a failed row always explains itself.
  failure_code        text,

  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT payment_failed_iff_failure_code CHECK ((status = 'failed') = (failure_code IS NOT NULL)),

  CONSTRAINT payment_booking_id_unique UNIQUE (booking_id)
);

-- The webhook resolves its PaymentIntent back to our payment row by the Stripe id.
CREATE UNIQUE INDEX payment_provider_payment_id_idx ON payment (provider_payment_id);

COMMENT ON TABLE payment IS
  'Payment aggregate. Contains NO card data by construction (PCI): only Stripe ids, status, and the '
  'amount read from the booking. The authoritative outcome is set by a verified Stripe webhook.';
