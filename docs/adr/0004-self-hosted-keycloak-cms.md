# ADR-0004: Self-hosted Keycloak for identity, Strapi/Directus for CMS

- **Status:** Proposed  <!-- agents propose; a human sets Accepted (adr-writing skill) -->
- **Date:** 2026-07-15
- **Deciders:** Architecture, Security (pending human sign-off)
- **Related:** Security stories (SSO on all UIs, OAuth for tooling, fine-grained access, vault), Business (social login), Designer/SEO (CMS)

## Context

We need social login (Google/X), SSO for internal UIs (Grafana, OpenSearch, admin console), OIDC for
dev/delivery tooling, and fine-grained RBAC — plus a CMS for content/keywords/styling. The security
stories require everything to sit behind our gateway and pull secrets from a central vault. Managed
SaaS (Auth0/Contentful) fights those isolation requirements and adds recurring per-MAU cost.

## Decision

**Self-host open-source**: **Keycloak** for identity (social IdPs, SSO, OIDC, realm roles / RBAC) and
**Strapi or Directus** for the CMS. $0 license; both run as containers behind the gateway with secrets
sourced from Vault (ADR-0005 environment isolation applies).

## Consequences

- (+) Full control, fits vault/gateway/SSO/isolation stories; no per-user SaaS fees.
- (+) One OIDC provider (Keycloak) unifies customer auth and internal SSO.
- (−) We own patching, HA, and backups for Keycloak and the CMS (ops cost, not license cost).
- CMS choice (Strapi vs Directus) finalized in a follow-up ADR once content model is drafted.

## Alternatives considered

- **Auth0/Okta (SaaS)** — rejected: recurring per-MAU cost and can't sit behind our gateway/vault as the isolation stories require.
- **AWS Cognito** — rejected: weaker SSO and fine-grained RBAC than the stories need.
- **Contentful (SaaS CMS)** — rejected: cost and external hosting conflict with self-isolation; OSS CMS covers the content/SEO needs.
- **Build auth/CMS from scratch** — rejected: re-implements mature, security-sensitive systems at high risk/effort.
