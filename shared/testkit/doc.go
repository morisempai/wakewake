// Package testkit provides the test harness shared by every service: a Postgres container with
// btree_gist, an AMQP container, envelope and OpenAPI assertions, and deterministic clocks.
//
// Test-only. Never import this from a production code path — it is a separate module precisely
// so testcontainers does not end up in any service's runtime dependency graph.
package testkit
