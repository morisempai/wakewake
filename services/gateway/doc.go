// Package gateway is the API gateway service.
//
// It is the sole external ingress (ADR-0006, ADR-0012): it validates Keycloak JWTs against the
// realm JWKS, mints and propagates the correlation id, rate-limits, and reverse-proxies by path
// prefix to the internal services. It owns no database and holds no business logic — routing,
// authentication, rate limiting, and correlation only. See CLAUDE.md in this directory for scope.
//
// This file is intentionally empty of logic: the implementation lives under internal/.
package gateway
