---
name: cicd-conventions
description: >
  Pipeline structure, deployment rules, and how to work with CI in this project. Use this
  whenever writing or modifying GitHub Actions workflows, diagnosing a failed CI run or red
  check on a PR, adding a service to the build matrix, working with Docker image builds/tags,
  touching IaC (Terraform/Helm) files, or anything involving deployment, releases, or
  environments. Also defines the ONLY permitted path to the cloud — read it before any
  action that could touch infrastructure.
---

# CI/CD Conventions

## Prime directive

The pipeline is the only entity that touches the cloud. Agents and humans deploy by
merging; nothing else. If you find yourself wanting cloud credentials, you are about
to violate Hard rule 3 — stop and re-read it.

## Pipeline stages (per service, path-filtered)

```
lint → unit tests → contract tests → build image → integration tests
     → [merge to main] → push image → deploy staging (auto)
     → smoke tests → deploy production (manual approval)
```

- Workflows live in `.github/workflows/` (protected — service agents propose changes via
  `shared-change` issues, never edit directly).
- Each service builds only when `services/<name>/**` or its contracts change (`paths:` filters).
- Cross-cutting checks on every PR: scope-check (branch may only touch its service),
  contract-guardian (fires on `contracts/**`), Claude reviewer.

## Conventions

- **Images:** `ghcr.io/<org>/crm-<service>:<git-sha>` — immutable, sha-tagged; `latest` is
  never deployed. Staging and production run the same image, differing only in config.
  <!-- EDIT registry -->
- **IaC:** Terraform in `infra/`, Helm values per environment in `deploy/`. Changing
  infrastructure = PR to these files; the pipeline plans on PR (posted as comment) and
  applies on merge. Agents may READ these to understand environments.
- **Environments:** `staging` auto-deploys from main; `production` requires manual approval
  (GitHub environment protection). Never propose weakening these gates.
- **Rollback:** re-deploy previous sha via the pipeline's rollback workflow — never manual.
- **Secrets:** GitHub Environments / OIDC to cloud. A secret value appearing in a workflow
  file, log, or echo is an incident — flag it immediately.

## When a CI run fails on your PR

1. Read the actual failing step's log (`gh run view <id> --log-failed`) — don't guess from
   the check name.
2. Classify: your code (fix it) / flaky test (rerun once; if it passes, open a flake issue —
   do NOT delete the test) / infrastructure or shared workflow (open `shared-change` issue,
   link the run).
3. Never bypass: no `--no-verify`, no commenting out checks, no force-push over red history.

## Adding a service to CI

Via `shared-change` issue containing the exact matrix entry + paths filter needed.
Template: copy an existing service's entries; every service is built, tested, and
deployed identically — snowflake pipelines require an ADR.
