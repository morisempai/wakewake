# Service: payment

> Inherits all root rules in `/CLAUDE.md`. This file scopes them to this service.

## Write scope
Only files under `services/payment/`. Contract/shared/other-service changes → GitHub issue + stop.

## Responsibility
Take payment for a booking via Stripe (test mode in dev). Idempotent. Emits payment outcome events.

## Owns
- Database: `payment` (exclusive). Tables: `payment`, `outbox`, `idempotency_key`.

## Implements (contracts — source of truth)
- HTTP: `contracts/openapi/payment.yaml` (create payment intent, Stripe webhook)
- Events published: `PaymentSucceeded`, `PaymentFailed`
  (schemas in `contracts/asyncapi/booking-events.yaml`)

## Consumes
- `BookingHeld` → (optionally) create a pending payment context for the hold.

## Non-negotiable — PCI
- NEVER store raw card data. Card handling is delegated to Stripe; we store only tokens/ids/status.
- Verify Stripe webhook signatures. All charge operations carry an idempotency key.
- Split-bill is DEFERRED (see docs/stories/deferred-backlog.md).
