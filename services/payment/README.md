# payment

Takes payment for a held booking via **Stripe (test mode in dev)**, idempotently, and emits the
outcome. The authoritative outcome arrives on Stripe's **webhook**, not the create response — a
`pending` payment says nothing about whether the charge will succeed.

**PCI is the design constraint.** Raw card data never reaches this service and is never stored or
logged: the browser sends the card straight to Stripe and returns a token; we persist only Stripe
ids, the amount (read from the booking), the currency, and the outcome.

## Contracts (source of truth)

This service implements these; it never defines them.

| Kind | File |
| --- | --- |
| HTTP | [`contracts/openapi/payment.yaml`](../../contracts/openapi/payment.yaml) |
| Events | [`contracts/asyncapi/booking-events.yaml`](../../contracts/asyncapi/booking-events.yaml) |

- **Publishes:** `PaymentSucceeded`, `PaymentFailed` (only from a verified webhook)
- **Consumes:** `BookingHeld` → prepare the payment context for the hold

## The flow

```
BookingHeld ─────────────────────────────▶ [booking_context: amount, currency, hold expiry]

POST /v1/payments ─▶ read amount from context ─▶ Stripe PaymentIntent (idempotency key) ─▶ [payment PENDING] ─▶ 201 + client_secret

POST /v1/webhooks/stripe ─▶ VERIFY SIGNATURE ─▶ payment_intent.succeeded ─▶ [SUCCEEDED + PaymentSucceeded]  (one tx)
                                             └─▶ payment_intent.payment_failed ─▶ [FAILED + PaymentFailed]  (one tx)
```

Booking consumes `PaymentSucceeded`/`PaymentFailed` to confirm or compensate — that is booking's
concern, not payment's.

Load-bearing facts, easy to break by accident:

- **The webhook signature is verified over the RAW request body, before the body is parsed or
  trusted.** An invalid, absent, or expired signature is rejected (`400`) and mutates nothing and
  emits nothing. This is why the webhook is a raw handler mounted ahead of the generated strict
  server (which would decode — and so destroy — the raw bytes).
- **The amount is read from the booking, never the request.** A client-supplied amount would let a
  customer set their own price. It comes from `booking_context`, built from `BookingHeld`.
- **Every charge carries an idempotency key to Stripe**, and `createPayment` is idempotent on
  `Idempotency-Key` locally. A retried charge is not a double charge.
- **The status change and its outbox row share one transaction** (ADR-0002). `occurred_at` is the
  database transaction time, never the app clock.
- **Secrets never appear in logs.** The Stripe secret key goes only in the Authorization header; the
  webhook signing secret is used only to verify.

## Owns

