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

Go (ADR-0008): stdlib `testing`, `testcontainers-go` for integration, `kin-openapi` to validate
responses against the specs. Assertion helpers (`testify`) are allowed but never required —
a plain `if got != want { t.Errorf(...) }` is idiomatic and fine.

## The pyramid and what goes where

Tests live **beside the code**, as `_test.go` files in the same package — not in a `tests/`
tree. What separates the layers is scope and build tags, not directory.

1. **Unit** — domain logic, mapping, validation. No network, no DB, no broker; infra
   dependencies replaced by fakes implementing the interfaces `domain` declares. Fast
   (<10s per service suite). This is where most tests live.
2. **Contract** — files tagged `//go:build contract`.
   - *Provider side:* every endpoint this service exposes is verified against
     `contracts/openapi/<service>.yaml` — load the spec and assert real responses validate
     against it. Hand-written structs that merely agree with your own reading of the spec
     prove nothing.
   - *Consumer side:* every external call and consumed event has a test pinning exactly the
     fields relied on, validated against the spec that defines them.
3. **Integration** — files tagged `//go:build integration`. Real Postgres/RabbitMQ via
   `testcontainers-go`, for transactional behavior (outbox, idempotency, migrations,
   constraint enforcement).

Tagged tests are excluded from a default `go test ./...`; CI runs them with `-tags`. Anything
requiring a container MUST be tagged, or the default suite silently depends on Docker.

Rule of thumb: business rule → unit; "do we agree with another service" → contract; "does the
plumbing work" → integration. Never test business logic through HTTP when a unit test reaches
it directly.

**ADR-0003's invariant is integration-only.** No-double-booking is enforced by a Postgres
exclusion constraint, so a fake repository cannot demonstrate it — a unit test with an
in-memory map will happily "prove" a guarantee the database is actually providing. Concurrent
reservation of one slot must be tested against a real Postgres, asserting exactly one caller
wins and the losers get `409 reservation_overlap`.

## TDD workflow (mandatory, see Hard rule #4)

1. Pick one acceptance criterion.
2. Write a failing test named after it:
   `func TestSendsOneEmailWhenBookingConfirmed_Issue42_AC1(t *testing.T)`.
3. Commit the red test (`test(notification): failing AC1 #42`).
4. Implement minimally to green. 5. Refactor. 6. Next AC.

Run the failing test and read the failure before implementing. A test that passes before the
feature exists is testing nothing, and that is invisible unless you watch it fail first.

## Conventions

- Test names describe behavior, not methods: ✅ `TestRejectsOverlappingReservation`
  ❌ `TestReserve2`.
- **Table-driven tests are the expected idiom** for a behavior across several inputs: a slice
  of named cases, one `t.Run(tc.name, ...)` per case. The loop is structural and encouraged;
  what stays banned is *conditional logic that decides what to assert* (`if tc.wantErr != nil`
  branching into different assertions). Split those into separate tests — a test whose
  assertions depend on a branch can pass by taking the branch that checks nothing.
- One behavior per test. AAA structure (arrange/act/assert).
- Builders/factories for test data (`internal/domain/testdata_test.go` or a `testutil`
  package) — no 30-line inline struct literals.
- Determinism: inject a clock and an ID generator; never call `time.Now()` or generate a UUID
  inside domain code under test. Zero tolerance for time- or order-dependent flakes.
- `go test -race` is mandatory, not optional. This project's core invariant is about
  concurrency; a suite that has never run under the race detector tells you nothing about it.
- Use `t.Parallel()` where tests are independent, and `t.Cleanup()` over `defer` for teardown
  that must survive a helper's return.
- Every consumer test includes: duplicate event delivery (idempotency), unknown extra fields
  (forward compat), and dependency-down (retry/DLQ) cases.
- **NEVER weaken an assertion or delete a test to make CI pass.** If a test seems wrong,
  comment on the issue/PR explaining why; changing test intent requires human approval.
- `t.Skip` requires a linked issue in a comment on the same line.

## Coverage

- Domain layer: ≥90% branches. Service overall: ≥80% lines. Measure with
  `go test -cover ./...`; `-coverprofile` + `go tool cover -func` to see the gaps.
- Coverage is a floor, not a goal — a PR full of assertion-free tests to hit numbers will be
  rejected in review. Go makes this failure mode easy: exercising a function without asserting
  on its result counts as covered.

## Pre-PR gate (run locally before `gh pr create`)

```bash
make check          # gofmt + vet + build + go test -race + contract lint
```

All green or the PR doesn't get opened. CI re-runs everything plus the `integration`- and
`contract`-tagged suites.
