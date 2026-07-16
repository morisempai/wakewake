# ADR-0003: No double-booking via Postgres exclusion constraint + hold/TTL saga

- **Status:** Proposed  <!-- agents propose; a human sets Accepted (adr-writing skill) -->
- **Date:** 2026-07-15
- **Deciders:** Architecture (pending human sign-off)
- **Related:** Business story ("several persons couldn't book the same place/equipment"); BOOK-2; ADR-0002

## Context

The core domain invariant is that no two bookings may hold the same resource over overlapping time.
Application-level checks ("SELECT then INSERT") race under concurrency and cannot guarantee the
invariant. The booking flow also spans multiple services (booking → payment) and must not leave a
slot locked forever if payment never completes.

## Decision

Enforce the invariant **in the database** in the Availability service using a Postgres **exclusion
constraint**:

```sql
CREATE EXTENSION IF NOT EXISTS btree_gist;
ALTER TABLE reservation
  ADD CONSTRAINT reservation_no_overlap
  EXCLUDE USING gist (resource_id WITH =, during WITH &&);
```

where `during` is a `tstzrange`. Overlapping inserts for the same resource are rejected by the DB,
not by app logic. The booking lifecycle is a **saga** orchestrated by the Booking service:
`HELD` (reservation inserted, with TTL) → pay → `CONFIRMED`; on payment failure/timeout the hold is
released (compensation) and the slot frees. Unpaid holds are swept/auto-released at TTL expiry.

## Consequences

- (+) Hard, race-proof guarantee independent of app concurrency.
- (+) TTL holds prevent inventory being locked by abandoned checkouts.
- (−) Requires `btree_gist` and careful range/timezone handling (all `tstzrange`, UTC).
- (−) Saga compensation + idempotency add complexity — covered by contract/unit tests including a
  concurrency test that fires two simultaneous requests for the same slot.

## Alternatives considered

- **App-level SELECT-then-INSERT check** — rejected: races under concurrency; cannot guarantee the invariant.
- **Distributed lock (Redis/advisory lock) per resource** — rejected: adds infra and a new failure mode; the DB constraint is simpler and authoritative.
- **Two-phase commit across services** — rejected: operationally heavy and poor availability; a saga with compensation fits the async model (ADR-0002).
