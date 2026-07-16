# ADR-0005: Per-environment AWS account isolation, managed by Terraform

- **Status:** Proposed  <!-- agents propose; a human sets Accepted (adr-writing skill) -->
- **Date:** 2026-07-15
- **Deciders:** Security, SRE (pending human sign-off)
- **Related:** Security stories (separate cloud accounts per env, dedicated audit account, vault, fine-grained access), Reliability (IaC, arbitrary environments)

## Context

Security stories require each environment (dev, prod) to run in isolated cloud infrastructure with a
**dedicated audit account**, all access via OAuth/OIDC, fine-grained permissions, and secrets in a
central vault. The user confirmed: **separate accounts on a single provider (AWS)** rather than truly
different providers — strong blast-radius isolation without multiplying toolchains/skills.

## Decision

Use **separate AWS accounts per environment** (`dev`, `prod`, plus a dedicated **audit** account),
organized under AWS Organizations, all provisioned by **Terraform** (with Ansible for config where
needed). Human and machine access is federated through Keycloak/OIDC; secrets come from Vault.
Terraform modules are parameterized so arbitrary environments can be stood up on demand.

## Consequences

- (+) Hard isolation between dev and prod; audit logs land in an account no workload can tamper with.
- (+) Reproducible, arbitrary environments via IaC; single cloud skill set and toolchain.
- (−) Cross-account IAM and Organizations setup is non-trivial; requires guardrails (SCPs).
- (−) One-provider dependency accepted as a deliberate tradeoff vs multi-cloud cost/complexity.
- No cloud CLI is ever run by hand — deployment happens only through CI/CD (per root rules).

## Alternatives considered

- **Truly multi-cloud (different provider per env)** — rejected: multiplies IaC, skills, and portability constraints for isolation we get from separate accounts; user confirmed one provider.
- **Single account, separate VPCs per env** — rejected: weaker blast-radius isolation and no clean home for a tamper-proof audit account.
- **Ansible-only (no Terraform)** — rejected: Terraform's state/plan model fits cloud resource provisioning better; Ansible retained for config management.
