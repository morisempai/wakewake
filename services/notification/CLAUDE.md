# Service: notification

> Inherits all root rules in `/CLAUDE.md`. This file scopes them to this service.

## Write scope
Only files under `services/notification/`. Contract/shared/other-service changes → GitHub issue + stop.

## Responsibility
Send transactional notifications. In the slice: booking confirmation email (Mailhog in dev).

## Owns
- Database: `notification` (exclusive). Table: `sent_notification` (dedupe/idempotency + audit).

## Implements (contracts — source of truth)
- Events consumed: `BookingConfirmed` (`contracts/asyncapi/booking-events.yaml`) → send confirmation email.
- No public HTTP endpoints in the slice.

## Non-negotiable
- Consumers are idempotent (dedupe by the envelope `id`) — at-least-once delivery (ADR-0002).
- Never log PII or tokens; recipient addresses are redacted in structured logs.
- Marketing/campaign notifications are DEFERRED; this service is transactional only for now.
