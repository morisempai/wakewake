// Package availability is the availability service.
//
// It implements contracts/openapi/availability.yaml — the contract is the source of truth; this code
// never defines it. See CLAUDE.md in this directory for scope, ownership, and events.
//
// This file carries no code. The binary is cmd/availability, and the implementation lives under
// internal/, where the compiler refuses to let another module import it — hard rule #6's data
// ownership, applied to code. Start at README.md.
package availability
