# availability

The anti-double-booking engine. Owns the resource calendar and reservations, and holds the one
claim in this repository that could actually be wrong: **no two live reservations may hold the
same resource over overlapping time.**

That invariant is enforced by the database, by a Postgres exclusion constraint, and by nothing
else. There is deliberately no application-level `SELECT`-then-`INSERT` anywhere in the reserve
path — that pattern races, and the race is precisely the bug the whole design exists to prevent.

## Contracts (source of truth)

This service implements these; it never defines them.

| Kind | File |
| --- | --- |
| HTTP | [`contracts/openapi/availability.yaml`](../../contracts/openapi/availability.yaml) |
| Events | [`contracts/asyncapi/booking-events.yaml`](../../contracts/asyncapi/booking-events.yaml) |

- **Publishes:** `ReservationCreated`, `ReservationReleased`
- **Consumes:** `BookingCancelled` → releases the reservation (the saga's compensation leg)

## Owns

The `availability` database, exclusively. No other service may read these tables (hard rule #6).

| Table | Purpose |
| --- | --- |
| `reservation` | The calendar. Carries the exclusion constraint. |
| `outbox` | Events staged in the same transaction as the state change (ADR-0002). |
| `idempotency_key` | `Idempotency-Key` → reservation, for safe retries. |
| `processed_event` | Consumer dedupe, keyed on the envelope id. |

## The invariant

```sql
CONSTRAINT reservation_no_overlap EXCLUDE USING gist (
  resource_id WITH =,
  during      WITH &&
) WHERE (status <> 'released')
```

Three things about it are load-bearing and are easy to break by accident:

- **`during` is a half-open `tstzrange`, `[starts_at, ends_at)`.** 10:00–11:00 and 11:00–12:00
  share an endpoint and do **not** overlap. A closed range would reject back-to-back bookings and
  silently lose revenue.
- **The constraint is partial** (`WHERE status <> 'released'`, ADR-0011, amending ADR-0003).
  Released rows are retained as history. A constraint over the whole table would let every
  expired hold poison its window permanently, turning the abandoned-checkout protection into a
  slow leak of inventory.
- **`CHECK (NOT isempty(during))`.** An equal-bounds `[)` range normalises to `empty` in
  Postgres, and an empty range overlaps *nothing* — it would slip past the exclusion constraint
  entirely.

A violation arrives as SQLSTATE `23P01` and is mapped to `409 reservation_overlap`. That mapping
is a contract obligation, not an implementation detail.

## Layout

```
cmd/availability/     bootstrap only — read config, wire, start, shut down
internal/api/         HTTP handlers against the generated strict server; no business decisions
internal/domain/      the reservation lifecycle; no pgx, no amqp, no otel (CI enforces this)
internal/infra/       the Postgres store, raw SQL, SQLSTATE translation
internal/events/      the BookingCancelled consumer
internal/sweeper/     the TTL sweep
internal/config/      env → typed struct, validated once at startup
migrations/           sequential SQL, applied by CI, never by the app at boot
```

`internal/domain` declares the interfaces it needs (`Store`, `Clock`, `IDGen`) and `internal/infra`
implements them. The interface belongs to the consumer, which is what keeps the domain free of
drivers and unit-testable with plain fakes.

## Running it

```bash
docker compose up            # from the repo root: Postgres + RabbitMQ + services
```

Standalone:

```bash
export DATABASE_URL='postgres://booking:booking_dev_pw@localhost:5432/availability'
export RABBITMQ_URL='amqp://booking:booking_dev_pw@localhost:5672'
go run ./cmd/availability
```

Migrations are applied by CI, not by the service at boot.

### Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `DATABASE_URL` | *required* | This service's own database. Startup fails without it. |
| `RABBITMQ_URL` | *required* | Broker. Startup fails without it. |
| `HTTP_ADDR` | `:8080` | Listen address. |
| `HTTP_SHUTDOWN_TIMEOUT` | `15s` | Grace period for in-flight requests. |
| `BOOKING_HOLD_TTL_SECONDS` | `900` | How long an unconfirmed hold keeps its window. Booking reads the same variable for `BookingHeld.hold_expires_at`; the two must agree. |
| `AVAILABILITY_SWEEP_INTERVAL_SECONDS` | `30` | How often the TTL sweeper runs. Must be shorter than the hold TTL, or a lapsed hold stays unbookable for nearly twice its TTL — startup rejects it otherwise. |
| `AVAILABILITY_SWEEP_BATCH_SIZE` | `100` | Holds released per sweep transaction. The sweeper drains the backlog across several. |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. |

### Probes

- `GET /healthz` — liveness. Checks **nothing** on purpose. Failing it kills the container, and a
  service that reports itself unhealthy because Postgres blinked restarts into the same
  unreachable Postgres.
- `GET /readyz` — readiness. Checks Postgres and the broker connection. Failing it removes the
  instance from the load balancer but leaves it running to recover.

## Tests

```bash
go test ./...                      # unit — fast, no Docker
go test -tags contract ./...       # responses validated against the OpenAPI spec; no Docker
go test -tags integration ./...    # real Postgres + RabbitMQ via testcontainers; needs Docker
go test -race ./...                # mandatory: this service's whole point is concurrency
```

The tagged suites are excluded from a plain `go test ./...`, so the default run never silently
depends on Docker.

**The concurrency proof lives in `internal/infra/store_integration_test.go`.** It fires N
simultaneous reserves at one window and asserts that exactly one wins — against a real Postgres,
because a fake repository backed by a map would cheerfully "prove" a guarantee only the database
is actually providing.

## Known deviations

- **No auth.** The spec declares `bearerAuth`, but no gateway issues tokens yet. Verification
  lands with the gateway in M3; until then this service trusts its internal-only network
  (ADR-0006). Flagged deliberately, not overlooked.
- **`releaseReservation` with no body** defaults to `booking_cancelled`. The spec marks the body
  `required: false` but says nothing about what its absence means; raised as a contract-change
  candidate rather than silently settled.
- **No documented 5xx.** No operation in the spec declares a 5xx response, so a genuine fault
  returns a body that matches the `Error` schema under a status the spec does not list. Also
  raised as a contract-change candidate.
- **`404 resource_not_found` on `listSlots` is never returned.** Resources are owned by catalog,
  and checking one would mean querying another service's data (hard rule #6) on the read path.

## Decisions

[ADR-0002](../../docs/adr/0002-async-messaging-rabbitmq-outbox.md) ·
[ADR-0003](../../docs/adr/0003-no-double-booking-exclusion-constraint-saga.md) ·
[ADR-0009](../../docs/adr/0009-shared-module-boundary-and-codegen.md) ·
[ADR-0010](../../docs/adr/0010-rabbitmq-topology-and-outbox-parameters.md) ·
[ADR-0011](../../docs/adr/0011-exclusion-constraint-is-partial.md)
