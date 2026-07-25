// Package config reads this service's environment ONCE at startup into a typed struct.
//
// Missing required variables fail before the server binds, with a message naming the variable
// (service-template). For payment that includes the two Stripe secrets: a service that starts
// without them would accept a createPayment and then fail the charge, or accept a webhook it cannot
// verify — both worse than failing loudly at boot.
//
// Secrets (the Stripe secret key and webhook signing secret) are read here and NEVER logged. This
// package's error messages name the missing variable but never print a value.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/morisempai/wakewake/shared/platform/config"
)

// ServiceName labels every log line, span, and consumer queue.
const ServiceName = "payment"

// Config is the whole of this service's configuration.
type Config struct {
	config.Postgres
	config.AMQP
	config.HTTP
	config.Observability

	Stripe Stripe
}

// Stripe holds the payment provider settings. The two secrets come from the environment only —
// never hardcoded, never logged (service CLAUDE.md, PCI).
type Stripe struct {
	// SecretKey authenticates to Stripe's API (sk_test_... in dev). Secret.
	SecretKey string

	// WebhookSecret verifies the Stripe-Signature on inbound webhooks (whsec_...). Secret. Without
	// it, a webhook body is attacker-controlled input that could mark any booking paid.
	WebhookSecret string

	// BaseURL is Stripe's API base. Overridable so a local mock or a test can point elsewhere;
	// production leaves it at the default. Never a route to a real charge in tests.
	BaseURL string

	// WebhookTolerance is how much clock skew a webhook timestamp may have before it is rejected as
	// a possible replay. Default 300s, matching Stripe's own default.
	WebhookTolerance time.Duration
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

		Stripe: Stripe{
			SecretKey:        strings.TrimSpace(os.Getenv("STRIPE_SECRET_KEY")),
			WebhookSecret:    strings.TrimSpace(os.Getenv("STRIPE_WEBHOOK_SECRET")),
			BaseURL:          stringOr("STRIPE_BASE_URL", "https://api.stripe.com"),
			WebhookTolerance: config.SecondsVar("STRIPE_WEBHOOK_TOLERANCE_SECONDS", 5*time.Minute),
		},
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validate rejects values that would make the service misbehave silently. It names the offending
// variable but NEVER prints its value — a secret in a startup error is still a secret in a log.
func (c Config) validate() error {
	if c.Stripe.SecretKey == "" {
		return fmt.Errorf("config: STRIPE_SECRET_KEY is required but not set")
	}
	if c.Stripe.WebhookSecret == "" {
		return fmt.Errorf("config: STRIPE_WEBHOOK_SECRET is required but not set")
	}
	if strings.TrimSpace(c.Stripe.BaseURL) == "" {
		return fmt.Errorf("config: STRIPE_BASE_URL must not be empty")
	}
	if c.Stripe.WebhookTolerance <= 0 {
		return fmt.Errorf("config: STRIPE_WEBHOOK_TOLERANCE_SECONDS must be positive")
	}
	return nil
}

func stringOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
