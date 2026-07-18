// Package config provides env-var fragments services embed in their own config struct.
//
// Deliberately NOT a god-struct. If every service's config lived here, adding one env var — the
// single most common thing a service agent does — would become a `shared-change` issue and a
// gated PR. That fails the second admission test in ADR-0009: the change trigger is service
// iteration, not a contract change.
//
// So shared owns only the fragments that are genuinely identical everywhere (how to reach
// Postgres, the broker, the collector) and each service embeds them:
//
//	type Config struct {
//	    config.Postgres
//	    config.AMQP
//	    config.HTTP
//	    HoldTTL time.Duration `env:"BOOKING_HOLD_TTL_SECONDS"`  // this service's own
//	}
//
// There is no loader here either. Services can use any mechanism they like; what matters is that
// it happens ONCE at startup and fails loudly, which is the service-template rule.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Postgres is a service's database connection settings. Each service owns its own database
// exclusively (hard rule #6), so DATABASE_URL differs per service even though the shape does not.
type Postgres struct {
	URL             string
	MaxConns        int32
	MaxConnLifetime time.Duration
}

// AMQP is the broker connection.
type AMQP struct {
	URL string
}

// HTTP is the inbound server.
type HTTP struct {
	Addr            string
	ShutdownTimeout time.Duration
}

// Observability configures logging and OTel export.
type Observability struct {
	ServiceName  string
	LogLevel     string
	OTLPEndpoint string
}

// LoadPostgres reads the Postgres fragment.
func LoadPostgres() (Postgres, error) {
	url, err := required("DATABASE_URL")
	if err != nil {
		return Postgres{}, err
	}
	return Postgres{
		URL:             url,
		MaxConns:        int32(intOr("DATABASE_MAX_CONNS", 10)),
		MaxConnLifetime: durationOr("DATABASE_MAX_CONN_LIFETIME", time.Hour),
	}, nil
}

// LoadAMQP reads the broker fragment.
func LoadAMQP() (AMQP, error) {
	url, err := required("RABBITMQ_URL")
	if err != nil {
		return AMQP{}, err
	}
	return AMQP{URL: url}, nil
}

// LoadHTTP reads the server fragment.
func LoadHTTP() HTTP {
	return HTTP{
		Addr:            stringOr("HTTP_ADDR", ":8080"),
		ShutdownTimeout: durationOr("HTTP_SHUTDOWN_TIMEOUT", 15*time.Second),
	}
}

// LoadObservability reads the observability fragment. serviceName is passed rather than read
// from env so it cannot be misconfigured — a service whose logs are labelled with another
// service's name is worse than unlabelled, because it sends you to the wrong place.
func LoadObservability(serviceName string) Observability {
	return Observability{
		ServiceName:  serviceName,
		LogLevel:     stringOr("LOG_LEVEL", "info"),
		OTLPEndpoint: stringOr("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
	}
}

// required fetches a variable that has no safe default.
//
// Returns an error rather than defaulting, so the service fails at startup with a message naming
// the variable. The alternative — defaulting to localhost — produces a service that starts
// cleanly in production and then cannot reach its database, which is a much harder failure to
// read than "DATABASE_URL is not set".
func required(key string) (string, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return "", fmt.Errorf("config: %s is required but not set", key)
	}
	return v, nil
}

func stringOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
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

// durationOr parses a Go duration ("15s", "1h"), falling back to def.
//
// Bare integers are rejected rather than guessed at: BOOKING_HOLD_TTL_SECONDS=900 means seconds
// by its name, but HTTP_SHUTDOWN_TIMEOUT=900 would be ambiguous, and silently reading one as
// nanoseconds is the kind of unit bug that only shows up under load. Services parse their own
// unit-suffixed variables explicitly.
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

// SecondsVar reads an integer-seconds variable, for the ones the project names that way
// (BOOKING_HOLD_TTL_SECONDS in .env.example).
func SecondsVar(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return time.Duration(n) * time.Second
}
