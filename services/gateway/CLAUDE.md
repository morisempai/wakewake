# Service: gateway

> Inherits all root rules in `/CLAUDE.md`. This file scopes them to this service.

## Write scope
Only files under `services/gateway/`. Contract/shared/other-service changes → GitHub issue + stop.

## Responsibility
The SOLE external ingress (ADR-0006). Validates Keycloak OIDC tokens, applies rate limiting, generates
and propagates the correlation id, and routes to internal services. Services are not publicly routable.

## Owns
- No database. Configuration and routing only.

## Implements (contracts — source of truth)
- Routes defined against the per-service OpenAPI specs in `contracts/openapi/`.

## Non-negotiable
- No business logic here — routing, authn, rate limiting, correlation only.
- A request without a correlation id gets one minted here; downstream must propagate it unchanged.
- Internal services must never be reachable except through this gateway (enforced by compose/network).
