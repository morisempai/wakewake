// Package config reads this service's environment ONCE at startup into a typed struct.
//
// Missing required variables fail before the server binds, with a message naming the variable
// (service-template). The alternative — defaulting DATABASE_URL to localhost — produces a service
// that starts cleanly in production and then cannot reach its database, which is a far harder
// failure to read than "DATABASE_URL is required but not set".
package config

import (
	"github.com/morisempai/wakewake/shared/platform/config"
)

// ServiceName labels every log line and span. Passed to the shared fragments rather than read from
// the environment: a service whose logs carry another service's name is worse than unlabelled,
// because it sends an incident responder to the wrong place.
const ServiceName = "catalog"

// Config is the whole of this service's configuration.
//
// There is deliberately no AMQP fragment: catalog publishes and consumes no events in this slice
// (see services/catalog/CLAUDE.md), so requiring RABBITMQ_URL would fail startup over a dependency
// the service never opens. The shared fragments cover what is genuinely identical everywhere
// (ADR-0009); catalog adds nothing of its own here — a read-mostly catalog has no tunable knobs
// beyond how to reach its database and where to listen.
type Config struct {
	config.Postgres
	config.HTTP
	config.Observability
}

// Load reads and validates the environment. It fails loudly on a missing DATABASE_URL rather than
// defaulting it, which is the service-template rule.
func Load() (Config, error) {
	pg, err := config.LoadPostgres()
	if err != nil {
		return Config{}, err
	}

	return Config{
		Postgres:      pg,
		HTTP:          config.LoadHTTP(),
		Observability: config.LoadObservability(ServiceName),
	}, nil
}
