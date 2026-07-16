# ADR-0001: Microservices in a monorepo, contract-first

- **Status:** Proposed  <!-- agents propose; a human sets Accepted (adr-writing skill) -->
- **Date:** 2026-07-15
- **Deciders:** Architecture (pending human sign-off)
- **Related:** Reliability stories (async comms), Development (run locally), Security (gateway-only access); ADR-0006

## Context

The platform spans several distinct bounded contexts (catalog, availability, booking, payment,
notifications, identity, admin) with different scaling, ownership, and reliability needs. Stories
require independent deployability, async inter-service comms, per-service observability, and strict
data ownership. We also need one coherent developer experience and a single source of truth for the
shapes that cross service boundaries.

Options: (a) modular monolith; (b) polyrepo microservices; (c) monorepo microservices.

## Decision

Build **microservices in a single monorepo**, managed with **pnpm workspaces + Nx**. All
inter-service communication shapes are defined **contract-first** in `contracts/` (OpenAPI for HTTP,
AsyncAPI for events) and are the single source of truth. Code implements contracts; it never defines
them. Each service owns its database exclusively.

## Consequences

- (+) Independent deployability and failure isolation; per-service scaling and SLOs.
- (+) Atomic cross-cutting changes (contract + consumers) in one PR; Nx affected-graph for CI.
- (+) Contracts prevent drift and enable generated types + contract tests.
- (−) Operational overhead (many services, network boundaries, distributed debugging) — mitigated by
  the observability stack (ADR-0007) and correlation IDs.
- (−) Requires discipline on write-scope and data ownership (enforced by CODEOWNERS + service CLAUDE.md).

## Alternatives considered

- **Modular monolith** — rejected: does not meet independent-deploy, per-service SLO, and failure-isolation stories.
- **Polyrepo microservices** — rejected: cross-cutting contract changes can't be made atomically; heavier tooling to keep specs and consumers in sync.
