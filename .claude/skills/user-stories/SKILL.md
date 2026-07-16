---
name: user-stories
description: >
  How to write, structure, and file user stories and their acceptance criteria for this CRM
  project. Use this whenever creating or editing anything in docs/stories/, drafting backlog
  items or GitHub issues describing functionality, breaking an epic into stories, or when the
  user asks for requirements, features, or scope definition — even if they don't say the words
  "user story". Also use it when implementing a story, to interpret acceptance criteria correctly.
---

# User Stories

## Where stories live

`docs/stories/<epic>/<story-id>-<slug>.md`, mirrored as a GitHub issue.
The markdown file is the source of truth; the issue tracks status. Story ID = issue number.

## Story format

```markdown
# [#42] Send email when deal changes stage

**Epic:** deal-pipeline
**Service(s):** notifications (owner), deals (event producer)
**Status:** draft | approved | in-progress | done

## Story
As a sales manager, I want an email when a deal I own changes stage,
so that I can react without watching the pipeline.

## Acceptance criteria
Given a deal owned by user U in stage "Qualified"
When the deal moves to stage "Proposal"
Then U receives one email within 60 seconds containing deal name, old stage, new stage
And the notification is recorded in notification_log

Given the email provider is unavailable
When a stage-change event is consumed
Then delivery is retried per the retry policy and no event is lost

## Out of scope
- In-app notifications (separate story)
- Digest/batching

## NFR notes
Latency: ≤60s p95. Idempotent per event id (redelivery must not duplicate emails).

## Contract impact
Consumes: DealStageChanged v1 (contracts/asyncapi/crm-events.yaml). No contract changes.
```

## Rules

- **One story = one service owner.** Cross-service behavior is an epic split into per-service
  stories connected by events/contracts, plus explicit "Contract impact" sections.
- **Every AC is testable.** If you cannot imagine the automated test, rewrite the criterion.
  Vague words banned in ACs: "quickly", "properly", "user-friendly", "robust".
- **Always include the failure-path AC.** Minimum one "Given <dependency> is unavailable" scenario.
- **NFR notes are mandatory** even if the answer is "defaults apply" (see docs/nfr.md for
  project-wide defaults: availability, latency, retention). <!-- EDIT: create docs/nfr.md -->
- **Contract impact is mandatory.** "No contract changes" must be stated explicitly, not implied.
  If a story requires a contract change, it is BLOCKED until the contract PR merges — say so.
- Stories are sized to one PR. If the plan needs >1 PR, split the story first.

## When implementing a story

- Treat ACs as the test plan: each AC maps to at least one test, named after it.
- Deviations discovered mid-work (AC impossible/ambiguous/wrong) → comment on the issue and
  ask; never silently reinterpret.
