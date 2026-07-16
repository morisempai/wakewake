---
name: testing-standards
description: >
  How tests are written, named, and structured in this project: unit tests, contract tests,
  integration tests, coverage expectations, and what must pass before a PR. Use this whenever
  writing or modifying ANY test file, doing TDD for a story, setting up test infrastructure,
  deciding what kind of test a behavior needs, fixing a failing CI test job, or reviewing
  test quality in a PR. Also consult it BEFORE writing implementation code, since TDD ordering
  is defined here.
---

# Testing Standards

<!-- EDIT: assumes Vitest + Pact + Testcontainers. Swap tool names for your stack. -->

## The pyramid and what goes where

1. **Unit** (`tests/unit/`) — domain logic, mapping, validation. No network, no DB, no broker;
   infra dependencies replaced by in-memory fakes implementing the repository interfaces.
   Fast (<10s per service suite). This is where most tests live.
2. **Contract** (`tests/contract/`) —
   - *Provider side:* every endpoint this service exposes is verified against
     `contracts/openapi/<service>.yaml` (responses validate against schema).
   - *Consumer side:* every external call/event consumed has a Pact (or schema) test pinning
     exactly the fields we rely on.
3. **Integration** (`tests/integration/`, optional per story) — service + real Postgres/RabbitMQ
   via Testcontainers, for transactional behavior (outbox, idempotency, migrations).

Rule of thumb: business rule → unit; "do we agree with another service" → contract;
"does the plumbing work" → integration. Never test business logic through HTTP when a
unit test can reach it directly.

## TDD workflow (mandatory, see Hard rule 4)

1. Pick one acceptance criterion.
2. Write a failing test named after it: `it("sends one email when deal moves stage (#42 AC1)")`.
3. Commit the red test (`test(notifications): failing AC1 #42`).
4. Implement minimally to green. 5. Refactor. 6. Next AC.

## Conventions

- Test names describe behavior, not methods: ✅ `rejects merge when deals belong to
  different tenants` ❌ `test mergeDeal 2`.
- AAA structure (arrange/act/assert), one behavior per test, no logic (loops/ifs) inside tests.
- Builders/factories for test data (`tests/factories.ts`) — no 30-line inline object literals.
- Determinism: fake clock, seeded ids. Zero tolerance for time/order-dependent flakes.
- Every consumer test includes: duplicate event delivery (idempotency), unknown extra
  fields (forward compat), and dependency-down (retry/DLQ) cases.
- NEVER weaken an assertion or delete a test to make CI pass. If a test seems wrong,
  comment on the issue/PR explaining why; changing test intent requires human approval.
- `test.skip` requires a linked issue in a comment on the same line.

## Coverage

- Domain layer: ≥90% branches. Service overall: ≥80% lines. <!-- EDIT thresholds -->
- Coverage is a floor, not a goal — a PR full of assertion-free tests to hit numbers
  will be rejected in review.

## Pre-PR gate (run locally before `gh pr create`)

```bash
npm run lint && npm test && npm run test:contract
```

All green or the PR doesn't get opened. CI re-runs everything plus integration and
cross-service contract verification.
