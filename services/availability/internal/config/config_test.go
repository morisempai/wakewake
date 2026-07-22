package config

import (
	"strings"
	"testing"
	"time"
)

// setEnv sets the variables a successful Load needs, plus any overrides. t.Setenv restores them
// and refuses to run under t.Parallel(), which is why nothing here is parallel.
func setEnv(t *testing.T, overrides map[string]string) {
	t.Helper()

	base := map[string]string{
		"DATABASE_URL": "postgres://availability@localhost:5432/availability",
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

	// The NFR note on issue #5 fixes this at 900 seconds, and Booking puts the same value in
	// BookingHeld.hold_expires_at — the two must agree.
	if cfg.HoldTTL != 15*time.Minute {
		t.Errorf("HoldTTL = %s, want 15m", cfg.HoldTTL)
	}
	if cfg.SweepInterval != 30*time.Second {
		t.Errorf("SweepInterval = %s, want 30s", cfg.SweepInterval)
	}
	if cfg.SweepBatchSize != 100 {
		t.Errorf("SweepBatchSize = %d, want 100", cfg.SweepBatchSize)
	}
	if cfg.ServiceName != ServiceName {
		t.Errorf("ServiceName = %q, want %q", cfg.ServiceName, ServiceName)
	}
}

func TestLoadReadsTheSecondsVariables(t *testing.T) {
	setEnv(t, map[string]string{
		"BOOKING_HOLD_TTL_SECONDS":            "600",
		"AVAILABILITY_SWEEP_INTERVAL_SECONDS": "5",
		"AVAILABILITY_SWEEP_BATCH_SIZE":       "250",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned %v, want nil", err)
	}

	if cfg.HoldTTL != 10*time.Minute {
		t.Errorf("HoldTTL = %s, want 10m", cfg.HoldTTL)
	}
	if cfg.SweepInterval != 5*time.Second {
		t.Errorf("SweepInterval = %s, want 5s", cfg.SweepInterval)
	}
	if cfg.SweepBatchSize != 250 {
		t.Errorf("SweepBatchSize = %d, want 250", cfg.SweepBatchSize)
	}
}

// A missing required variable must fail at startup, naming the variable. A service that defaults
// its database URL starts cleanly in production and then cannot reach its database, which is a
// much harder failure to read.
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

// A sweep interval at or beyond the hold TTL means a lapsed hold sits unbookable for roughly
// twice its TTL. That is a configuration mistake, and it must be loud rather than merely slow.
func TestLoadRejectsASweepIntervalThatIsNotShorterThanTheHoldTTL(t *testing.T) {
	cases := []struct {
		name     string
		ttl      string
		interval string
	}{
		{"interval equals ttl", "900", "900"},
		{"interval exceeds ttl", "900", "1800"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, map[string]string{
				"BOOKING_HOLD_TTL_SECONDS":            tc.ttl,
				"AVAILABILITY_SWEEP_INTERVAL_SECONDS": tc.interval,
			})

			_, err := Load()

			if err == nil {
				t.Fatal("Load accepted a sweep interval that is not shorter than the hold TTL")
			}
			if !strings.Contains(err.Error(), "AVAILABILITY_SWEEP_INTERVAL_SECONDS") {
				t.Errorf("error %q does not name the offending variable", err)
			}
		})
	}
}

func TestLoadRejectsNonPositiveValues(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string]string
		wantVar   string
	}{
		{
			name:      "zero hold ttl",
			overrides: map[string]string{"BOOKING_HOLD_TTL_SECONDS": "0"},
			wantVar:   "BOOKING_HOLD_TTL_SECONDS",
		},
		{
			name:      "zero sweep batch",
			overrides: map[string]string{"AVAILABILITY_SWEEP_BATCH_SIZE": "0"},
			wantVar:   "AVAILABILITY_SWEEP_BATCH_SIZE",
		},
		{
			name:      "negative sweep batch",
			overrides: map[string]string{"AVAILABILITY_SWEEP_BATCH_SIZE": "-1"},
			wantVar:   "AVAILABILITY_SWEEP_BATCH_SIZE",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, tc.overrides)

			_, err := Load()

			if err == nil {
				t.Fatalf("Load accepted %v", tc.overrides)
			}
			if !strings.Contains(err.Error(), tc.wantVar) {
				t.Errorf("error %q does not name %s", err, tc.wantVar)
			}
		})
	}
}
