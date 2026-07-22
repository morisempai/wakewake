package config

import (
	"strings"
	"testing"
	"time"
)

// setEnv sets the variables a successful Load needs, plus any overrides. t.Setenv restores them and
// refuses to run under t.Parallel(), which is why nothing here is parallel.
func setEnv(t *testing.T, overrides map[string]string) {
	t.Helper()

	base := map[string]string{
		"DATABASE_URL": "postgres://booking@localhost:5432/booking",
		"RABBITMQ_URL": "amqp://guest:guest@localhost:5672",
	}
	for k, v := range base {
		if _, overridden := overrides[k]; !overridden {
			t.Setenv(k, v)
		}
	}
	for k, v := range overrides {
		t.Setenv(k, v)
	}
}

func TestLoadAppliesTheDocumentedDefaults(t *testing.T) {
	setEnv(t, nil)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned %v, want nil", err)
	}

	// 900 seconds — the same value Availability reads for the reservation's expiry, so
	// hold_expires_at and expires_at describe the same instant.
	if cfg.HoldTTL != 15*time.Minute {
		t.Errorf("HoldTTL = %s, want 15m", cfg.HoldTTL)
	}
	if cfg.AvailabilityBaseURL == "" || cfg.CatalogBaseURL == "" {
		t.Errorf("dependency base URLs are empty: availability=%q catalog=%q", cfg.AvailabilityBaseURL, cfg.CatalogBaseURL)
	}
	if cfg.ServiceName != ServiceName {
		t.Errorf("ServiceName = %q, want %q", cfg.ServiceName, ServiceName)
	}
}

func TestLoadReadsTheHoldTTLSecondsVariable(t *testing.T) {
	setEnv(t, map[string]string{"BOOKING_HOLD_TTL_SECONDS": "600"})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned %v, want nil", err)
	}
	if cfg.HoldTTL != 10*time.Minute {
		t.Errorf("HoldTTL = %s, want 10m", cfg.HoldTTL)
	}
}

func TestLoadReadsTheDependencyBaseURLs(t *testing.T) {
	setEnv(t, map[string]string{
		"AVAILABILITY_BASE_URL": "http://availability.internal:9000",
		"CATALOG_BASE_URL":      "http://catalog.internal:9000",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned %v, want nil", err)
	}
	if cfg.AvailabilityBaseURL != "http://availability.internal:9000" {
		t.Errorf("AvailabilityBaseURL = %q", cfg.AvailabilityBaseURL)
	}
	if cfg.CatalogBaseURL != "http://catalog.internal:9000" {
		t.Errorf("CatalogBaseURL = %q", cfg.CatalogBaseURL)
	}
}

// A missing required variable must fail at startup, naming the variable.
func TestLoadFailsAndNamesAMissingRequiredVariable(t *testing.T) {
	cases := []struct {
		name    string
		missing string
	}{
		{"no database url", "DATABASE_URL"},
		{"no broker url", "RABBITMQ_URL"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, map[string]string{tc.missing: ""})

			_, err := Load()

			if err == nil {
				t.Fatalf("Load succeeded with %s unset", tc.missing)
			}
			if !strings.Contains(err.Error(), tc.missing) {
				t.Errorf("error %q does not name %s", err, tc.missing)
			}
		})
	}
}

func TestLoadRejectsANonPositiveHoldTTL(t *testing.T) {
	setEnv(t, map[string]string{"BOOKING_HOLD_TTL_SECONDS": "0"})

	_, err := Load()

	if err == nil {
		t.Fatal("Load accepted a zero hold TTL")
	}
	if !strings.Contains(err.Error(), "BOOKING_HOLD_TTL_SECONDS") {
		t.Errorf("error %q does not name the offending variable", err)
	}
}
