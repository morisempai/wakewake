# Booking domain rules

Domain rules missing from the originals that carry real architectural weight. Only hold-expiry and
no-double-booking are in the first slice; the rest are scoped as follow-ups.

## BOOK-1 — Hold expiry (TTL) [SLICE]
As the business I want unpaid holds to auto-release so inventory isn't locked by abandoned checkouts.
**AC:** Given a `HELD` booking, when its TTL passes without payment, then the reservation is released and the slot is bookable again; the booking becomes `CANCELLED`.

## BOOK-2 — No double-booking [SLICE]
As the business I want it to be impossible for two customers to book the same resource over overlapping time.
**AC:** Given two simultaneous requests for the same resource/slot, when both are processed, then exactly one succeeds and the other is cleanly rejected. (Enforced by DB exclusion constraint — ADR-0003.)

## BOOK-3 — Cancellation & refund policy [DEFERRED]
Refund windows, cancellation fees, no-show handling; deposits partially/fully refundable per policy.

## BOOK-4 — Reschedule / modify [DEFERRED]
Move a booking to another slot subject to availability and policy, preserving payment where possible.

## BOOK-5 — Deposit / pre-authorization [DEFERRED]
Hold a deposit or pre-auth for high-value assets (yachts); capture on confirmation, release on cancel.

## BOOK-6 — Buffer / turnaround time [DEFERRED]
Enforce cleaning/refuel buffers between rentals so back-to-back slots aren't sold.

## BOOK-7 — Resource capacity & staff/skipper [DEFERRED]
A resource has a capacity (people) and may require an assigned skipper/staff member (staff scheduling).

## BOOK-8 — Weather / operational cancellation [DEFERRED]
Ops can cancel a day/slot (weather, maintenance) triggering auto-refund/reschedule and notifications.

## BOOK-9 — Waitlist [DEFERRED]
When sold out, customers can join a waitlist and be offered released slots.

## BOOK-10 — Legal gating [DEFERRED]
Liability waiver, insurance, age/boating-license verification, damage deposit before handover.

## BOOK-11 — Pricing model [DEFERRED]
Hourly / half-day / seasonal / peak / dynamic pricing; tax, currency, invoices, receipts.
