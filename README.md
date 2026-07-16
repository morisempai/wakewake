# Watercraft Booking Platform

Booking & rental platform for **boats, yachts, and wakesurf sessions** — a contract-first
microservices monorepo. See `CLAUDE.md` for the non-negotiable working rules and
`docs/adr/` for the architecture decisions.

## Layout

```
contracts/openapi/    OpenAPI 3.1, one file per service — SOURCE OF TRUTH for HTTP
contracts/asyncapi/   AsyncAPI — SOURCE OF TRUTH for events
services/<name>/  one service per dir, each owns its DB, each has its own CLAUDE.md
shared/contracts/     types generated from the specs above
shared/logger/        structured JSON logger + correlation-id context (service-template skill)
shared/platform/      transactional-outbox helper, event envelope
docs/adr/             Architecture Decision Records
docs/stories/         user stories (versioned)
docs/nfr.md           project-wide non-functional defaults (availability, latency, retention)
infra/            docker-compose (local dev) + terraform (per-env AWS accounts)
.github/          CI/CD
```

## Services (first vertical slice)

| Service        | Responsibility                                              | Owns DB        |
|----------------|-------------------------------------------------------------|----------------|
| `gateway`      | Sole ingress; OIDC auth, rate limit, correlation id         | —              |
| `catalog`      | Products (boats/yachts/wakesurf)                             | `catalog`      |
| `availability` | Anti-double-booking engine (Postgres exclusion constraint)  | `availability` |
| `booking`      | Booking lifecycle + hold→pay→confirm saga (outbox)          | `booking`      |
| `payment`      | Stripe (test) payments, idempotent                          | `payment`      |
| `notification` | Confirmation email (Mailhog in dev)                         | `notification` |

Identity is **Keycloak** (self-hosted); CMS will be **Strapi/Directus** (ADR-0004). Deferred contexts
(marketplace, promotions, CRM, audit UI, split-bill) are tracked in `docs/stories/deferred-backlog.md`.

## Local dev

Prereqs: **Node 22** (`.nvmrc`), **pnpm 9**, **Docker**.

```bash
cp .env.example .env
pnpm install
pnpm infra:up        # Postgres, RabbitMQ, Keycloak, Vault, Mailhog, OTel, Grafana/Prometheus/Tempo/Loki
pnpm build && pnpm test
```

Dev URLs: RabbitMQ UI `:15672`, Keycloak `:8080`, Mailhog `:8025`, Grafana `:3000`, Vault `:8200`.

> Never test against anything remote. Deployment happens only through CI/CD — never run cloud CLIs by hand.

## Core invariant

No two customers can book the same resource over overlapping time. Enforced **by the database** via a
Postgres exclusion constraint in the Availability service (ADR-0003), wrapped in a hold/TTL saga.
