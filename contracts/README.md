# Contracts — the source of truth

Everything crossing a service boundary is defined here. Code implements these files; it never
defines them (root `CLAUDE.md`, hard rule #2).

```
contracts/openapi/     OpenAPI 3.1, one file per service — HTTP
contracts/asyncapi/    AsyncAPI 3.0 — events
```

| File | Service | Boundary |
|---|---|---|
| `openapi/catalog.yaml` | catalog | Browse and fetch products |
| `openapi/availability.yaml` | availability | Query slots, reserve, confirm, release |
| `openapi/booking.yaml` | booking | Hold, fetch, cancel — the saga's entry point |
| `openapi/payment.yaml` | payment | Start a payment, fetch status, Stripe webhook |
| `asyncapi/booking-events.yaml` | all | The seven domain events |

## Rules

**Service agents never edit this directory.** A wrong or missing contract is an issue labeled
`contract-change` stating what's needed, why, which consumers are affected, and whether the change
is compatible or breaking. Work depending on it stops until the contract PR merges.

**Read before writing code that crosses a boundary.** Use only documented endpoints and fields.
A wrong assumption about another service's behavior is the most expensive mistake available here.

**Conventions live in the api-contracts skill**, not in this README — versioning, the error
envelope, pagination, auth, and naming are defined there and enforced by `.spectral.yaml`. If the
skill and the ruleset disagree, the skill wins and the ruleset is the bug.

## Two things worth knowing before you read the specs

**A successful availability query guarantees nothing.** Between querying slots and reserving one,
another customer can take it. The reserve call is the only authority: it returns `409
reservation_overlap` when the Postgres exclusion constraint (ADR-0003) rejects an overlap. This
is deliberately a contract-level fact, not an implementation detail — callers must handle it.

**A `201` from `POST /v1/payments` is not a successful payment.** It creates a Stripe
PaymentIntent in `pending`. The authoritative outcome arrives later via the signature-verified
Stripe webhook, which is what emits `PaymentSucceeded` / `PaymentFailed`.

## Validating locally

```bash
make contracts-lint            # both of the below
make contracts-lint-openapi    # Spectral, using .spectral.yaml
make contracts-lint-asyncapi   # AsyncAPI CLI
```

These need **Node 22** (`.nvmrc`), even though the services are Go (ADR-0008). Spectral and the
AsyncAPI CLI lint YAML; what implements the specs is irrelevant to them. Node is in this repo for
these two commands and nothing else.

CI runs the same checks on every PR. They invoke `npx` with pinned majors rather than installed
dependencies, so there is no Node lockfile to maintain for a repo with no Node code.
