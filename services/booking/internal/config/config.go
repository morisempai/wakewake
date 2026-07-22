// Package config reads this service's environment ONCE at startup into a typed struct.
//
// Missing required variables fail before the server binds, with a message naming the variable
// (service-template). The alternative — defaulting DATABASE_URL to localhost — produces a service
// that starts cleanly in production and then cannot reach its database, which is a far harder
// failure to read than "DATABASE_URL is required but not set".
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/morisempai/wakewake/shared/platform/config"
)

// ServiceName labels every log line, span, and consumer queue. Passed to the shared fragments
// rather than read from the environment: a service whose logs carry another service's name is
// worse than unlabelled, because it sends an incident responder to the wrong place.
const ServiceName = "booking"

// Config is the whole of this service's configuration.
type Config struct {
	config.Postgres
	config.AMQP
	config.HTTP
	config.Observability

	// HoldTTL is how long an unconfirmed booking keeps its hold before it is swept. It is named
	// after the whole booking flow, not this service: Availability reads the SAME variable for the
	// reservation's expiry, and the two must agree, so Availability's expires_at and this booking's
	// hold_expires_at describe the same instant. `BOOKING_HOLD_TTL_SECONDS`, default 900.
	//
	// Booking does not itself sweep expired holds — Availability's sweeper does, emitting
	// ReservationReleased(hold_expired), which this service consumes. The value is kept here only
	// so the API response and the BookingHeld payload can report a hold_expires_at even in the rare
	// case Availability's reservation did not carry one.
	HoldTTL time.Duration

	// AvailabilityBaseURL is where the Availability HTTP API lives (createReservation,
	// confirmReservation). Internal only, not publicly routable (ADR-0006).
	AvailabilityBaseURL string

	// CatalogBaseURL is where the Catalog HTTP API lives (getProduct). Booking resolves a
	// product_id to its resource_id, capacity, and price here — catalog owns that mapping and this
	// service must never read catalog's tables directly (hard rule #6).
	CatalogBaseURL string
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

		HoldTTL: config.SecondsVar("BOOKING_HOLD_TTL_SECONDS", 15*time.Minute),

		// Service DNS names on the compose/cluster network, binding the shared HTTP default (:8080).
		// The OpenAPI `servers` URLs are contract-routing hosts, not the process bind addresses.
		AvailabilityBaseURL: stringOr("AVAILABILITY_BASE_URL", "http://availability:8080"),
		CatalogBaseURL:      stringOr("CATALOG_BASE_URL", "http://catalog:8080"),
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
	if strings.TrimSpace(c.AvailabilityBaseURL) == "" {
		return fmt.Errorf("config: AVAILABILITY_BASE_URL must not be empty")
	}
	if strings.TrimSpace(c.CatalogBaseURL) == "" {
		return fmt.Errorf("config: CATALOG_BASE_URL must not be empty")
	}
	return nil
}

func stringOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
