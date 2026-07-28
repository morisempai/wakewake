package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/morisempai/wakewake/services/gateway/internal/auth"
	"github.com/morisempai/wakewake/services/gateway/internal/authtest"
)

const testIssuer = "https://issuer.test/realms/booking"

// newVerifier builds a Verifier whose JWKS is served by iss over httptest, while the EXPECTED issuer
// string is passed independently. The JWKS URL (a 127.0.0.1 address) is deliberately different from
// the issuer string, which is exactly the docker split the gateway must handle.
func newVerifier(t *testing.T, iss *authtest.Issuer, expectedIssuer string, skew time.Duration) *auth.Verifier {
	t.Helper()
	srv := iss.StartJWKS(t)
	if srv.URL == expectedIssuer {
		t.Fatalf("test setup: JWKS URL must differ from the issuer string to exercise the split")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	v, err := auth.NewVerifier(ctx, srv.URL, expectedIssuer, skew)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

func claims(iss, sub string, exp time.Time) jwt.RegisteredClaims {
	c := jwt.RegisteredClaims{
		Issuer:   iss,
		Subject:  sub,
		IssuedAt: jwt.NewNumericDate(time.Now()),
	}
	if !exp.IsZero() {
		c.ExpiresAt = jwt.NewNumericDate(exp)
	}
	return c
}

func TestVerify_Table(t *testing.T) {
	iss := authtest.NewIssuer(t)
	other := authtest.NewIssuer(t) // same default kid, different key → forged signature
	v := newVerifier(t, iss, testIssuer, 30*time.Second)

	now := time.Now()

	tests := []struct {
		name    string
		token   string
		wantSub string
		wantErr bool
	}{
		{
			name:    "valid",
			token:   iss.Sign(t, claims(testIssuer, "user-123", now.Add(time.Hour))),
			wantSub: "user-123",
		},
		{
			name:    "expired",
			token:   iss.Sign(t, claims(testIssuer, "user-123", now.Add(-2*time.Hour))),
			wantErr: true,
		},
		{
			name:    "wrong issuer",
			token:   iss.Sign(t, claims("https://evil.example/realms/booking", "user-123", now.Add(time.Hour))),
			wantErr: true,
		},
		{
			name:    "missing exp",
			token:   iss.Sign(t, claims(testIssuer, "user-123", time.Time{})),
			wantErr: true,
		},
		{
			name:    "missing sub",
			token:   iss.Sign(t, claims(testIssuer, "", now.Add(time.Hour))),
			wantErr: true,
		},
		{
			name:    "bad signature",
			token:   other.Sign(t, claims(testIssuer, "user-123", now.Add(time.Hour))),
			wantErr: true,
		},
		{
			name:    "malformed",
			token:   "this.is.not-a-jwt",
			wantErr: true,
		},
		{
			name:    "empty",
			token:   "",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sub, err := v.Verify(tc.token)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Verify(%s): expected an error, got sub=%q", tc.name, sub)
				}
				return
			}
			if err != nil {
				t.Fatalf("Verify(%s): unexpected error: %v", tc.name, err)
			}
			if sub != tc.wantSub {
				t.Fatalf("Verify(%s): sub = %q, want %q", tc.name, sub, tc.wantSub)
			}
		})
	}
}

// TestVerify_ClockSkew shows a token that expired just now is still accepted within the configured
// skew, and rejected once it is outside it.
func TestVerify_ClockSkew(t *testing.T) {
	iss := authtest.NewIssuer(t)
	v := newVerifier(t, iss, testIssuer, 30*time.Second)
	now := time.Now()

	within := iss.Sign(t, claims(testIssuer, "u", now.Add(-10*time.Second)))
	if _, err := v.Verify(within); err != nil {
		t.Fatalf("token expired within skew should be accepted: %v", err)
	}

	beyond := iss.Sign(t, claims(testIssuer, "u", now.Add(-5*time.Minute)))
	if _, err := v.Verify(beyond); err == nil {
		t.Fatalf("token expired beyond skew should be rejected")
	}
}

func TestReady(t *testing.T) {
	iss := authtest.NewIssuer(t)
	v := newVerifier(t, iss, testIssuer, 30*time.Second)
	if err := v.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: expected the fetched JWKS to hold a key: %v", err)
	}
}
