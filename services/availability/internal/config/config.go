// Package config reads this service's environment ONCE at startup into a typed struct.
//
// Missing required variables fail before the server binds, with a message naming the variable
// (service-template). The alternative — defaulting DATABASE_URL to localhost — produces a
// service that starts cleanly in production and then cannot reach its database, which is a far
// harder failure to read than "DATABASE_URL is required but not set".
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/morisempai/wakewake/shared/platform/config"
)

// ServiceName labels every log line, span, and consumer queue. Passed to the shared fragments
// rather than read from the environment: a service whose logs carry another service's name is
// worse than unlabelled, because it sends an incident responder to the wrong place.
const ServiceName = "availability"

// Config is the whole of this service's configuration.
//
// The shared fragments cover what is genuinely identical everywhere (ADR-0009); everything below
// them is this service's own, so adding one does not become a gated shared-change PR.
type Config struct {
	config.Postgres
	config.AMQP
	config.HTTP
	config.Observability

	// HoldTTL is how long an unconfirmed reservation holds its window before the sweeper
	// releases it. `BOOKING_HOLD_TTL_SECONDS`, default 900 (issue #5's NFR note).
	//
	// It is named in the plural of the whole booking flow, not of this service, because Booking
	// puts the same value in BookingHeld.hold_expires_at. The two must agree, so they read the
	// same variable.
	HoldTTL time.Duration

	// SweepInterval is how often the TTL sweeper runs.
	//
	// It bounds how long a slot stays unbookable after its hold lapses: a customer refreshing
	// the calendar sees the freed window up to this late. Cheap to run — the scan is served by
	// a partial index over held rows only — so it is set well below the hold TTL rather than
	// near it.
	SweepInterval time.Duration

	// SweepBatchSize caps how many holds one sweep pass releases, so a large backlog cannot hold
	// one long transaction open.
	SweepBatchSize int
}

// Load reads and validates the environment.
func Load() (Config, error) {
	pg, err := config.LoadPostgres()
	if err != nil {
		return Config{}, err
	}
	amqp, err := config.LoadAMQP()
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Postgres:      pg,
		AMQP:          amqp,
		HTTP:          config.LoadHTTP(),
		Observability: config.LoadObservability(ServiceName),

		HoldTTL:        config.SecondsVar("BOOKING_HOLD_TTL_SECONDS", 15*time.Minute),
		SweepInterval:  config.SecondsVar("AVAILABILITY_SWEEP_INTERVAL_SECONDS", 30*time.Second),
		SweepBatchSize: intOr("AVAILABILITY_SWEEP_BATCH_SIZE", 100),
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validate rejects values that would make the service misbehave silently rather than loudly.
func (c Config) validate() error {
	if c.HoldTTL <= 0 {
		return fmt.Errorf("config: BOOKING_HOLD_TTL_SECONDS must be positive, got %s", c.HoldTTL)
	}
	if c.SweepInterval <= 0 {
		return fmt.Errorf("config: AVAILABILITY_SWEEP_INTERVAL_SECONDS must be positive, got %s", c.SweepInterval)
	}
	if c.SweepBatchSize <= 0 {
		return fmt.Errorf("config: AVAILABILITY_SWEEP_BATCH_SIZE must be positive, got %d", c.SweepBatchSize)
	}
	// A sweep interval at or beyond the hold TTL means a lapsed hold can sit unbookable for
	// roughly twice its TTL. That is a configuration mistake, not a tuning choice.
	if c.SweepInterval >= c.HoldTTL {
		return fmt.Errorf(
			"config: AVAILABILITY_SWEEP_INTERVAL_SECONDS (%s) must be shorter than BOOKING_HOLD_TTL_SECONDS (%s), "+
				"or expired holds stay unbookable for nearly twice their TTL", c.SweepInterval, c.HoldTTL)
	}
	return nil
}

func intOr(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
