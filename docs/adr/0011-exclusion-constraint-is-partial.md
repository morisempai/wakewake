# ADR-0011: the no-overlap exclusion constraint is partial on live reservations

**Status:** proposed
**Date:** 2026-07-18
**Deciders:** morisempai (pending sign-off)
**Related:** amends ADR-0003; BOOK-1, BOOK-2; issue #5

## Context

ADR-0003 specifies the no-double-booking invariant as an exclusion constraint over the whole
`reservation` table:

```sql
ALTER TABLE reservation
  ADD CONSTRAINT reservation_no_overlap
  EXCLUDE USING gist (resource_id WITH =, during WITH &&);
```

That is wrong, and wrong in a direction that only appears after the system has been running.

Released reservations are retained as history — the table is the audit trail of what was held and
why it ended. A constraint over the whole table therefore counts a released row as blocking. So
when the TTL sweeper releases an abandoned hold (BOOK-1), the row stays, and its window remains
permanently unbookable. The abandoned-checkout protection that BOOK-1 exists to provide becomes a
slow leak of inventory: every unpaid hold silently destroys that slot forever.

This surfaces in no unit test, because it requires a release followed by a second booking attempt
on the same window. It would surface in production as "customers say the calendar is full but we
have no bookings."

A second, related defect: an equal-bounds `[)` range normalises to `empty` in Postgres, and an
empty range overlaps nothing. A request where `starts_at == ends_at` would slip past the
exclusion constraint entirely.

## Decision

We will make the constraint **partial**, excluding terminal rows, and add CHECK constraints that
close the empty-range hole and keep the TTL sweeper's view consistent:

```sql
CONSTRAINT reservation_no_overlap EXCLUDE USING gist (
  resource_id WITH =,
  during      WITH &&
) WHERE (status <> 'released')
```

`held` and `confirmed` are the live states and block; `released` is terminal history and does
not. Alongside it: `CHECK (NOT isempty(during))`, and
`CHECK ((status = 'held') = (expires_at IS NOT NULL))` so that a held row is always visible to
the sweeper and a confirmed row can never be swept after it was paid for.

The `during` column stays a `tstzrange` in UTC with **half-open `[)` bounds**, so back-to-back
bookings (10:00–11:00 and 11:00–12:00) do not overlap.

## Consequences

- (+) Releasing a hold genuinely frees the slot, which is what BOOK-1's acceptance criterion
  requires and what ADR-0003 as written would have prevented.
- (+) History is retained without cost to availability — the audit trail and the invariant stop
  fighting each other.
- (+) The empty-range and NULL-`expires_at` holes are closed in the schema rather than in
  application code, so they hold regardless of which service or migration touches the table.
- (−) A partial exclusion constraint uses a partial gist index, so the planner will not use it
  for queries that do not include the same predicate. The slot-listing query must be written with
  `status <> 'released'` to benefit.
- (−) The invariant is now expressed in two places — the constraint and the status enum — so a
  future status value must be classified as live or terminal deliberately. Adding one without
  thinking makes it silently non-blocking.
- (−) This ADR amends a `proposed` ADR that was never signed off, so the record now requires
  reading both 0003 and 0011 to know what is actually built.

## Alternatives considered

- **Delete released rows instead** — rejected: destroys the audit trail, and a booking dispute
  needs to show what was held and when it was released.
- **Move released rows to a history table** — rejected as premature: two tables and a move on
  every release, to avoid a `WHERE` clause. Reconsider if the table grows enough for the
  retained rows to hurt.
- **Enforce in application code with a `SELECT` before `INSERT`** — rejected for the same reason
  ADR-0003 rejected it: it races, and the race is exactly the bug the whole design exists to
  prevent.
