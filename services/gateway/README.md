# gateway

The sole external ingress for the booking platform (ADR-0006, ADR-0012). A custom Go reverse proxy
that authenticates callers, mints and propagates the correlation id, rate-limits, and routes to the
internal services by path prefix. It owns no database and holds no business logic.

## What it does

- **JWT validation** — every request except the public Stripe webhook must carry a bearer token
  signed by a key in the realm JWKS, with an `iss` equal to `OIDC_ISSUER` and an unexpired `exp`
  (within a bounded clock skew). Anything else is a `401` with the shared error envelope
  (`unauthenticated`), leaking nothing about which check failed.
- **Correlation id** — an absent `X-Correlation-Id` is minted (UUIDv7) at the edge, propagated
  downstream unchanged, echoed on the response, and logged under `correlation_id` (the same key the
  services use, so Grafana's derived field links logs to traces).
- **Rate limiting** — a token bucket keyed on the token subject (`sub`), falling back to the client
  IP for the public webhook. A breach is a `429` with the shared envelope.
- **Reverse proxy** — the full path, query, method, headers, and body pass through unchanged.

## Routing table

| Prefix | Upstream | Auth |
|---|---|---|
| `/v1/products*` | catalog | JWT required |
| `/v1/resources*`, `/v1/reservations*` | availability | JWT required |
| `/v1/bookings*` | booking | JWT required |
| `/v1/payments*` | payment | JWT required |
| `POST /v1/webhooks/stripe` | payment | **public** (Stripe-signature auth is downstream) |

## Configuration (env)

| Variable | Required | Default | Notes |
|---|---|---|---|
| `HTTP_ADDR` | no | `:8080` | listen address |
| `OIDC_ISSUER` | yes | — | exact string the token `iss` must equal |
| `OIDC_JWKS_URL` | yes | — | where to fetch signing keys (**may differ from the issuer**) |
| `OIDC_CLOCK_SKEW` | no | `30s` | tolerance on `exp`/`nbf`/`iat` |
| `CATALOG_BASE_URL` | yes | — | upstream base URL |
| `AVAILABILITY_BASE_URL` | yes | — | upstream base URL |
| `BOOKING_BASE_URL` | yes | — | upstream base URL |
| `PAYMENT_BASE_URL` | yes | — | upstream base URL |
| `RATE_LIMIT_RPS` | no | `50` | tokens per second per key |
| `RATE_LIMIT_BURST` | no | `100` | bucket size |
| `LOG_LEVEL` | no | `info` | |

### Why `OIDC_ISSUER` and `OIDC_JWKS_URL` are separate

In docker, a token minted through the browser carries an `iss` of
`http://localhost:8080/realms/booking`, while the gateway reaches Keycloak over the internal network
at `http://keycloak:8080`. The issuer is the string to *match*; the JWKS URL is where the gateway
*fetches keys*. Collapsing them would reject every real token.

## Health

- `GET /healthz` — liveness, checks nothing (a liveness probe that fails on a dependency blip causes
  a crash loop).
- `GET /readyz` — readiness, fails if the JWKS holds no usable signing key.

## Build and test

```
cd services/gateway
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test -race ./...
```

`GOWORK=off` is required until the gateway is added to the repo-root `go.work` (issue #30): the
workspace was written when the gateway was still planned as an
off-the-shelf proxy (ADR-0006), which ADR-0012 supersedes. The Dockerfile already builds with
`GOWORK=off` and the module's `replace` directives, so this affects only local workspace-mode
commands.
```
docker build -f services/gateway/Dockerfile -t gateway .
```
