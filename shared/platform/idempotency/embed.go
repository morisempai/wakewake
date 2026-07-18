package idempotency

import _ "embed"

// MigrationSQL is the canonical idempotency_key DDL. Each service pastes it into its own
// migration and adds the DEFERRABLE foreign key to its own domain table — see the note in the
// SQL for why deferrable is required.
//
//go:embed schema.sql
var MigrationSQL string
