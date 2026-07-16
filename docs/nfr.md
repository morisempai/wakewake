# Project-wide non-functional defaults

Every story's **NFR notes** section either states "defaults apply" (meaning this file) or overrides a
specific value here with a justification. The user-stories skill makes that section mandatory.

These are the *defaults*; per-story overrides are normal and expected — the point is that the number
is written down somewhere, not that every story restates it.

> Values marked <!-- ASSUMED --> were derived from the user stories but never explicitly confirmed.
> Treat them as proposals until a human signs off (same posture as ADR status).

## Availability

- **Customer-facing paths** (catalog browse, availability query, booking hold, payment): **99.9%**
  monthly — from the Reliability story. Error budget: ~43 min/month.
- **Internal/admin paths** (ops console, dashboards): 99.5% <!-- ASSUMED -->
- **Async consumers** (notification): no availability SLO; bounded by the delivery-latency target
  below. A consumer being down must never lose an event (durable queues, ADR-0002).

## Latency

Measured at the gateway, p95 unless stated. <!-- ASSUMED — no latency numbers appear in the stories -->

| Path | p95 | p99 |
|---|---|---|
| Catalog read | 200 ms | 500 ms |
| Availability query | 300 ms | 800 ms |
| Booking hold (write) | 500 ms | 1 s |
| Payment intent (write, Stripe upstream) | 1 s | 3 s |
| Event → consumer handled (e.g. `BookingConfirmed` → email sent) | 60 s | — |

Outbound HTTP calls between services default to a **3 s timeout**, retries with jitter only on
idempotent calls (api-contracts skill).

## Durability & recovery

- **RPO = 0** for booking and payment state — the stories require it. This is why every producer
  writes state and event in one transaction (transactional outbox, ADR-0002, NFR-2).
- **RTO** is undefined and deliberately open (NFR-6). It needs a human decision before any DR
  runbook is written; do not invent one in a story.

## Retention

<!-- ASSUMED — no retention numbers appear in the stories; these are starting points, not policy -->

| Data | Default retention |
|---|---|
| Structured logs (Loki) | 30 days |
| Traces (Tempo) | 7 days |
| Metrics (Prometheus) | 90 days |
| Outbox rows (after relay confirms publish) | 7 days, then pruned |
| Audit events (dedicated audit account, ADR-0005) | indefinite until a legal/GDPR policy exists |
| Booking/payment business records | indefinite pending the GDPR work in NFR-4 |

Retention interacts with GDPR right-to-be-forgotten (NFR-4, deferred). Any story touching PII should
say so explicitly rather than assuming the table above settles it.

## Cross-cutting requirements that apply to every story

These are not negotiable per-story; they are the Definition of Done in the root `CLAUDE.md`:

- **Correlation ID** on every request, event, log line, and span (NFR-3).
- **Idempotency**: HTTP writes take an idempotency key; event consumers dedupe on the envelope `id`
  (at-least-once delivery, NFR-1).
- **No PII or tokens in logs.**
- **Failure-path AC**: every story states what happens when a dependency is unavailable.

## Security & compliance posture

- **PCI**: raw card data is never stored or logged — Stripe holds it, we hold tokens/ids/status
  (NFR-5). This is a hard design constraint today, not deferred work.
- **Secrets** come from Vault; a secret in a workflow file, log, or compose file is an incident.
- **Ingress**: only through the gateway (ADR-0006); services are not publicly routable.
