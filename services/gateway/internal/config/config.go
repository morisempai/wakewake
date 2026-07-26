// Package config reads the gateway's environment ONCE at startup into a typed struct.
//
// Missing required variables fail before the server binds, with a message naming the variable
// (service-template). The gateway has no database, so the shared Postgres/AMQP fragments do not
// apply; it reuses the shared HTTP and Observability fragments and adds its own OIDC, upstream, and
// rate-limit settings. Those are gateway-specific by nature and so live here, not in
// shared/platform/config (ADR-0009: shared owns only what is genuinely identical everywhere).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/morisempai/wakewake/shared/platform/config"
)

// ServiceName labels every log line and span. Passed to the shared fragments rather than read from
// the environment: a service whose logs carry another service's name sends an incident responder to
// the wrong place.
const ServiceName = "gateway"

// Config is the whole of the gateway's configuration.
type Config struct {
	config.HTTP
	config.Observability

	// OIDCIssuer is the exact string a token's `iss` claim must equal. It is deliberately SEPARATE
	// from OIDCJWKSURL: in docker the token's issuer is often http://localhost:8080/realms/booking
	// (the URL a browser used to log in) while the gateway reaches Keycloak over the internal
	// network at http://keycloak:8080. Collapsing the two would reject every real token.
	OIDCIssuer string

	// OIDCJWKSURL is where the gateway fetches the realm's signing keys. This is a network address
	// the gateway can actually reach, which is not necessarily the issuer string above.
	OIDCJWKSURL string

	// ClockSkew is the tolerance applied to exp/nbf/iat, so a small clock drift between Keycloak and
	// the gateway does not spuriously reject a freshly minted token.
	ClockSkew time.Duration

	Upstreams Upstreams
	RateLimit RateLimit
}

// Upstreams are the internal base URLs the gateway proxies to, one per service.
type Upstreams struct {
	Catalog      string
	Availability string
	Booking      string
	Payment      string
}

// RateLimit configures the per-subject token bucket. Defaults are generous for dev; production
// tightens them via env.
type RateLimit struct {
	RPS   float64
	Burst int
}

// Load reads and validates the environment, failing loudly on any missing required variable.
func Load() (Config, error) {
	issuer, err := required("OIDC_ISSUER")
	if err != nil {
		return Config{}, err
	}
	jwks, err := required("OIDC_JWKS_URL")
	if err != nil {
		return Config{}, err
	}

	up, err := loadUpstreams()
	if err != nil {
		return Config{}, err
	}

	return Config{
		HTTP:          config.LoadHTTP(),
		Observability: config.LoadObservability(ServiceName),
		OIDCIssuer:    issuer,
		OIDCJWKSURL:   jwks,
		ClockSkew:     durationOr("OIDC_CLOCK_SKEW", 30*time.Second),
		Upstreams:     up,
		RateLimit: RateLimit{
			RPS:   floatOr("RATE_LIMIT_RPS", 50),
			Burst: intOr("RATE_LIMIT_BURST", 100),
		},
	}, nil
}

func loadUpstreams() (Upstreams, error) {
	catalog, err := required("CATALOG_BASE_URL")
	if err != nil {
		return Upstreams{}, err
	}
	availability, err := required("AVAILABILITY_BASE_URL")
	if err != nil {
		return Upstreams{}, err
	}
	booking, err := required("BOOKING_BASE_URL")
	if err != nil {
		return Upstreams{}, err
	}
	payment, err := required("PAYMENT_BASE_URL")
	if err != nil {
		return Upstreams{}, err
	}
	return Upstreams{
		Catalog:      catalog,
		Availability: availability,
		Booking:      booking,
		Payment:      payment,
	}, nil
}

// required fetches a variable that has no safe default, failing at startup with a message naming it
// rather than defaulting to something that only breaks later.
func required(key string) (string, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return "", fmt.Errorf("config: %s is required but not set", key)
	}
	return v, nil
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

func floatOr(key string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// durationOr parses a Go duration ("30s", "1m"), falling back to def. Bare integers are rejected
// rather than guessed at, matching shared/platform/config.
func durationOr(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
