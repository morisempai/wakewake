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

<!-- EDIT: entire file assumes TypeScript + NestJS + Postgres + RabbitMQ. Retarget as needed. -->

## Directory layout (mandatory)

```
services/<name>/
├── CLAUDE.md              # agent identity: responsibilities, contracts, hard rules
├── src/
│   ├── main.ts            # bootstrap only — no logic
│   ├── config.ts          # typed config, reads env, fails fast on missing vars
│   ├── health/            # /healthz (liveness), /readyz (checks db+broker)
│   ├── api/               # HTTP layer: controllers + DTO validation, NO business logic
│   ├── domain/            # business logic, pure where possible, no framework imports
│   ├── infra/             # db repositories, event publisher/consumer, external clients
│   └── events/            # consumer handlers (thin: validate → call domain)
├── tests/
│   ├── unit/              # mirrors src/ structure
│   └── contract/          # provider/consumer contract tests
├── migrations/            # SQL migrations, sequential, never edited after merge
├── Dockerfile
├── package.json
└── README.md              # what it owns, how to run it, links to its contracts
```

## Layering rule

`api`/`events` → `domain` → `infra`. Never skip layers, never import upward.
Controllers and consumers contain zero business decisions — they validate input,
call one domain method, map the result. If a controller has an `if` about business
state, it's in the wrong layer.

## Config

- All config via env vars, read ONCE in `config.ts` into a typed, validated object.
  Missing required var = crash at startup with a clear message, not a runtime surprise.
- No secrets in code, compose files committed with real values, or logs. Local dev
  values go in `.env.example` (committed) / `.env` (gitignored).

## Logging & observability

- Structured JSON logs via the shared logger (`shared/logger`). Fields always present:
  `service`, `level`, `correlation_id`, `event`/`route`.
- Correlation ID: taken from incoming header/event envelope, generated at the edge if
  absent, propagated to every outgoing call, event, and log line.
- Log levels: `error` = someone should look; `warn` = degraded but handled;
  `info` = business events; `debug` = off in production. Never log PII or tokens.
- Every HTTP handler and consumer emits duration metrics. <!-- EDIT: prom-client conventions -->

## Database

- One database per service; schema owned exclusively by it (Hard rule 6).
- Migrations: `migrations/NNNN_description.sql`, applied by CI, never by app at boot
  in production. New columns nullable-or-defaulted first; destructive changes need an ADR.

## Dockerfile

Multi-stage: build stage → slim runtime (`node:22-alpine`), non-root user,
`HEALTHCHECK` hitting `/healthz`, no dev dependencies in final image.

## Scaffolding a new service

Copy `services/_template/` if it exists; otherwise follow this file exactly.
Then: add compose entry, add CI matrix entry (via `shared-change` issue — `.github/` is
protected), create its CLAUDE.md, and verify `docker compose up <name>` passes `/readyz`.
