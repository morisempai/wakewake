---
name: api-contracts
description: >
  Our conventions for HTTP APIs and async events: versioning, error envelope, pagination, auth,
  naming, and event schema rules. Use this whenever creating or modifying any HTTP handler,
  route, controller, DTO, request/response type, event producer or consumer, or any
  OpenAPI/AsyncAPI file — and whenever writing code that calls another service. Also use it
  when reviewing such code. This skill explains how to IMPLEMENT AGAINST contracts; editing
  files in contracts/ is forbidden for service agents (open a contract-change issue instead).
---

# API & Event Contracts

## Ground rules

- Specs in `contracts/openapi/<service>.yaml` (OpenAPI 3.1) and
  `contracts/asyncapi/booking-events.yaml` (AsyncAPI) are the source of truth.
- Generate/validate types from specs; never hand-write a struct that drifts from the spec.
  Go: `oapi-codegen` for server types from OpenAPI, `kin-openapi` to validate real responses
  against the spec at test time (see `testing-standards`). A hand-written struct only proves
  the code agrees with itself.
- Service agents NEVER edit `contracts/`. Wrong/missing contract → issue labeled
  `contract-change` with: what's needed, why, affected consumers, compatible-or-breaking.

## HTTP conventions

- **URLs:** plural nouns, kebab-case: `/v1/deal-notes`, nesting max 1 level
  (`/v1/deals/{dealId}/notes`). No verbs in paths; actions that aren't CRUD use
  `POST /v1/deals/{id}/actions/merge`-style sub-resources.
- **Versioning:** major version in path (`/v1/`). Additive changes (new optional field,
  new endpoint) don't bump. Anything else is breaking → new version + deprecation window.
- **IDs:** UUIDv7, string-typed everywhere.
- **Timestamps:** RFC 3339 UTC, field names end in `_at` (`created_at`).
- **Field naming:** snake_case in JSON payloads. <!-- EDIT if you prefer camelCase -->
- **Pagination:** cursor-based. Request: `?limit=50&cursor=...`. Response envelope:
  `{ "data": [...], "next_cursor": "..." | null }`. limit max 200.
- **Auth:** `Authorization: Bearer <jwt>` on every route except `/healthz`, `/readyz`.
  Tenant ID comes from the token claims, NEVER from a request parameter.

## Error envelope (all non-2xx responses)

```json
{
  "error": {
    "code": "deal_not_found",
    "message": "Human-readable, no internals leaked",
    "details": [{ "field": "stage_id", "issue": "unknown value" }],
    "correlation_id": "..."
  }
}
```

- `code` is machine-readable snake_case from the spec's enum — clients switch on it, not on message.
- 400 validation / 401 unauthenticated / 403 unauthorized / 404 not found / 409 conflict /
  422 domain rule violated / 5xx never contain stack traces.

## Event conventions

- Names: `<Entity><PastTenseVerb>` — `DealStageChanged`, `ContactCreated`. Versioned in the
  payload envelope: `{ "event": "DealStageChanged", "version": 1, "id": "<uuidv7>",
  "occurred_at": "...", "correlation_id": "...", "payload": { ... } }`.
- Events describe **facts that happened**, past tense, never commands to another service.
- Consumers MUST be idempotent keyed on event `id`, and MUST ignore unknown payload fields
  (forward compatibility).
- Producers publish AFTER the local transaction commits (transactional outbox pattern).

## Calling another service

- Read its OpenAPI spec first; use only documented endpoints/fields.
- Set timeout (default 3s), retries with jitter only on idempotent calls, propagate
  `correlation_id` header.
- Prefer consuming events over synchronous calls when eventual consistency is acceptable —
  if unsure which, ask in the issue rather than defaulting to HTTP.
