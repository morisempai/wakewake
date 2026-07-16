# ADR-0002: Async messaging via RabbitMQ with the transactional outbox pattern

- **Status:** Proposed  <!-- agents propose; a human sets Accepted (adr-writing skill) -->
- **Date:** 2026-07-15
- **Deciders:** Architecture, SRE (pending human sign-off)
- **Related:** Reliability stories (async comms, zero RPO for completed transactions); ADR-0003

## Context

Stories mandate async-only inter-service communication and **zero RPO for all completed user
transactions**. Naive "write DB then publish event" risks losing events if the process crashes
between the two, or double-processing on retries. We need exactly-once *effects* across the
booking/payment flow.

## Decision

Use **RabbitMQ** as the event backbone. Every producer writes domain state and an **outbox** row in
the **same local transaction**; a relay publishes outbox rows to RabbitMQ and marks them sent.
Consumers are **idempotent** (dedupe by message id / idempotency key) and use manual acks. Event
shapes are governed by AsyncAPI in `contracts/`.

## Consequences

- (+) No lost events even on crash → satisfies zero-RPO for completed transactions.
- (+) Decoupled services; natural retry/backoff and dead-lettering.
- (−) At-least-once delivery means consumers MUST be idempotent (enforced in code + tests).
- (−) Extra outbox table + relay per producing service — provided as a shared helper in `shared/platform`.

## Alternatives considered

- **Kafka** — rejected (for now): throughput doesn't justify the operational weight; RabbitMQ is simpler to self-host.
- **Dual-write (DB then publish, no outbox)** — rejected: loses events on crash between the two writes, violating the zero-RPO story.
- **CDC via Debezium** — rejected: operational complexity unjustified at current scale.
