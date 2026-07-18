---
name: service-template
description: >
  The required anatomy of a service in this repo: folder structure, config, health endpoints,
  logging, Dockerfile, and boilerplate conventions. Use this whenever scaffolding a new
  service, adding a new module/endpoint/consumer to an existing service, writing a Dockerfile
  or docker-compose entry, wiring configuration or env vars, or setting up logging — and when
  reviewing whether a service follows project structure. Every service must look like this
  template; deviations require an ADR.
---

# Service Template

Services are **Go** (ADR-0008). Node.js appears in this repo only to run the contract linters
over `contracts/`; it is never a service runtime.

## Directory layout (mandatory)

```
services/<name>/
├── CLAUDE.md              # agent identity: responsibilities, contracts, hard rules
├── go.mod                 # one module per service — dependencies owned as exclusively as data
├── cmd/<name>/main.go     # bootstrap only: read config, wire deps, start, shut down. No logic.
├── internal/              # compiler-enforced privacy: nothing here is importable by another service
│   ├── config/            # env → typed struct, validated once, fails fast
│   ├── health/            # /healthz (liveness), /readyz (checks db + broker)
│   ├── api/               # HTTP layer: handlers + request validation, NO business logic
│   ├── domain/            # business logic, no framework or driver imports
│   ├── infra/             # db repositories, event publisher/consumer, external clients
│   └── events/            # consumer handlers (thin: validate → call domain)
├── migrations/            # SQL migrations, sequential, never edited after merge
├── Dockerfile
└── README.md              # what it owns, how to run it, links to its contracts
```

Use `internal/` rather than a flat package tree: Go refuses at compile time to let another
module import it. Hard rule #6's data ownership gets the same enforcement for code.

New modules must be added to the root `go.work` `use` list, or the workspace ignores them.
`go.work` pins the Go version — match it in `go.mod`; do not raise one without the other.

## Test placement

Go convention puts `_test.go` files **next to the code they test**, in the same package, not in
a separate tree. Follow it — `internal/domain/booking_test.go`, not `tests/unit/...`.
See `testing-standards` for what belongs in each layer and how integration tests are tagged.

## Layering rule

`api`/`events` → `domain` → `infra`. Never skip layers, never import upward.
Handlers and consumers contain zero business decisions — they validate input, call one domain
method, map the result. If a handler has an `if` about business state, it's in the wrong layer.

In Go, invert the dependency with interfaces: `domain` **declares** the narrow interface it
needs (`type Reservations interface { Insert(...) error }`) and `infra` implements it. The
interface belongs to the consumer, not the implementation — that is what keeps `domain` free of
`pgx` and `amqp` imports and unit-testable with plain fakes.

## Config

- All config via env vars, read ONCE at startup into a typed, validated struct in
  `internal/config`. Missing required var = fail before serving, with a message naming the var.
- Pass the config struct explicitly down the call chain. No package-level globals, no `init()`
  reading env — both make tests order-dependent and hide what a component actually needs.
- No secrets in code, in committed compose files, or in logs. Local dev values go in
  `.env.example` (committed) / `.env` (gitignored).

## Logging & observability

- Structured JSON via stdlib `log/slog`. Fields always present: `service`, `level`,
  `correlation_id`, `event`/`route`.
- Carry the correlation ID in `context.Context` and take a `context.Context` as the first
  parameter of anything that logs, calls out, or touches the DB. This is what makes propagation
  automatic rather than something each call site has to remember.
- Correlation ID: taken from incoming header/event envelope, generated at the edge if absent,
  propagated unchanged to every outgoing call, event, and log line.
- Log levels: `error` = someone should look; `warn` = degraded but handled; `info` = business
  events; `debug` = off in production. Never log PII or tokens.
- Metrics via `prometheus/client_golang`; traces via `go.opentelemetry.io/otel` (ADR-0007).
  Every HTTP handler and consumer emits duration metrics.

## Errors

- Wrap with `fmt.Errorf("...: %w", err)` so callers can `errors.Is`/`errors.As`. Never discard
  an error to satisfy the compiler — `_ = err` needs a comment explaining why it is safe.
- `domain` returns domain error values (`var ErrSlotUnavailable = errors.New(...)`), and `api`
  maps them to the HTTP codes in `contracts/openapi/<service>.yaml`. Driver errors must not
  leak upward: `infra` translates them — e.g. Postgres SQLSTATE `23P01` (exclusion constraint
  violation, ADR-0003) becomes `ErrSlotUnavailable`, which `api` renders as
  `409 reservation_overlap`. That mapping is a contract obligation, not an implementation
  detail; test it.

## Database

- One database per service; schema owned exclusively by it (Hard rule #6).
- `pgx` directly. No ORM without an ADR — ADR-0003 puts the core invariant in a Postgres
  exclusion constraint, and an abstraction that hides SQLSTATE codes hides the invariant firing.
- Migrations: `migrations/NNNN_description.sql`, applied by CI, never by the app at boot in
  production. New columns nullable-or-defaulted first; destructive changes need an ADR.

## Dockerfile

Multi-stage: `golang:<version from go.work>` build stage → minimal runtime. Build with
`CGO_ENABLED=0` for a static binary, copy it into `gcr.io/distroless/static:nonroot` (or
`alpine` if a shell is genuinely needed), run as non-root, `HEALTHCHECK` hitting `/healthz`.
No toolchain or source in the final image.

## Scaffolding a new service

Follow this file exactly. Then: add the module to `go.work`, add a compose entry, add a CI
matrix entry, create its `CLAUDE.md`, and verify `docker compose up <name>` passes `/readyz`.

`.github/` is protected — the CI matrix entry goes through a `shared-change` issue, not a
direct edit.
