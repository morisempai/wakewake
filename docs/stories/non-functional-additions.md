# Non-functional additions

Cross-cutting requirements missing from the originals. Several are load-bearing for the slice.

## NFR-1 — Idempotency [SLICE]
Payment and booking write endpoints accept an idempotency key; retries never double-charge or double-book.

## NFR-2 — Transactional outbox [SLICE]
Producers persist state + event in one transaction; a relay publishes to RabbitMQ (ADR-0002). Supports zero-RPO.

## NFR-3 — Correlation IDs & structured logs [SLICE]
Every request/event carries a correlation id, stamped on all logs and spans (root CLAUDE.md DoD).

## NFR-4 — GDPR / data privacy [DEFERRED]
PII inventory, right-to-be-forgotten, data retention, per-service data minimization.

## NFR-5 — PCI-DSS [DEFERRED → design constraint now]
Never store raw card data; delegate to the payment provider (Stripe). Slice already honors this.

## NFR-6 — RTO (recovery time) [DEFERRED]
Stories specify RPO=0 but not RTO. Define target recovery time + backup/restore + DR runbooks.

## NFR-7 — Edge protection [DEFERRED]
Rate limiting, WAF, and bot protection at the gateway (ADR-0006).

## NFR-8 — Contract & event versioning governance [SLICE-adjacent]
Backward-compatible evolution rules for OpenAPI/AsyncAPI; CI checks for breaking changes.

## NFR-9 — i18n & a11y [DEFERRED]
Localization and accessibility for the public customer site.

## NFR-10 — Feature flags [DEFERRED]
Runtime toggles for safe rollout, decoupled from deploys.
