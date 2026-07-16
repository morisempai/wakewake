---
name: adr-writing
description: >
  How to write Architecture Decision Records and when one is required. Use this whenever
  making or proposing a significant technical choice — picking a library/technology/pattern,
  changing a contract, altering service boundaries or data ownership, adding infrastructure,
  deviating from the service template, or anything a future maintainer would ask "why is it
  like this?" about. Also use when the user asks to document, justify, or review a design
  decision, even if they don't say "ADR".
---

# Architecture Decision Records

## When an ADR is REQUIRED (not optional)

- Any change to `contracts/` (new version, new event, deprecation)
- New technology/major dependency (message pattern, database, framework, external SaaS)
- Change to service boundaries, responsibilities, or data ownership
- Deviation from service-template or testing-standards
- Destructive migration; security/auth model changes; any snowflake CI behavior

If in doubt, write one — a two-paragraph ADR is cheap; archaeology later is not.
Agents don't merge ADRs: they propose them (status `proposed`) in the PR or a dedicated
issue; a human sets `accepted`.

## Location & naming

`docs/adr/NNNN-short-slug.md`, sequential, never renumbered. Superseding: new ADR links
old one; old one gets status `superseded by ADR-NNNN` — never edit past decisions.

## Format

```markdown
# ADR-0007: Transactional outbox for event publishing

**Status:** proposed | accepted | superseded by ADR-XXXX
**Date:** 2026-07-15
**Deciders:** <human name(s)>
**Related:** story #42, ADR-0003

## Context
2–5 sentences. The forces at play: the problem, constraints (NFRs, team, cost),
and why "do nothing" is inadequate. No solution talk here.

## Decision
One paragraph, active voice: "We will publish events by writing to an outbox table
in the same transaction as the state change; a relay process forwards to RabbitMQ."

## Consequences
Honest, both directions:
- + At-least-once delivery without dual-write inconsistency
- + Works with plain Postgres, no new infra
- − Relay adds latency (measured budget: ≤2s) and one more component to monitor
- − Consumers must be idempotent (already required by api-contracts skill)

## Alternatives considered
For each rejected option, ONE sentence of what and ONE of why not.
"CDC via Debezium — rejected: operational complexity unjustified at current scale."
```

## Quality bar

- Consequences must contain at least one real drawback. An ADR with only upsides is
  advertising, not a record — it will be rejected.
- Context states constraints as facts with sources (NFR doc, story, measurement),
  not vibes ("it's more scalable").
- Keep it under one page. Link details; don't inline designs.
