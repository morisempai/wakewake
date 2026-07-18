// Package platform is the runtime substrate every service shares: correlation IDs, structured
// logging, the transactional outbox, the consumer loop, and the Postgres/AMQP plumbing.
//
// Import rule (ADR-0009): internal/domain may import shared/contracts but MUST NOT import this
// module. It carries pgx, amqp, and otel, and pulling those into a domain layer is exactly the
// layering violation the service-template skill forbids. CI enforces this with a grep.
package platform
