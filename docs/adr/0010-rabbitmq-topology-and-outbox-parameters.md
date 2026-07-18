# ADR-0010: RabbitMQ topology and outbox operational parameters

**Status:** proposed
**Date:** 2026-07-18
**Deciders:** morisempai (pending sign-off)
**Related:** ADR-0002 (fills in what it left open), ADR-0009, issue #8

## Context

ADR-0002 chose RabbitMQ with a transactional outbox and idempotent consumers, but deliberately
specified no numbers. A repo-wide search finds no retry counts, backoff intervals, DLQ names,
prefetch values, or relay poll interval anywhere. The only decided figures are in `docs/nfr.md`:
outbox rows pruned 7 days after the relay confirms publish, event→handled p95 of 60 s, and
outbound HTTP timeout 3 s.

These values cannot stay undecided. Five service agents implementing consumers independently
would each pick their own, and queue names in particular are wire-level: renaming a durable queue
in a running environment is an operational migration, not an edit.

## Decision

We will fix the following. They live in one ADR separate from ADR-0009 because they are tuning
values expected to change under load, whereas ADR-0009 records structure.

**Topology.** Topic exchange `booking.events` (durable), routing key = event name, both fixed by
the AsyncAPI contract. Queue per consumer per event: `<service>.<EventName>`, e.g.
`payment.BookingHeld` — rather than one queue per service — so a poison message on one event
cannot block another and prefetch is tunable per event type. Dead-letter exchange
`booking.events.dlx` (topic, durable); DLQ `<queue>.dlq`, reached by setting
`x-dead-letter-exchange` and an explicit `x-dead-letter-routing-key` on the main queue.

**Consumers.** Prefetch 20, except notification at 5 (it calls SMTP — slower, external, less
tolerant of parallelism). In-process retry 3 attempts with backoff 200 ms, 1 s, 5 s and **full
jitter**. After exhaustion, `nack(requeue=false)` to the DLQ. Malformed envelopes bypass retry
and dead-letter immediately.

**Relay.** Poll every 1 s, plus an in-process `Kick()` after commit to cut happy-path latency to
milliseconds. Batch 100, claimed with `FOR UPDATE SKIP LOCKED` so N replicas are safe. Publisher
confirms on; `published_at` is set only after the broker confirms. Outbox rows retry to 10
attempts, then `failed_at` is set, the row is skipped, and an alert fires.

**Retention.** Outbox pruned 7 days after publish (from `docs/nfr.md`). `processed_event` pruned
at 30 days.

## Consequences

- (+) Every number a service agent needs is decided in one place, so five consumers cannot drift
  into five retry policies.
- (+) Full jitter, rather than fixed or equal jitter, addresses the actual failure mode here: N
  consumer replicas retrying in lockstep against one recovering Postgres.
- (+) 3 attempts over ~6 s means anything reaching the DLQ is almost certainly a real bug rather
  than a transient blip, so the DLQ stays a signal instead of a dumping ground.
- (+) Marking `published_at` only after a broker confirm is what makes the RPO=0 claim true;
  reversing that ordering would lose events on a crash.
- (−) **Per-event queues multiply queue count**: 7 in the slice, and it grows as
  consumers×events. Every one needs monitoring, and the DLQ set doubles it.
- (−) **Cross-replica publish ordering is not guaranteed.** With N relay replicas, per-channel
  order holds but global order does not. Consumers must therefore be state-based rather than
  order-dependent. The saga already is, but this is a real constraint on future consumers, and a
  per-aggregate advisory lock in the relay is the documented escape hatch if one ever needs
  total per-aggregate ordering.
- (−) An in-process relay means publish stops when the service stops. Harmless — outbox rows are
  durable and the backlog drains on restart — but it does mean relay health is not independently
  observable from service health.
- (−) 30-day `processed_event` retention makes that table the largest in some services. It is
  bounded and indexed, but it is not free.
- (−) These numbers are picked from reasoning, not measurement. **No load test has been run.**
  They are starting points to be revised once real traffic exists, and should be read that way.

## Alternatives considered

- **One queue per service, all events** — rejected: a poison message on one event type blocks
  every other event for that service.
- **Relay as a separate deployable** — rejected for now: three more binaries, compose entries,
  and CI matrix rows, each a gated `.github/` change. The `Relay.Run(ctx)` shape keeps extraction
  cheap if scaling demands it later.
- **RabbitMQ TTL+DLX retry queues instead of in-process retry** — rejected for now: more moving
  topology for the same effect at this scale; swappable behind the consumer options later.
- **`LISTEN/NOTIFY` to wake the relay** — rejected: `Kick()` covers the in-process happy path
  without adding a second notification mechanism to reason about.
- **Infinite outbox retry** — rejected: one unpublishable row would wedge the relay behind it
  forever, converting a single bad event into total delivery failure.
