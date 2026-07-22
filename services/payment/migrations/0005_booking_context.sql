-- 0005_booking_context.sql — the payment context built from BookingHeld.
--
-- payment.yaml is emphatic that createPayment reads the amount FROM THE BOOKING, never from the
-- request. Payment owns its own database and must not read booking's tables (hard rule #6), and
-- there is no service-to-service authenticated read path in this slice. So payment learns the
-- authoritative amount the contract-sanctioned way: it consumes BookingHeld ("Payment prepares a
-- payment context for the hold", booking-events.yaml) and records exactly the fields it will need
-- when the customer later calls createPayment.
--
-- This is the "pending payment context" the issue's saga role describes. It is NOT card data and it
-- is NOT another service's table — it is payment's own durable projection of a fact it was told.

CREATE TABLE booking_context (
  -- The held booking. One context per booking; a duplicate BookingHeld (at-least-once delivery) is
  -- an ON CONFLICT DO NOTHING no-op in the consumer.
  booking_id      uuid PRIMARY KEY,

  -- Kept so a future authorization check (does this payment belong to the caller?) has the owner to
  -- hand. Not used to set the price.
  customer_id     uuid NOT NULL,

  -- The authoritative amount and currency to charge, copied from the BookingHeld payload.
  amount_minor    bigint NOT NULL CHECK (amount_minor >= 0),
  currency        text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),

  -- After this instant the hold is swept and the booking is no longer payable (422
  -- booking_not_payable). This is the only payability signal payment has locally — see the README's
  -- flagged limitation about confirmed/cancelled bookings.
  hold_expires_at timestamptz NOT NULL,

  created_at      timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE booking_context IS
  'Payment''s own projection of BookingHeld: the authoritative amount/currency and hold expiry for '
  'a held booking, so createPayment reads the price from the booking rather than the request.';
