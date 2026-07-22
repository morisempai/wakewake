# booking

The booking aggregate and the **hold→pay→confirm saga** with compensation (ADR-0003). Booking is
the only service that decides a booking's state (`held → confirmed` or `held → cancelled`), and it
is the saga orchestrator: it reserves through Availability, reacts to Payment's events, and reaches
a terminal state deterministically. Zero-RPO is the target — every state change is written in the
same transaction as its outbox row (ADR-0002).

## Contracts (source of truth)

This service implements these; it never defines them.

| Kind | File |
| --- | --- |
| HTTP | [`contracts/openapi/booking.yaml`](../../contracts/openapi/booking.yaml) |
| Events | [`contracts/asyncapi/booking-events.yaml`](../../contracts/asyncapi/booking-events.yaml) |

- **Publishes:** `BookingHeld`, `BookingConfirmed`, `BookingCancelled`
- **Consumes:** `PaymentSucceeded` → confirm, `PaymentFailed` → cancel (compensation),
  `ReservationReleased`(hold_expired) → cancel the abandoned hold
- **Calls (HTTP):** Availability `createReservation` + `confirmReservation`; Catalog `getProduct`

## The saga

```
createBooking ──▶ Catalog.getProduct ──▶ Availability.createReservation ──▶ [booking HELD + BookingHeld]  (one tx)
                                                                                    │
                        PaymentSucceeded ──▶ Availability.confirmReservation (sync) ─┴─▶ [CONFIRMED + BookingConfirmed]
                        PaymentFailed    ─────────────────────────────────────────────▶ [CANCELLED + BookingCancelled]  (Availability self-releases)
                        ReservationReleased(hold_expired) ────────────────────────────▶ [CANCELLED + BookingCancelled]
```

Three facts are load-bearing and easy to break by accident:

- **Confirm is synchronous over HTTP.** On `PaymentSucceeded` booking calls Availability's confirm
  endpoint and only then promotes the booking. Availability does **not** consume `BookingConfirmed`
  — that is for notification. (The AsyncAPI summary string listing availability as a consumer is
  stale; see issue #12.)
- **Compensation is event-driven.** On `PaymentFailed` booking cancels and emits `BookingCancelled`;
  Availability frees the slot by consuming that event. Booking does **not** call Availability's
  release endpoint.
- **The outbox row shares the state change's transaction.** `occurred_at` is the database
  transaction time, never the app clock (ADR-0002).

## Owns

The `booking` database, exclusively. No other service may read these tables (hard rule #6).

| Table | Purpose |
| --- | --- |
| `booking` | The aggregate and its lifecycle. |
| `outbox` | Events staged in the same transaction as the state change (ADR-0002). |
| `idempotency_key` | `Idempotency-Key` → booking, for safe create-hold retries. |
| `processed_event` | Consumer dedupe, keyed on the envelope id. |

## Layout

```
cmd/booking/     bootstrap only — read config, wire, start, shut down
internal/api/    HTTP handlers against the generated strict server; auth; no business decisions
internal/domain/ the booking lifecycle and the saga orchestration; no pgx, no amqp (CI enforces this)
internal/infra/  the Postgres store, and the Availability + Catalog typed HTTP clients
internal/events/ the PaymentSucceeded / PaymentFailed / ReservationReleased consumers
internal/config/ env → typed struct, validated once at startup
migrations/      sequential SQL, applied by CI, never by the app at boot
```

`internal/domain` declares the interfaces it needs (`Store`, `Reservations`, `Catalog`, `Clock`,
`IDGen`) and the other layers implement them. The interface belongs to the consumer, which keeps the
domain free of drivers and unit-testable with plain fakes.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `DATABASE_URL` | *required* | This service's own database. Startup fails without it. |
| `RABBITMQ_URL` | *required* | Broker. Startup fails without it. |
| `AVAILABILITY_BASE_URL` | `http://availability:8080` | Availability HTTP API base URL. |
| `CATALOG_BASE_URL` | `http://catalog:8080` | Catalog HTTP API base URL. |
| `BOOKING_HOLD_TTL_SECONDS` | `900` | Hold lifetime. Availability reads the same variable for the reservation's expiry; the two must agree. |
| `HTTP_ADDR` | `:8080` | Listen address. |
| `HTTP_SHUTDOWN_TIMEOUT` | `15s` | Grace period for in-flight requests. |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. |

There is **no** TTL sweeper in this service: Availability owns the sweep and publishes
`ReservationReleased`, which booking consumes.

## Probes

- `GET /healthz` — liveness. Checks **nothing** on purpose.
- `GET /readyz` — readiness. Checks Postgres and the broker connection.

## Tests

```bash
go test ./...                      # unit — fast, no Docker
go test -tags contract ./...       # responses validated against booking.yaml; no Docker
go test -tags integration ./...    # real Postgres via testcontainers; needs Docker
go test -race ./...
```

- **AC1** (outbox atomicity) and **AC2** (idempotency) live in
  `internal/infra/store_integration_test.go`, including a rolled-back transaction that leaves neither
  the booking nor its event.
- **AC3/AC4** (consumer dedupe, unknown fields, and the three saga paths) live in
  `internal/events/saga_integration_test.go`, each asserting the emitted envelope validates against
  the AsyncAPI spec.

## Known deviations & flagged gaps

- **No JWT signature verification.** The spec declares `bearerAuth`; this service reads the `sub`
  claim to scope bookings but does **not** verify the signature. ADR-0006 puts verification at the
  gateway (not yet wired) and Keycloak is not reachable from the test environment. Flagged, not
  overlooked — a shared verifier is a `shared-change`, not a booking-local edit.
- **Catalog dependency.** `createBooking` resolves `product_id` → `resource_id`, capacity and price
  through Catalog's `getProduct`. Catalog's service is not part of this slice, so integration tests
  fake it; only its committed contract was used.
- **No `catalog_unavailable` error code.** `booking.yaml` has a 503 named `availability_unavailable`
  but none for Catalog. A Catalog outage is reported as `503 availability_unavailable` (correct retry
  signal, imprecise name). Raised as a contract-change candidate.
- **Pricing model.** `total_minor` is taken as Catalog's `base_price_minor` (a flat price). The
  contracts do not define whether party size multiplies the price, and Catalog's price is documented
  as indicative while Payment computes the authoritative amount. Flagged.
- **Paid-but-lost-slot.** If `PaymentSucceeded` arrives after the hold's TTL swept it, Availability's
  confirm returns 422 and booking cancels (`hold_expired`). This is a real refund situation; refunds
  are out of scope for this slice per `booking.yaml`.

## Decisions

[ADR-0002](../../docs/adr/0002-async-messaging-rabbitmq-outbox.md) ·
[ADR-0003](../../docs/adr/0003-no-double-booking-exclusion-constraint-saga.md) ·
[ADR-0006](../../docs/adr/0006-api-gateway-sole-ingress.md) ·
[ADR-0009](../../docs/adr/0009-shared-module-boundary-and-codegen.md) ·
[ADR-0010](../../docs/adr/0010-rabbitmq-topology-and-outbox-parameters.md)
