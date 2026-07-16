# Booking Microservices — Root Rules


## Project

Microservices booking. Monorepo layout:

- `contracts/` — OpenAPI (HTTP) + AsyncAPI (events) specs. **Source of truth for all inter-service communication.**
- `services/<name>/` — one directory per service, each with its own CLAUDE.md
- `docs/adr/` — Architecture Decision Records
- `docs/stories/` — user stories (markdown, versioned)
- `shared/` — shared libraries (protected, CODEOWNERS-gated)
- `.github/workflows/` — CI/CD (protected)

Stack: Go 1.23, PostgreSQL, RabbitMQ, Docker (ADR-0008, superseding ADR-0001's stack choice).
Node 22 is present for one reason only: running the contract linters (Spectral, AsyncAPI CLI)
over `contracts/`. It is not a service runtime. See the Makefile header.

## Hard rules (non-negotiable)

1. **Write scope.** You may only create/edit files inside the service directory named in your
   service CLAUDE.md. Never edit `contracts/`, `shared/`, `.github/`, `.claude/`, or other services.
   If a change is needed there: open a GitHub issue (label `contract-change` or `shared-change`),
   reference it in your PR, and stop work that depends on it.
2. **Contract-first.** Never invent or assume an endpoint, payload field, or event shape.
   Read the spec in `contracts/` before writing any code that crosses a service boundary.
   Code implements contracts; it never defines them.
3. **No direct cloud access.** Deployment happens ONLY through CI/CD. Never run cloud CLI
   commands (aws/gcloud/az/kubectl against remote clusters), never handle cloud credentials.
   Infrastructure changes = edits to IaC files, proposed via issue if outside your scope.
4. **TDD.** Write a failing test tied to an acceptance criterion before implementation code.
5. **Everything through PRs.** Never push to `main`. Branch naming: `feat/<service>-<issue#>`.
   Commits: Conventional Commits with issue ref, e.g. `feat(contacts): add merge endpoint (#42)`.
6. **Data ownership.** Each service owns its database exclusively. Never query another
   service's tables — use its API or events per the contracts.
7. **Traceability.** Every PR links a story/issue. Every significant design choice gets an ADR.
   Abandoned work is closed with an explanation, never silently deleted.

## Definition of done (any story)

- [ ] Failing tests written first, now green (unit + contract where applicable)
- [ ] Lint passes, no skipped/disabled tests without a linked issue
- [ ] Structured logs with correlation IDs on all new code paths
- [ ] PR opened with story link, summary of approach, and any deviations flagged

## Local environment

- `docker compose up` from repo root starts all services + Postgres + RabbitMQ
- `npm test` inside a service runs its unit tests; `npm run test:contract` runs contract tests
- Never test against anything remote

## When unsure

Prefer opening an issue or asking over guessing. A wrong assumption about a contract
or another service's behavior is the most expensive class of mistake in this project.

