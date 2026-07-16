# Deferred backlog

Intentionally out of the first vertical slice. Ordered roughly by likely priority after the slice.

## Split-bill payment saga [DEFERRED]
Distributed saga: initiator, per-person vs even split, per-payer state, partial-paid, deadline,
compensation if a payer fails. Non-trivial — its own design + ADR before build. (Sales + CUST-8.)

## Marketplace / vendor context [DEFERRED — architecture keeps room]
Vendor onboarding/KYC, per-vendor calendars, commission + payout ledger. Bounded contexts are kept
clean now (ADR-0001) so this can be added without rework. (Business model: "single now, marketplace later".)

## Promotions [DEFERRED]
Promocodes and referral links bound to specific sellers; deals/campaigns. (Sales stories.)

## CRM / Customer 360 [DEFERRED]
Per-client order/booking status, activity timeline, support tooling distinct from admin.

## Admin / Ops console [DEFERRED]
Manage orders/bookings/clients/errors; operator tools to resolve client issues.

## Audit service + UI [DEFERRED]
Immutable audit log of all admin-console activity, shipped to the dedicated audit account (ADR-0005).

## Reviews & ratings [DEFERRED]
Post-trip reviews to build trust and drive sales.

## CMS content surface [DEFERRED]
Strapi/Directus-backed pages, keywords, styling, analytics (GA), SEO editing. (ADR-0004; SEO/Designer.)

## Notifications — marketing [DEFERRED]
Deal/service announcements and campaign sends (distinct from transactional order-status emails).
