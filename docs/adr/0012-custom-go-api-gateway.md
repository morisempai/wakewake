# ADR-0012: Custom Go API gateway

**Status:** Accepted
**Date:** 2026-07-26
**Deciders:** morisempai
**Related:** ADR-0006 (sole-ingress principle — this finalizes its deferred technology choice),
ADR-0008 (Go for all services), ADR-0007 (OpenTelemetry observability), ADR-0009 (shared/ boundary)

## Context

ADR-0006 committed to a **single external ingress** — every external call passes through one
gateway; services sit on an internal network with no public route and trust only the gateway. It
deliberately deferred the *technology*: "Kong / APISIX / Traefik — finalized in follow-up." M3
needs that finalized, and this ADR is the follow-up.

Forces bearing on the choice:

- **Go-first (ADR-0008).** The stack was just ratified as Go for strict syntax, execution speed,
  and resource efficiency. A second runtime + config language at the edge cuts against that.
- **One logging vocabulary (ADR-0007, M4).** Every service logs a correlation/trace key that
  Grafana's Loki datasource extracts with a derived-field regex. The gateway mints the correlation
  id for the whole request; if it spells or emits that key differently from the services, trace↔log
  correlation breaks with green CI. The safest guarantee is that the gateway logs through the
  **same `shared/platform`** the services do.
- **An existing token→claims contract.** Services already extract the JWT `sub` in their own
  `auth.go` and trust an upstream verifier (ADR-0006). The gateway's validation and claim
  pass-through must match what the services already read — no plugin impedance mismatch.
- **Deployment model.** Distroless per-service containers behind compose networking (see the
  service Dockerfiles + root compose). A gateway that fits this mold adds no new operational shape.

## Decision

Implement the gateway as a **custom Go service** under `services/gateway/`. It:

1. Is the **only host-published component**; catalog/availability/booking/payment/notification are
   reachable only on the internal compose network.
2. **Validates Keycloak-issued JWTs** against the `booking` realm's JWKS — signature, issuer, and
   expiry (with bounded clock skew). Missing/invalid/expired tokens get `401` using the **shared
   Error envelope**; the body never leaks validation internals.
3. **Mints a correlation id** (UUIDv7) when `X-Correlation-Id` is absent and propagates it
   unchanged downstream, logging via `shared/platform` so the key is identical to the services.
4. Applies **basic per-principal rate limiting** (token bucket keyed on `sub`, falling back to
   client IP); a breach returns `429` with the shared envelope.
5. **Reverse-proxies by path prefix** (`net/http/httputil.ReverseProxy`) to the internal services:
   `/v1/products*` → catalog, `/v1/resources*` + `/v1/reservations*` → availability,
   `/v1/bookings*` → booking, `/v1/payments*` → payment.
6. Routes the **Stripe webhook** `POST /v1/webhooks/stripe` → payment **without JWT auth**: it is
   authenticated by Stripe signature downstream (ADR: payment webhook), and a user JWT is neither
   present nor meaningful on a Stripe-originated call. This is the one deliberately public,
   unauthenticated route.

It reuses `shared/platform` (correlation, logging, health, httpx) per ADR-0009, carries **no
database and no business logic**, and ships as a distroless image like the other services.

## Consequences

- (+) One toolchain, one deploy model, one logging vocabulary — the gateway's correlation/trace key
  matches the services **by construction**, so M4's Loki derived-field works uniformly.
- (+) Full control over the JWT→`sub` contract the services already depend on.
- (+) The surface is small and well-trodden: `httputil.ReverseProxy` + a JWKS validator (a small
  OIDC library) + an in-memory token-bucket limiter.
- (−) We own edge concerns an off-the-shelf gateway gives for free. **Rate limiting is in scope
  now; WAF / bot protection is explicitly deferred** and must be tracked before production.
- (−) JWKS key rotation, clock-skew tolerance, and audience validation are ours to get right —
  pinned by tests rather than a vendor's battle-tested plugin.
- (−) This is not a mesh: service-to-service auth (mTLS) stays deferred (ADR-0006); the gateway is
  the only trust boundary for now. Compose networking, not the services, enforces internal-only.

## Alternatives considered

- **Kong / APISIX** — full-featured (WAF, rate limiting, OIDC plugins) but a heavy runtime plus its
  own datastore and a config DSL; OIDC plugins are frequently enterprise-gated. A second
  operational model for capabilities we do not yet need.
- **Traefik** — lightweight and config-driven, but community-edition JWT/OIDC validation is thin;
  realistically it delegates auth to a Go "forward-auth" validator — i.e. Go code *plus* a second
  runtime, the worst of both.
- **Custom Go (chosen)** — most consistent with ADR-0008 and the ADR-0007 logging contract. The
  edge features forgone (WAF) are not yet required and are tracked for when they are.
