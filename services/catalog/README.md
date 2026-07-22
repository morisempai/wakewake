# catalog

The read-mostly product catalog. Owns product identity and display pricing for the things the
business rents — boats, yachts, wakesurf sessions — and serves them over a small HTTP API with
filtering and cursor pagination.

Catalog does **not** own availability (that is the availability service's exclusion constraint) and
its prices are **indicative**: Payment computes the authoritative charge. A product's `resource_id`
is the handle availability reserves against; catalog only records it.

## Contracts (source of truth)

This service implements this contract; it never defines it.

| Kind | File |
| --- | --- |
| HTTP | [`contracts/openapi/catalog.yaml`](../../contracts/openapi/catalog.yaml) |

- **Publishes:** nothing in this slice.
- **Consumes:** nothing in this slice.

Because it neither publishes nor consumes events, catalog opens no broker connection and requires
no `RABBITMQ_URL` — the one structural difference from the other services.

## Owns

The `catalog` database, exclusively. No other service may read these tables (hard rule #6).

| Table | Purpose |
| --- | --- |
| `product` | Product identity, capacity, location, and display price. |

## Endpoints

| Method | Path | Notes |
| --- | --- | --- |
| `GET` | `/v1/products` | Newest-first, cursor-paginated. Filters: `type`, `min_capacity`, `location`. |
| `GET` | `/v1/products/{product_id}` | One product; `404 product_not_found` when absent. |
| `GET` | `/healthz` | Liveness. Checks **nothing** on purpose. |
| `GET` | `/readyz` | Readiness. Checks Postgres. |

### Pagination

`GET /v1/products` orders by `created_at DESC, id DESC` and pages with an **opaque keyset cursor**,
not an offset. The cursor names the exact `(created_at, id)` of the last row on the page, so a
product inserted ahead of the boundary between two page fetches does not shift the window — no row
is served twice and none is skipped. `next_cursor` is `null` on the last page.

### Filters

- **`type`** — one of `boat`, `yacht`, `wakesurf_session`. Any other value is `400 validation_failed`.
- **`min_capacity`** — products seating **at least** this many people (the party-size filter).
- **`location`** — exact-match location slug, e.g. `lake-geneva`.

## Layout

```
cmd/catalog/       bootstrap only — read config, wire, start, shut down
internal/api/      HTTP handlers against the generated strict server; no business decisions
internal/domain/   the product read model, filters, and the opaque cursor; no pgx (CI enforces this)
internal/infra/    the Postgres store, raw SQL, keyset query
internal/config/   env → typed struct, validated once at startup
migrations/        sequential SQL, applied by CI, never by the app at boot
```

`internal/domain` declares the interface it needs (`Store`) and `internal/infra` implements it. The
interface belongs to the consumer, which is what keeps the domain free of drivers and
unit-testable with a plain fake.

## Running it

```bash
docker compose up            # from the repo root: Postgres + services
```

Standalone:

```bash
export DATABASE_URL='postgres://booking:booking_dev_pw@localhost:5432/catalog'
go run ./cmd/catalog
```

Migrations are applied by CI, not by the service at boot. Catalog has no write endpoint in this
slice; rows arrive by seed (see the integration tests) or an out-of-band admin path that is out of
scope here.

### Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `DATABASE_URL` | *required* | This service's own database. Startup fails without it. |
| `HTTP_ADDR` | `:8080` | Listen address. |
| `HTTP_SHUTDOWN_TIMEOUT` | `15s` | Grace period for in-flight requests. |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. |

### Probes

- `GET /healthz` — liveness. Checks **nothing** on purpose. Failing it kills the container, and a
  service that reports itself unhealthy because Postgres blinked restarts into the same unreachable
  Postgres.
- `GET /readyz` — readiness. Checks Postgres. Failing it removes the instance from the load balancer
  but leaves it running to recover.

## Tests

```bash
go test ./...                      # unit — fast, no Docker
go test -tags contract ./...       # responses validated against the OpenAPI spec; no Docker
go test -tags integration ./...    # real Postgres via testcontainers; needs Docker
go test -race ./...                # mandatory
```

The tagged suites are excluded from a plain `go test ./...`, so the default run never silently
depends on Docker.

- **Contract tests** (`internal/api`) drive every documented response through the real router and
  validate it against `catalog.yaml` itself — including the error envelope.
- **Integration tests** (`internal/infra`) prove each filter by asserting an excluded row is
  actually absent, prove the cursor stays stable across an insert made mid-pagination, and assert
  the live `product` table matches the DDL the store scans.

## Known deviations

- **No auth.** The spec declares `bearerAuth`, but no gateway issues tokens yet. Verification lands
  with the gateway in M3; until then this service trusts its internal-only network (ADR-0006).
- **No documented 5xx.** No products operation in the spec declares a 5xx response, so a genuine
  fault returns a body that matches the `Error` schema under a status the spec does not list.
  Raised as a contract-change candidate; the contract test pins the body shape in the meantime.
- **No date filter.** The story mentions a "date availability window" filter, but `catalog.yaml`
  defines no date parameter on `listProducts`, and availability — not catalog — owns the calendar.
  Implementing one would mean inventing a query parameter the contract does not have (hard rule #2)
  and querying another service's data (hard rule #6). Flagged, not guessed; see the PR.
```
