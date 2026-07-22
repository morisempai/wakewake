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
		"DATABASE_URL": "postgres://catalog@localhost:5432/catalog",
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

	if cfg.HTTP.Addr != ":8080" {
		t.Errorf("HTTP.Addr = %q, want :8080", cfg.HTTP.Addr)
	}
	if cfg.HTTP.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout = %s, want 15s", cfg.HTTP.ShutdownTimeout)
	}
	if cfg.ServiceName != ServiceName {
		t.Errorf("ServiceName = %q, want %q", cfg.ServiceName, ServiceName)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
}

func TestLoadReadsOverrides(t *testing.T) {
	setEnv(t, map[string]string{
		"HTTP_ADDR":             ":9090",
		"HTTP_SHUTDOWN_TIMEOUT": "30s",
		"LOG_LEVEL":             "debug",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned %v, want nil", err)
	}

	if cfg.HTTP.Addr != ":9090" {
		t.Errorf("HTTP.Addr = %q, want :9090", cfg.HTTP.Addr)
	}
	if cfg.HTTP.ShutdownTimeout != 30*time.Second {
		t.Errorf("ShutdownTimeout = %s, want 30s", cfg.HTTP.ShutdownTimeout)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
}

// A missing DATABASE_URL must fail at startup, naming the variable. A service that defaults its
// database URL starts cleanly in production and then cannot reach its database.
func TestLoadFailsWhenDatabaseURLIsMissing(t *testing.T) {
	setEnv(t, map[string]string{"DATABASE_URL": ""})

	_, err := Load()

	if err == nil {
		t.Fatal("Load succeeded with DATABASE_URL unset")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("error %q does not name DATABASE_URL", err)
	}
}

// Catalog opens no broker connection in this slice, so a missing RABBITMQ_URL must NOT stop it
// starting — the opposite of availability, which requires one. This guards against someone copying
// availability's config wholesale and reintroducing a dependency catalog does not have.
func TestLoadDoesNotRequireABroker(t *testing.T) {
	setEnv(t, map[string]string{"RABBITMQ_URL": ""})

	if _, err := Load(); err != nil {
		t.Errorf("Load failed with no RABBITMQ_URL set: %v — catalog publishes no events and must not require a broker", err)
	}
}
