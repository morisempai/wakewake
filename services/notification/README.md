# notification

Sends **transactional** notifications. In this slice that is one thing: the booking-confirmation
email. The service is **consumer-only** — it consumes `BookingConfirmed` and sends exactly one
email per booking. It publishes no events and exposes no public HTTP API (only orchestration
probes). Marketing/campaign notifications are explicitly deferred (CLAUDE.md).

## Contracts (source of truth)

This service implements these; it never defines them.

| Kind | File |
| --- | --- |
| Events consumed | [`contracts/asyncapi/booking-events.yaml`](../../contracts/asyncapi/booking-events.yaml) → `BookingConfirmed` |

- **Consumes:** `BookingConfirmed` → send the confirmation email.
- **Publishes:** nothing.
- **HTTP:** none public. `GET /healthz`, `GET /readyz` for probes only.

## How it works

```
BookingConfirmed ──▶ inbox.Process (dedupe tx) ──▶ resolve recipient ──▶ compose ──▶ record ──▶ send (SMTP → Mailhog)
                          │                                                            │
                          └── processed_event + sent_notification committed together ──┘
```

Three facts are load-bearing:

- **The handler runs inside the dedupe transaction.** `shared/platform/inbox` hands the handler a
  `pgx.Tx`; the `processed_event` dedupe row and the `sent_notification` audit row commit together.
  A redelivery of the same envelope `id` is suppressed by `processed_event`, so **a duplicate
  delivery sends exactly one email** (ADR-0002, at-least-once).
- **The email is sent inside that transaction, before commit.** Database-plus-SMTP is a dual write;
  exactly-once across the two is impossible. The choice made here is to send before commit, so the
  one unavoidable failure window (a commit that fails after the relay accepted the mail) resolves as
  a **duplicate** confirmation on redelivery, never a **lost** one. For a payment confirmation that
  is the right side to err on. See `internal/events/confirmation.go`.
- **No PII in logs or at rest.** `BookingConfirmed` carries no email by construction (contract
  x-notes, NFR-4). Recipient addresses are **redacted** in every log line (`c***@example.test`), and
  only the redacted form is persisted in `sent_notification` — the raw address is never logged and
  never stored.

## KNOWN GAP — recipient resolution

`BookingConfirmed` carries `customer_id` but deliberately no email, and there is **no
customer/identity service in this slice**, so there is no real source for the address. Rather than
invent one or read another service's database (hard rule #6), the service ships a **DEV stub**
(`infra.DevRecipientResolver`) behind the `domain.RecipientResolver` port. It derives a
deterministic, non-deliverable address from the customer id:

```
customer-<customer_id>@example.test     (.test is a reserved TLD — RFC 6761)
```

A production resolver must call the real identity source over its API. That is **out of scope for
this slice and flagged as the follow-up** that closes this gap.

## Owns

The `notification` database, exclusively. No other service may read these tables (hard rule #6).

| Table | Purpose |
| --- | --- |
| `sent_notification` | One row per confirmation email sent. `event_id` (the envelope id) is the idempotency key; recipient stored **redacted only**. Audit + dedupe. |
| `processed_event` | Consumer dedupe, keyed on the envelope id. Canonical DDL pasted from `shared/platform/inbox`. |

## Layout

```
cmd/notification/  bootstrap only — read config, wire, start, shut down
internal/domain/   email composition and recipient redaction; the RecipientResolver + Mailer ports; no pgx, no amqp
internal/infra/    the sent_notification store, the SMTP mailer, and the DEV recipient resolver
internal/events/   the BookingConfirmed consumer (thin: decode → resolve → compose → record → send)
internal/config/   env → typed struct, validated once at startup
migrations/        sequential SQL, applied by CI, never by the app at boot
```

`internal/domain` imports only the standard library (no `shared/platform`, no drivers). The
`Recorder` interface the handler needs is declared in `internal/events` and implemented by
`internal/infra` — the interface belongs to the consumer.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `DATABASE_URL` | *required* | This service's own database. Startup fails without it. |
| `RABBITMQ_URL` | *required* | Broker. Startup fails without it. |
| `SMTP_HOST` | `mailhog` | Outbound SMTP relay host. In dev, Mailhog. |
| `SMTP_PORT` | `1025` | Outbound SMTP relay port. |
| `NOTIFICATION_FROM_ADDRESS` | `no-reply@bookings.example.test` | Envelope-from on every message. |
| `HTTP_ADDR` | `:8080` | Probe listen address. |
| `HTTP_SHUTDOWN_TIMEOUT` | `15s` | Grace period for in-flight probes. |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. |

## Probes

- `GET /healthz` — liveness. Checks **nothing** on purpose.
- `GET /readyz` — readiness. Checks Postgres and the broker connection.

## Tests

```bash
go test ./...                      # unit — fast, no Docker
go test -tags integration ./...    # real Postgres via testcontainers; needs Docker
go test -race ./...
```

- **AC3** (unknown-field tolerance) and **AC4** (recipient redaction / no-PII-in-logs, correlation
  id on the path) have fast unit tests in `internal/events/confirmation_test.go` driven through the
  real handler with fakes.
- **AC5** (`sent_notification` migration and idempotency) lives in
  `internal/infra/store_integration_test.go` against a real Postgres via `pgtest`.
- **AC2** (duplicate delivery → exactly one email) and **AC3** end-to-end live in
  `internal/events/confirmation_integration_test.go`, driven through the real `inbox` transaction.
- **AC1** (delivery to Mailhog, read back via its HTTP API) is written in the same file but
  **skips** unless `SMTP_HOST`/`SMTP_PORT`/`MAILHOG_API_URL` point at a running Mailhog — see the
  gap below.

## Known deviations & flagged gaps

- **Recipient resolution is a DEV stub.** See "KNOWN GAP" above (issue #19). A real identity source
  is the follow-up.
- **AC1 Mailhog test cannot self-start a container.** `shared/testkit` ships `pgtest` and `amqptest`
  but no Mailhog helper, and importing `testcontainers-go` directly would pull the Docker client
  dependency tree into this service's `go.sum` (forbidden — verified). The AC1 assertion is written
  and runs against the compose Mailhog when `SMTP_HOST`/`SMTP_PORT`/`MAILHOG_API_URL` are set, and
  otherwise skips with **issue #20** linked (a `mailhogtest` testkit helper).
- **The compose/CI wiring for this service is not added here.** `infra/docker-compose.yml` (platform
  infra) and `.github/` (CI matrix) are outside this service's write scope. Adding the notification
  service entry and its CI job is a follow-up via a `shared-change` issue.
- **Money formatting assumes a 2-decimal minor unit** (EUR/USD/GBP). Zero-decimal currencies like
  JPY would render wrong; a currency-aware exponent is a follow-up. Flagged.
- **SMTP is plaintext, no auth** — correct for the dev Mailhog sink. A production relay needs
  STARTTLS/auth and a context-aware client; that is config plus a small mailer change, out of scope.

## Decisions

[ADR-0002](../../docs/adr/0002-async-messaging-rabbitmq-outbox.md) ·
[ADR-0009](../../docs/adr/0009-shared-module-boundary-and-codegen.md) ·
[ADR-0010](../../docs/adr/0010-rabbitmq-topology-and-outbox-parameters.md)
