# ADR-0006: API gateway as the sole external ingress

- **Status:** Proposed  <!-- agents propose; a human sets Accepted (adr-writing skill) -->
- **Date:** 2026-07-15
- **Deciders:** Security, Architecture (pending human sign-off)
- **Related:** Security story (external API only through dedicated gateway, no direct service access); ADR-0001

## Context

Security requires that all external API calls go through a dedicated gateway with no direct access to
services. We also want a single place for authn (OIDC token validation), rate limiting, WAF/bot
protection, and request correlation.

## Decision

Introduce an **API gateway** (Kong / APISIX / Traefik — finalized in follow-up) as the only publicly
routable component. Services are placed on an internal network with **no public ingress**; they trust
requests only from the gateway. The gateway validates Keycloak OIDC tokens, injects/propagates the
correlation ID, and applies rate limiting. In dev, compose networking enforces the same boundary.

## Consequences

- (+) Single, auditable choke point for authn, rate limiting, and correlation.
- (+) Services need not each re-implement edge concerns.
- (−) Gateway is a critical path → must be HA and observed (SLOs apply).
- (−) Internal service-to-service auth still required (defense in depth) — mTLS/JWT considered separately.

## Alternatives considered

- **Per-service public ingress + auth in each service** — rejected: duplicates edge concerns and violates the "no direct service access" story.
- **Service mesh ingress (Istio/Linkerd) only** — deferred: heavier to operate at current scale; a gateway meets the requirement now, mesh can be layered later.
