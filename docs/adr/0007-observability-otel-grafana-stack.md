# ADR-0007: Observability via OpenTelemetry → Prometheus/Tempo/Loki + Grafana

- **Status:** Proposed  <!-- agents propose; a human sets Accepted (adr-writing skill) -->
- **Date:** 2026-07-15
- **Deciders:** SRE, Development (pending human sign-off)
- **Related:** Operation stories (monitoring dashboards, umbrella status), Reliability (SLA/SLO dashboards + alerts), Development (inspect traces/runtime info)

## Context

Stories require per-service metrics/errors/traces, an umbrella "all services" status, SLA/SLO
dashboards with near-limit alerting, and developer access to traces locally. We want vendor-neutral
instrumentation and a self-hostable stack (consistent with the isolation/SSO stories).

## Decision

Instrument every service with the **OpenTelemetry SDK** (traces, metrics, logs) exporting OTLP to an
**OTel Collector**, which fans out to **Prometheus** (metrics), **Tempo** (traces), and **Loki**
(logs), all visualized in **Grafana**. A **correlation ID** is generated at the gateway and
propagated through HTTP headers and event metadata, and stamped on every structured log line and span
(shared middleware in `shared/platform`). Grafana/OpenSearch UIs sit behind Keycloak SSO in real envs.

## Consequences

- (+) One trace follows a booking across gateway→booking→availability→payment→notification.
- (+) Vendor-neutral; self-hosted; per-service + umbrella dashboards; SLO alerting.
- (−) Instrumentation and collector are operational surface area to maintain.
- (−) Trace/log volume needs sampling and retention policy (defined with SLOs in M4).

## Alternatives considered

- **Datadog/New Relic (SaaS)** — rejected: recurring cost and external data hosting conflict with the self-isolation stories.
- **ELK/OpenSearch for everything incl. metrics** — partially adopted: OpenSearch is an option for logs, but Prometheus/Tempo are better fits for metrics/traces; kept the best-of-breed split.
- **Vendor SDKs instead of OpenTelemetry** — rejected: OTel keeps instrumentation vendor-neutral and portable across the backends above.
