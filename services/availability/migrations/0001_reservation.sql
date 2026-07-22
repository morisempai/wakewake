-- 0001_reservation.sql — the anti-double-booking core (ADR-0003, amended by ADR-0011).
--
-- The invariant "no two live reservations hold the same resource over overlapping time" is
-- enforced HERE, by the database, and nowhere else. There is deliberately no application-level
-- SELECT-then-INSERT anywhere in the reserve path: that pattern races and cannot guarantee the
-- invariant, which is the entire reason this constraint exists.

-- Required for the exclusion constraint: gist must index `resource_id` (an equality-compared
-- scalar) alongside `during` (an overlap-compared range) in the same index.
CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TYPE reservation_status AS ENUM ('held', 'confirmed', 'released');
CREATE TYPE release_reason AS ENUM ('payment_failed', 'booking_cancelled', 'hold_expired');

CREATE TABLE reservation (
  id              uuid PRIMARY KEY,
  resource_id     uuid NOT NULL,
  booking_id      uuid NOT NULL,

  -- Half-open [starts_at, ends_at) in UTC. Half-open is what makes back-to-back bookings
  -- legal: 10:00–11:00 and 11:00–12:00 do not overlap, because `&&` on '[)' ranges excludes
  -- the shared endpoint. A closed range would reject them and silently lose revenue.
  during          tstzrange NOT NULL,

  status          reservation_status NOT NULL DEFAULT 'held',
  expires_at      timestamptz,
  released_reason release_reason,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),

  -- ends_at must be strictly after starts_at. Postgres normalises an inverted range into an
  -- error and an equal-bounds '[)' range into `empty`, which would overlap nothing and slip
  -- past the exclusion constraint entirely. Rejecting empty here closes that hole.
  CONSTRAINT reservation_window_not_empty CHECK (NOT isempty(during)),
  CONSTRAINT reservation_window_bounded   CHECK (lower(during) IS NOT NULL AND upper(during) IS NOT NULL),

  -- The TTL sweeper finds work by `status = 'held' AND expires_at <= now()`. A held row with a
  -- NULL expires_at would be invisible to it and hold the slot forever; a confirmed row with a
  -- non-NULL expires_at would be swept away after it was paid for. Both are barred.
  CONSTRAINT reservation_held_iff_expiry   CHECK ((status = 'held') = (expires_at IS NOT NULL)),
  CONSTRAINT reservation_released_iff_reason CHECK ((status = 'released') = (released_reason IS NOT NULL)),

  -- THE INVARIANT.
  --
  -- Partial on `status <> 'released'` (ADR-0011). ADR-0003 specified this constraint over the
  -- whole table; that is wrong. Released rows are kept as history, so a total constraint would
  -- let one expired hold poison its window permanently — the TTL sweeper would free a slot that
  -- could then never be re-booked, turning the abandoned-checkout protection into a slow leak
  -- of inventory. `held` and `confirmed` are the live states and block; `released` is terminal
  -- history and does not.
  CONSTRAINT reservation_no_overlap EXCLUDE USING gist (
    resource_id WITH =,
    during      WITH &&
  ) WHERE (status <> 'released')
);

-- The sweeper's query. Partial: only held rows are ever due, and they are a small minority of
-- the table once the system has any history.
CREATE INDEX reservation_due_for_sweep_idx
  ON reservation (expires_at)
  WHERE status = 'held';

CREATE INDEX reservation_booking_id_idx ON reservation (booking_id);

COMMENT ON CONSTRAINT reservation_no_overlap ON reservation IS
  'ADR-0003/ADR-0011: the no-double-booking invariant. Violation surfaces as SQLSTATE 23P01 and '
  'is mapped to the contract-level 409 reservation_overlap. Never bypass with an app-level check.';
