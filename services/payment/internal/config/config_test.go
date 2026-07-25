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
		"DATABASE_URL":          "postgres://payment@localhost:5432/payment",
		"RABBITMQ_URL":          "amqp://guest:guest@localhost:5672",
		"STRIPE_SECRET_KEY":     "sk_test_dummy",
		"STRIPE_WEBHOOK_SECRET": "whsec_dummy",
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
	if cfg.Stripe.BaseURL != "https://api.stripe.com" {
		t.Errorf("Stripe.BaseURL = %q, want the Stripe API default", cfg.Stripe.BaseURL)
	}
	if cfg.Stripe.WebhookTolerance != 5*time.Minute {
		t.Errorf("Stripe.WebhookTolerance = %s, want 5m", cfg.Stripe.WebhookTolerance)
	}
	if cfg.ServiceName != ServiceName {
		t.Errorf("ServiceName = %q, want %q", cfg.ServiceName, ServiceName)
	}
}

func TestLoadReadsStripeOverrides(t *testing.T) {
	setEnv(t, map[string]string{
		"STRIPE_BASE_URL":                  "http://stripe-mock:12111",
		"STRIPE_WEBHOOK_TOLERANCE_SECONDS": "120",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned %v", err)
	}
	if cfg.Stripe.BaseURL != "http://stripe-mock:12111" {
		t.Errorf("Stripe.BaseURL = %q", cfg.Stripe.BaseURL)
	}
	if cfg.Stripe.WebhookTolerance != 2*time.Minute {
		t.Errorf("Stripe.WebhookTolerance = %s, want 2m", cfg.Stripe.WebhookTolerance)
	}
}

// A missing required variable — including each Stripe secret — must fail at startup, naming it.
func TestLoadFailsAndNamesAMissingRequiredVariable(t *testing.T) {
	cases := []struct{ name, missing string }{
		{"no database url", "DATABASE_URL"},
		{"no broker url", "RABBITMQ_URL"},
		{"no stripe secret key", "STRIPE_SECRET_KEY"},
		{"no stripe webhook secret", "STRIPE_WEBHOOK_SECRET"},
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

// A startup error must name a missing secret's VARIABLE but never print its value.
func TestLoadErrorDoesNotEchoSecretValues(t *testing.T) {
	setEnv(t, map[string]string{"STRIPE_WEBHOOK_SECRET": ""})
	// The secret key is present; make sure a validation failure never echoes it.
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_this_must_never_be_logged")

	_, err := Load()
	if err == nil {
		t.Fatal("expected a validation error")
	}
	if strings.Contains(err.Error(), "sk_test_this_must_never_be_logged") {
		t.Errorf("the config error echoes a secret value: %v", err)
	}
}
