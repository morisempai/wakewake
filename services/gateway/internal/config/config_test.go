package config_test

import (
	"testing"
	"time"

	"github.com/morisempai/wakewake/services/gateway/internal/config"
)

// setRequired sets every mandatory variable to a valid value so a test can then clear or override
// exactly the one it is exercising.
func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("OIDC_ISSUER", "https://issuer.test/realms/booking")
	t.Setenv("OIDC_JWKS_URL", "http://keycloak:8080/realms/booking/protocol/openid-connect/certs")
	t.Setenv("CATALOG_BASE_URL", "http://catalog:8080")
	t.Setenv("AVAILABILITY_BASE_URL", "http://availability:8080")
	t.Setenv("BOOKING_BASE_URL", "http://booking:8080")
	t.Setenv("PAYMENT_BASE_URL", "http://payment:8080")
}

func TestLoad_DefaultsAndSplit(t *testing.T) {
	setRequired(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}

	// The issuer string and the JWKS URL are read from separate variables and are NOT required to
	// match — this is the docker gotcha the gateway must handle.
	if cfg.OIDCIssuer == cfg.OIDCJWKSURL {
		t.Fatalf("issuer and JWKS URL should be independent; both are %q", cfg.OIDCIssuer)
	}
	if cfg.OIDCIssuer != "https://issuer.test/realms/booking" {
		t.Errorf("OIDCIssuer = %q", cfg.OIDCIssuer)
	}

	if cfg.Upstreams.Catalog != "http://catalog:8080" {
		t.Errorf("Upstreams.Catalog = %q", cfg.Upstreams.Catalog)
	}
	if cfg.Upstreams.Payment != "http://payment:8080" {
		t.Errorf("Upstreams.Payment = %q", cfg.Upstreams.Payment)
	}

	if cfg.ClockSkew != 30*time.Second {
		t.Errorf("ClockSkew default = %v, want 30s", cfg.ClockSkew)
	}
	if cfg.RateLimit.RPS != 50 {
		t.Errorf("RateLimit.RPS default = %v, want 50", cfg.RateLimit.RPS)
	}
	if cfg.RateLimit.Burst != 100 {
		t.Errorf("RateLimit.Burst default = %v, want 100", cfg.RateLimit.Burst)
	}
}

func TestLoad_Overrides(t *testing.T) {
	setRequired(t)
	t.Setenv("OIDC_CLOCK_SKEW", "5s")
	t.Setenv("RATE_LIMIT_RPS", "250")
	t.Setenv("RATE_LIMIT_BURST", "500")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg.ClockSkew != 5*time.Second {
		t.Errorf("ClockSkew = %v, want 5s", cfg.ClockSkew)
	}
	if cfg.RateLimit.RPS != 250 {
		t.Errorf("RPS = %v, want 250", cfg.RateLimit.RPS)
	}
	if cfg.RateLimit.Burst != 500 {
		t.Errorf("Burst = %v, want 500", cfg.RateLimit.Burst)
	}
}

func TestLoad_MissingRequired(t *testing.T) {
	for _, missing := range []string{
		"OIDC_ISSUER", "OIDC_JWKS_URL",
		"CATALOG_BASE_URL", "AVAILABILITY_BASE_URL", "BOOKING_BASE_URL", "PAYMENT_BASE_URL",
	} {
		t.Run(missing, func(t *testing.T) {
			setRequired(t)
			t.Setenv(missing, "")

			if _, err := config.Load(); err == nil {
				t.Fatalf("Load: expected an error when %s is unset, got nil", missing)
			}
		})
	}
}
