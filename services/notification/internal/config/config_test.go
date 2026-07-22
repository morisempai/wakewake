package config

import (
	"strings"
	"testing"
)

// setEnv sets the variables a successful Load needs, plus any overrides. t.Setenv restores them and
// refuses to run under t.Parallel(), which is why nothing here is parallel.
func setEnv(t *testing.T, overrides map[string]string) {
	t.Helper()

	base := map[string]string{
		"DATABASE_URL": "postgres://notification@localhost:5432/notification",
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

	if cfg.SMTP.Host != "mailhog" {
		t.Errorf("SMTP.Host = %q, want mailhog", cfg.SMTP.Host)
	}
	if cfg.SMTP.Port != 1025 {
		t.Errorf("SMTP.Port = %d, want 1025", cfg.SMTP.Port)
	}
	if cfg.FromAddress == "" {
		t.Error("FromAddress is empty")
	}
	if cfg.ServiceName != ServiceName {
		t.Errorf("ServiceName = %q, want %q", cfg.ServiceName, ServiceName)
	}
}

func TestLoadReadsTheSMTPVariables(t *testing.T) {
	setEnv(t, map[string]string{"SMTP_HOST": "smtp.internal", "SMTP_PORT": "2525"})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned %v, want nil", err)
	}
	if cfg.SMTP.Host != "smtp.internal" || cfg.SMTP.Port != 2525 {
		t.Errorf("SMTP = %+v, want {smtp.internal 2525}", cfg.SMTP)
	}
}

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

func TestLoadRejectsANonNumericSMTPPort(t *testing.T) {
	setEnv(t, map[string]string{"SMTP_PORT": "not-a-port"})

	_, err := Load()

	if err == nil {
		t.Fatal("Load accepted a non-numeric SMTP_PORT")
	}
	if !strings.Contains(err.Error(), "SMTP_PORT") {
		t.Errorf("error %q does not name the offending variable", err)
	}
}

func TestLoadRejectsAnOutOfRangeSMTPPort(t *testing.T) {
	setEnv(t, map[string]string{"SMTP_PORT": "70000"})

	_, err := Load()

	if err == nil {
		t.Fatal("Load accepted an out-of-range SMTP_PORT")
	}
	if !strings.Contains(err.Error(), "SMTP_PORT") {
		t.Errorf("error %q does not name the offending variable", err)
	}
}
