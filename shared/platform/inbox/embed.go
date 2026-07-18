package inbox

import _ "embed"

// MigrationSQL is the canonical processed_event DDL. Each consuming service pastes it into its
// own numbered migration.
//
//go:embed schema.sql
var MigrationSQL string
