# Service: availability

> Inherits all root rules in `/CLAUDE.md`. This file scopes them to this service.

## Write scope
Only files under `services/availability/`. Contract/shared/other-service changes → GitHub issue + stop.

## Responsibility
The anti-double-booking engine. Owns the resource calendar and reservations. Reserves and releases
time windows for resources with a hard, race-proof guarantee (ADR-0003).

## Owns
- Database: `availability` (exclusive). Key table `reservation` with the exclusion constraint:
  `EXCLUDE USING gist (resource_id WITH =, during WITH &&)` over a `tstzrange` (requires `btree_gist`).

## Implements (contracts — source of truth)
- HTTP: `contracts/openapi/availability.yaml` (query slots, reserve, release, confirm)
- Events published: `ReservationCreated`, `ReservationReleased`
  (schemas in `contracts/asyncapi/booking-events.yaml`)

## Consumes
- `BookingCancelled` → release the reservation (compensation path).

## Non-negotiable
- The overlap guarantee is enforced BY THE DATABASE, never by app-level SELECT-then-INSERT.
- A concurrency test (two simultaneous reserves of the same slot → exactly one wins) is required.
- All ranges `tstzrange` in UTC.
- Consumers are idempotent keyed on the envelope `id` and ignore unknown payload fields.
