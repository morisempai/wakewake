# Customer persona

The person actually booking a boat/yacht/wakesurf session. This persona was almost entirely absent
from the original stories (which were mostly ops/sales/owner), yet drives the core flow.

## CUST-1 — Browse & filter products [SLICE]
As a customer I want to browse and filter available watercraft and sessions so I can find one that fits.
**AC:**
- Given the catalog, when I filter by type (boat/yacht/wakesurf), date, and party size, then I see only matching, active products.
- Each result shows price, capacity, location, and photos.

## CUST-2 — See live availability [SLICE]
As a customer I want to see up-to-date availability for a product so I don't try to book a taken slot.
**AC:**
- Given a product and a date, when I view availability, then I see open slots reflecting current holds/confirmations (no stale data).

## CUST-3 — Place a hold and check out [SLICE]
As a customer I want to reserve a slot and pay so my booking is confirmed.
**AC:**
- Given an open slot, when I start checkout, then a hold is created with a visible expiry (e.g. 15 min).
- If I don't pay before expiry, the hold is released and the slot becomes bookable again.
- On successful payment, the booking is confirmed and the slot is no longer offered to others.

## CUST-4 — Receive booking confirmation [SLICE]
As a customer I want a confirmation (email) so I have proof and details of my booking.
**AC:**
- Given a confirmed booking, when payment succeeds, then I receive a confirmation with booking id, product, time, and price.

## CUST-5 — View my bookings [DEFERRED]
As a customer I want to see my current and past bookings and their status.

## CUST-6 — Sign in with a social account [SLICE]
As a customer I want to sign in with Google/X so registration is one click.
**AC:** Given the login page, when I choose a social provider, then I authenticate via Keycloak and get a session. (Dev: username/password against Keycloak realm; social IdPs configured in real envs.)

## CUST-7 — Cancel / reschedule my booking [DEFERRED]
As a customer I want to cancel or reschedule within policy and get the correct refund. (See BOOK rules.)

## CUST-8 — Invite friends to split the bill [DEFERRED]
As a customer I want to split a booking's cost among several people. (See deferred split-bill saga.)
