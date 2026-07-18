# ADR-0009: shared/ module boundary and contract code generation

**Status:** proposed
**Date:** 2026-07-18
**Deciders:** morisempai (pending sign-off)
**Related:** ADR-0002, ADR-0007, ADR-0008, issue #8

## Context

Five services are implemented by five separate agents, each confined to its own
`services/<name>/` directory by hard rule #1. `shared/` did not exist. Several shapes are
genuinely cross-service: the event envelope in `contracts/asyncapi/booking-events.yaml` has its
producer in one service and its consumer in another, so no single service can own it.

If those shapes are left to the service agents, five independent definitions appear. Divergence
in an envelope field name or a timestamp's meaning is invisible in each service's own tests —
every suite passes — and fails only in production, across the bus. ADR-0002 already assumed a
resolution, stating the outbox would be "provided as a shared helper in `shared/platform`".

The opposing force is real: `shared/` is CODEOWNERS-gated, so everything placed in it becomes a
`shared-change` issue for any service agent who later needs it adjusted. Over-sharing converts
routine service work into cross-team coordination.

## Decision

We will create `shared/` as **three modules** — `shared/contracts`, `shared/platform`,
`shared/testkit` — and admit code to them only when **both** of these hold:

1. **Divergence would be a silent contract violation, not a visible bug.** Five envelope
   marshalers pass their own tests and break the bus; five HTTP routers are style variance.
2. **Its change trigger is already gated.** Generated OpenAPI code regenerates only when
   `contracts/` changes, which is already a gated PR, so sharing it adds no new bottleneck. A
   cursor codec changes whenever a service changes its sort key, so sharing it would convert
   routine work into a `shared-change` issue.

Three modules rather than one because notification has no HTTP API, catalog publishes no events,
and nothing needs testcontainers at runtime; one module would put the Docker client dependency
tree into every service's `go.sum`.

We adopt one further rule, enforced by CI: **`internal/domain` may import `shared/contracts` but
never `shared/platform`.** `contracts` is pure types; `platform` carries `pgx`, `amqp`, and
`otel`, and importing it from a domain layer is the layering violation `service-template`
forbids.

OpenAPI types are **generated** with `oapi-codegen`, committed, and guarded by a
`git diff --exit-code` check. AsyncAPI payload types are **hand-written** and guarded by contract
tests that compare each struct's json tags against the spec's properties in both directions.

## Consequences

- (+) The envelope, the outbox, and consumer dedupe have exactly one definition, and the
  subtleties in them (transaction-time `occurred_at`, dedupe inside the handler's transaction)
  are decided once instead of five times.
- (+) Test 2 kept genuinely service-owned things out: error-code enums (each spec has a
  different enum — a shared union would let booking return `reservation_overlap` and compile),
  cursor pagination, config loading, and SQL migrations.
- (+) Splitting into three modules isolates dependency blast radius: a `testcontainers` bump
  cannot churn five services' `go.sum` files.
- (−) **Every change to `shared/` is a gated PR.** A service agent blocked on a missing helper
  must open a `shared-change` issue and stop. That is friction by design, but it is friction.
- (−) **Hand-written AsyncAPI structs deviate from the `api-contracts` skill**, which says never
  hand-write a struct that can drift. The skill's stated reason is that "a hand-written struct
  only proves the code agrees with itself" — the drift tests refute that specific objection, but
  the deviation is real and rests on those tests continuing to exist and stay strict.
- (−) Committed generated code means `make contracts-gen` must be run and the diff reviewed on
  every contract change; forgetting turns into a confusing CI failure rather than a clear one.
- (−) Three modules mean three `go.mod` files and `replace` directives in every consumer. A
  new service must be wired up correctly or it silently resolves against a published path that
  does not exist.

## Alternatives considered

- **No `shared/`; each service owns everything** — rejected: the envelope has no owning service,
  and five definitions differ in ways only production reveals.
- **One `shared` module** — rejected: puts testcontainers and the Docker client tree in every
  service's dependency graph, so unrelated bumps churn five services at once.
- **Generate AsyncAPI types with Modelina** — rejected: it requires a second Node toolchain step
  and flattens the `allOf: [Envelope, {payload}]` composition, merging payload fields into the
  envelope, which is precisely the shape this contract must not have.
- **Publish `shared/` as tagged modules** — rejected: pre-1.0 version ceremony for code that only
  ever ships inside this repo; `replace` directives cost nothing and always resolve.
