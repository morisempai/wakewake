# User Stories

Versioned, reviewable stories. The originals live in `/user_stories.txt`; this directory expands
them into per-persona stories with acceptance criteria, and adds the gaps found during architecture.

## Conventions

- One file per persona/theme. Each story has an ID (`CUST-1`, `BOOK-3`, …) and acceptance criteria.
- **[SLICE]** tags a story included in the first thin vertical slice (see the architecture plan).
- **[DEFERRED]** tags work intentionally out of the first slice (see `deferred-backlog.md`).
- Every story maps to at least one ADR and, when built, a GitHub issue + PR.
- **NFR notes** and **Contract impact** sections are mandatory on every story (user-stories skill).
  Project-wide NFR defaults live in `docs/nfr.md`.

## Naming: these files vs. the skill's format

The user-stories skill specifies `docs/stories/<epic>/<story-id>-<slug>.md` with **story ID = GitHub
issue number**. The files here predate any issues existing, so they use provisional IDs
(`CUST-1`, `BOOK-2`, …) grouped by persona. When a story is picked up, it gets an issue and moves to
the skill's layout, keeping the provisional ID as an alias in the file so links don't rot.

## Files

- `customer-persona.md` — the end customer (was largely missing from the originals).
- `booking-domain-rules.md` — cancellation, reschedule, holds, weather, capacity, legal, etc.
- `non-functional-additions.md` — idempotency, GDPR/PCI, RTO, rate limiting, i18n/a11y.
- `deferred-backlog.md` — split-bill, marketplace/vendor, promotions, reviews, CRM, audit UI.

## Slice scope (first end-to-end flow)

`browse catalog → check availability → hold → pay → confirm → notify`
Stories: CUST-1, CUST-2, CUST-3, CUST-4, CUST-6, BOOK-1 (hold TTL), BOOK-2 (no double-book).
