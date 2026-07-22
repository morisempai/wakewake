-- 0001_booking.sql — the booking aggregate (ADR-0003 saga).
--
-- Booking is the ONLY service that decides a booking's state (contracts/openapi/booking.yaml).
-- It reserves a window through Availability and reacts to PaymentSucceeded / PaymentFailed /
-- ReservationReleased; it never writes another service's database (hard rule #6). Every state
-- change is written in the SAME transaction as its outbox row (ADR-0002), which is what makes the
-- RPO=0 target achievable.

CREATE TYPE booking_status AS ENUM ('held', 'confirmed', 'cancelled');

-- Why a booking was cancelled, matching BookingCancelled.reason in the AsyncAPI contract. Stored
-- so the terminal state carries its own explanation and the audit trail is self-describing.
CREATE TYPE booking_cancel_reason AS ENUM
  ('payment_failed', 'hold_expired', 'customer_cancelled', 'operator_cancelled');

CREATE TABLE booking (
  id              uuid PRIMARY KEY,

  -- From the JWT `sub` claim, never client-supplied (booking.yaml). The list endpoint scopes to
  -- it and get/cancel authorise against it, so it is indexed below.
  customer_id     uuid NOT NULL,

  -- The catalog product the customer asked for. resource_id is what Availability actually holds;
  -- catalog owns the product -> resource mapping (hard rule #6), so booking resolves it once at
  -- hold time and stores it, rather than re-querying catalog on every state change.
  product_id      uuid NOT NULL,
  resource_id     uuid NOT NULL,

  -- The reservation Availability created for this booking. Nullable because the contract types it
  -- `oneOf: [Uuid, null]` ("Null once released"); kept populated here so compensation and the
  -- BookingCancelled payload can still name the reservation Availability must free.
  reservation_id  uuid,

  -- Half-open [starts_at, ends_at) in UTC, mirroring Availability's `during` range.
  starts_at       timestamptz NOT NULL,
  ends_at         timestamptz NOT NULL,

  party_size      integer NOT NULL CHECK (party_size >= 1),

  status          booking_status NOT NULL DEFAULT 'held',

  -- When an unpaid `held` booking is auto-cancelled and its slot freed. Taken from the
  -- reservation Availability created (its DB clock), so the two agree on the deadline. Null once
  -- confirmed or cancelled — the biconditional below enforces that, so a confirmed booking can
  -- never carry a stale expiry and a held one can never lack it.
  hold_expires_at timestamptz,

  -- Authoritative amount to charge, in the currency's minor unit. Integer, never a float: this is
  -- a payments system and 0.1 + 0.2 != 0.3 in binary floating point.
  total_minor     bigint NOT NULL CHECK (total_minor >= 0),
  currency        text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),

  cancel_reason   booking_cancel_reason,
  cancellation_reason text CHECK (cancellation_reason IS NULL OR length(cancellation_reason) <= 500),

  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),

  -- ends_at strictly after starts_at — the same rule Availability enforces on `during`.
  CONSTRAINT booking_window_bounded CHECK (ends_at > starts_at),

  -- hold_expires_at is set exactly while the booking is held. A held row without an expiry would
  -- misrepresent when its slot frees; a confirmed/cancelled row with one would claim a deadline
  -- that no longer applies.
  CONSTRAINT booking_held_iff_expiry CHECK ((status = 'held') = (hold_expires_at IS NOT NULL)),

  -- cancel_reason is set exactly when cancelled, so the terminal state always explains itself and
  -- a non-terminal row never carries a spurious reason.
  CONSTRAINT booking_cancelled_iff_reason CHECK ((status = 'cancelled') = (cancel_reason IS NOT NULL))
);

-- listBookings pages the caller's own bookings newest-first, optionally filtered by status. The
-- (customer_id, created_at DESC, id) shape serves both the scope and the keyset cursor.
CREATE INDEX booking_customer_created_idx ON booking (customer_id, created_at DESC, id DESC);

COMMENT ON TABLE booking IS
  'The booking aggregate (ADR-0003). Booking alone decides a booking''s state; every transition '
  'is written in the same transaction as its outbox row (ADR-0002).';
