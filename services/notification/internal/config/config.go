// Package config reads this service's environment ONCE at startup into a typed struct.
//
// Missing required variables fail before anything binds, with a message naming the variable
// (service-template). Defaulting a broker or database URL to localhost would produce a service
// that starts cleanly in production and then cannot do its job, which is a far harder failure to
// read than "RABBITMQ_URL is required but not set".
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/morisempai/wakewake/shared/platform/config"
)

// ServiceName labels every log line, span, and consumer queue. Passed to the shared fragments
// rather than read from the environment: a service whose logs carry another service's name sends
// an incident responder to the wrong place.
const ServiceName = "notification"

// Config is the whole of this service's configuration.
type Config struct {
	config.Postgres
	config.AMQP
	config.HTTP
	config.Observability

	// SMTP is the outbound mail relay. In dev this is Mailhog (see infra/docker-compose.yml and
	// .env.example: SMTP_HOST=mailhog, SMTP_PORT=1025).
	SMTP SMTP

	// FromAddress is the envelope-from on every message. `NOTIFICATION_FROM_ADDRESS`, default a
	// reserved-TLD no-reply so a misconfigured relay cannot mail a real person.
	FromAddress string
}

// SMTP is the mail relay connection.
type SMTP struct {
	Host string
	Port int
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

	port, err := intVar("SMTP_PORT", 1025)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Postgres:      pg,
		AMQP:          amqp,
		HTTP:          config.LoadHTTP(),
		Observability: config.LoadObservability(ServiceName),

		SMTP: SMTP{
			Host: stringOr("SMTP_HOST", "mailhog"),
			Port: port,
		},
		FromAddress: stringOr("NOTIFICATION_FROM_ADDRESS", "no-reply@bookings.example.test"),
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validate rejects values that would make the service misbehave silently rather than loudly.
func (c Config) validate() error {
	if strings.TrimSpace(c.SMTP.Host) == "" {
		return fmt.Errorf("config: SMTP_HOST must not be empty")
	}
	if c.SMTP.Port < 1 || c.SMTP.Port > 65535 {
		return fmt.Errorf("config: SMTP_PORT must be in 1..65535, got %d", c.SMTP.Port)
	}
	if strings.TrimSpace(c.FromAddress) == "" {
		return fmt.Errorf("config: NOTIFICATION_FROM_ADDRESS must not be empty")
	}
	return nil
}

func stringOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// intVar parses an integer variable, returning an error (rather than silently defaulting) when it
// is set but unparseable — a typo'd SMTP_PORT should fail loudly at startup, not fall back to a
// port that quietly reaches nothing.
func intVar(key string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be an integer, got %q", key, v)
	}
	return n, nil
}
