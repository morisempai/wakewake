// Package booking is the booking service.
//
// It implements contracts/openapi/booking.yaml and orchestrates the hold→pay→confirm saga — the
// contracts are the source of truth; this code never defines them. See CLAUDE.md in this directory
// for scope, ownership, and events.
//
// This file carries no code. The binary is cmd/booking, and the implementation lives under
// internal/, where the compiler refuses to let another module import it — hard rule #6's data
// ownership, applied to code. Start at README.md.
package booking
