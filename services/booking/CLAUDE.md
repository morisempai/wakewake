# Service: booking

> Inherits all root rules in `/CLAUDE.md`. This file scopes them to this service.

## Write scope
Only files under `services/booking/`. Contract/shared/other-service changes → GitHub issue + stop.

## Responsibility
The booking aggregate and lifecycle (`HELD → CONFIRMED → CANCELLED`). Orchestrates the
hold→pay→confirm **saga** with compensation. Zero-RPO target — uses the transactional outbox.

## Owns
- Database: `booking` (exclusive). Tables: `booking`, `outbox`, `idempotency_key`.

## Implements (contracts — source of truth)
- HTTP: `contracts/openapi/booking.yaml` (create hold, get booking, cancel)
- Events published: `BookingHeld`, `BookingConfirmed`, `BookingCancelled`
  (schemas in `contracts/asyncapi/booking-events.yaml`)

## Consumes
- `PaymentSucceeded` → confirm booking + emit `BookingConfirmed`
- `PaymentFailed` → cancel booking + emit `BookingCancelled` (compensation)
- `ReservationReleased` (TTL sweep) → cancel expired holds

## Non-negotiable
- Outbox write is in the SAME transaction as the state change.
- Create-hold is idempotent (idempotency key). Hold TTL from `BOOKING_HOLD_TTL_SECONDS`.
- Consumers are idempotent keyed on the envelope `id` and ignore unknown payload fields.