The `payment` database, exclusively. No other service may read these tables (hard rule #6).

| Table | Purpose |
| --- | --- |
| `payment` | The payment aggregate. **No card columns** — asserted by a test (AC2). |
| `outbox` | Events staged in the same transaction as the status change (ADR-0002). |
| `idempotency_key` | `Idempotency-Key` → payment, for safe createPayment retries. |
| `processed_event` | Consumer dedupe (BookingHeld), keyed on the envelope id. |
| `booking_context` | Payment's own projection of `BookingHeld`: the authoritative amount/currency and hold expiry. |
| `stripe_event` | Webhook dedupe, keyed on the Stripe event id. |

## Layout

```
cmd/payment/     bootstrap only — read config, wire, start, shut down
internal/api/    HTTP handlers (strict server) + the raw Stripe webhook; auth; no business decisions
internal/domain/ the payment lifecycle and the charge flow; no pgx, no amqp, no HTTP (CI enforces this)
internal/stripe/ signature verification, the PaymentIntents client, and event parsing
internal/infra/  the Postgres store
internal/events/ the BookingHeld consumer
internal/config/ env → typed struct, validated once at startup
migrations/      sequential SQL, applied by CI, never by the app at boot
```

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `DATABASE_URL` | *required* | This service's own database. Startup fails without it. |
| `RABBITMQ_URL` | *required* | Broker. Startup fails without it. |
| `STRIPE_SECRET_KEY` | *required* | Stripe API key (`sk_test_…` in dev). **Secret** — never logged. |
| `STRIPE_WEBHOOK_SECRET` | *required* | Webhook signing secret (`whsec_…`). **Secret** — never logged. |
| `STRIPE_BASE_URL` | `https://api.stripe.com` | Stripe API base. Override for a local mock. |
| `STRIPE_WEBHOOK_TOLERANCE_SECONDS` | `300` | Max signature-timestamp skew before a webhook is rejected as a replay. |
| `HTTP_ADDR` | `:8080` | Listen address. |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. |

## Probes

- `GET /healthz` — liveness. Checks **nothing** on purpose.
- `GET /readyz` — readiness. Checks Postgres and the broker connection.

## Tests

```bash
go test ./...                      # unit — fast, no Docker
go test -tags contract ./...       # responses validated against payment.yaml; webhook signature gate; no Docker
go test -tags integration ./...    # real Postgres via testcontainers; needs Docker
go test -race ./...
```

- **AC1** (webhook signature): valid signed payloads accepted, invalid/absent/expired rejected —
  `internal/stripe/signature_test.go` (unit) and `internal/api/contract_test.go` (through the real
  router, proving a rejected webhook records nothing).
- **AC2** (no card data at rest): `internal/infra/store_integration_test.go` inspects the live
  `payment` table's columns.
- **AC3** (createPayment idempotency): `internal/domain/service_test.go` (replay) and
  `internal/infra/store_integration_test.go` (replay, reuse → 409, concurrent same-key → one row).
- **AC4** (outcome + outbox in one tx, envelope validates): `internal/infra/store_integration_test.go`.
- **AC5** (BookingHeld consumer idempotent, unknown fields):
  `internal/events/consumer_integration_test.go`.
- **AC6** (responses validate against `payment.yaml`, secrets never logged, correlation id):
  `internal/api/contract_test.go`.

## Known deviations & flagged gaps

- **No JWT signature verification.** The spec declares `bearerAuth`; this service reads the `sub`
  claim to authorise a payment against its booking's owner but does **not** verify the signature.
  ADR-0006 puts verification at the gateway (not yet wired) and Keycloak is not reachable from the
  test environment. A shared verifier is a `shared-change`, not a payment-local edit.
- **Payability signal is local and partial.** `createPayment` returns `422 booking_not_payable`
  when the hold has **expired**, which is the only signal payment holds locally. The contract also
  lists "already confirmed or cancelled" as not-payable; payment does not learn those transitions in
  this slice (it consumes only `BookingHeld`), so a booking that was confirmed/cancelled elsewhere
  is not distinguished here. Flagged as a possible future event subscription.
- **Stripe is faked in all tests.** No test reaches the real Stripe. The PaymentIntents client is
  driven through a stubbed HTTP transport; webhook fixtures are signed locally with the test signing
  secret to produce valid AND invalid payloads. Real Stripe behaviour (e.g. its own idempotency
  replay of a client secret, its exact error bodies) is therefore assumed, not verified end to end.
- **`booking_context` and `stripe_event` are payment-owned tables** beyond the three named in the
  service CLAUDE.md. They are this service's own data (a projection of `BookingHeld`, and webhook
  dedupe), not another service's tables — data ownership is intact.
- **Not wired into `infra/docker-compose.yml`.** That file is out of this service's write scope
  (root rule #1). `SERVICE_DATABASES` there already lists `payment`, but no `payment` service block
  or Stripe env vars exist yet — an infra change to be raised separately.

## Decisions

[ADR-0002](../../docs/adr/0002-async-messaging-rabbitmq-outbox.md) ·
[ADR-0006](../../docs/adr/0006-api-gateway-sole-ingress.md) ·
[ADR-0009](../../docs/adr/0009-shared-module-boundary-and-codegen.md) ·
[ADR-0010](../../docs/adr/0010-rabbitmq-topology-and-outbox-parameters.md)
