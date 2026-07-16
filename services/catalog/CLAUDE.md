# Service: catalog

> Inherits all root rules in `/CLAUDE.md`. This file scopes them to this service.

## Write scope
You may only create/edit files under `services/catalog/`. For changes to `contracts/`, `shared/`,
`.github/`, or other services: open a GitHub issue (`contract-change` / `shared-change`), reference it
in your PR, and stop dependent work.

## Responsibility
Products the business sells/rents: boats, yachts, wakesurf sessions. Read-mostly. Filtering by type,
date, party size, location.

## Owns
- Database: `catalog` (exclusive). Tables for products, product types, media refs, pricing display.
- No other service reads these tables — expose via API/events only.

## Implements (contracts — source of truth)
- HTTP: `contracts/openapi/catalog.yaml`
- Events published: (none in slice)

## Consumes
- Nothing in the slice.

## Notes
- All timestamps UTC. Prices are display-only here; authoritative charge is Payment's concern.
