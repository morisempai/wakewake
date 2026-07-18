package outbox

import _ "embed"

// MigrationSQL is the canonical outbox DDL. Each producing service pastes this into its own
// numbered migration — shared/ cannot own another service's migration sequence (hard rule #6).
//
// testkit's outbox schema assertion checks a service's real table against what Enqueue writes,
// so a service that pastes a stale copy fails a test rather than failing at 3am.
//
//go:embed schema.sql
var MigrationSQL string
