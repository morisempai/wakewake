-- Fixture for testkit's own integration test. NOT a service migration.
--
-- It reproduces the shape ADR-0003 and ADR-0011 specify, so that the claims in those ADRs are
-- executed rather than merely asserted. The availability service owns the real migration; this
-- exists so the harness and the reasoning are both proven before any service depends on them.

CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TYPE fixture_status AS ENUM ('held', 'confirmed', 'released');

CREATE TABLE fixture_reservation (
  id          uuid PRIMARY KEY,
  resource_id uuid NOT NULL,
  during      tstzrange NOT NULL,
  status      fixture_status NOT NULL DEFAULT 'held',

  CONSTRAINT fixture_window_not_empty CHECK (NOT isempty(during)),

  -- ADR-0011: partial on live states. A total constraint would let a released row block its
  -- window forever, so the TTL sweeper would free slots that could never be rebooked.
  CONSTRAINT fixture_no_overlap EXCLUDE USING gist (
    resource_id WITH =,
    during      WITH &&
  )
);
