// Package auth validates Keycloak-issued JWTs against the realm JWKS and exposes the result as HTTP
// middleware.
//
// The verifier checks three things a bearer token must satisfy before any request reaches an
// internal service: a signature made by a key in the realm JWKS, an `iss` equal to the configured
// issuer, and an `exp` in the future (within a bounded clock skew). Anything else is a 401 — the
// gateway is the one place these checks happen, so a gap here is a gap everywhere.
package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// ErrNoSubject is returned when a token validates but carries no `sub` claim. A token with no
// subject cannot be attributed to a caller, so it is rejected rather than proxied anonymously.
var ErrNoSubject = errors.New("auth: token has no subject (sub) claim")

// allowedAlgs pins the acceptable signing algorithms. Restricting these is a security control, not
// a formality: without it an attacker can present an `alg: none` token, or an HS256 token signed
// with the public key as the HMAC secret, and have it accepted. Keycloak signs realm tokens with
// RSA, so only the RS family is allowed.
var allowedAlgs = []string{"RS256", "RS384", "RS512"}

// Verifier validates bearer tokens. It is safe for concurrent use.
type Verifier struct {
	keyfunc keyfunc.Keyfunc
	issuer  string
	leeway  time.Duration
}

// NewVerifier builds a Verifier that fetches signing keys from jwksURL and refreshes them in the
// background for the life of ctx. It fails if the JWKS cannot be fetched at startup — the same
// posture as a service that cannot reach its database: better to fail loudly at boot than to accept
// nothing (or worse, everything) later.
//
// issuer is matched against the token's `iss` claim and is deliberately independent of jwksURL. See
// config.Config.OIDCIssuer for why the two must be configured separately.
func NewVerifier(ctx context.Context, jwksURL, issuer string, clockSkew time.Duration) (*Verifier, error) {
	if issuer == "" {
		return nil, errors.New("auth: OIDC issuer must be configured")
	}
	if jwksURL == "" {
		return nil, errors.New("auth: OIDC JWKS URL must be configured")
	}
	kf, err := keyfunc.NewDefaultCtx(ctx, []string{jwksURL})
	if err != nil {
		return nil, fmt.Errorf("auth: fetching JWKS from %q: %w", jwksURL, err)
	}
	return NewVerifierWithKeyfunc(kf, issuer, clockSkew), nil
}

// NewVerifierWithKeyfunc builds a Verifier around an already-constructed keyfunc. It exists so tests
// can supply a keyfunc pointed at a fake JWKS server rather than a live Keycloak.
func NewVerifierWithKeyfunc(kf keyfunc.Keyfunc, issuer string, clockSkew time.Duration) *Verifier {
	return &Verifier{keyfunc: kf, issuer: issuer, leeway: clockSkew}
}

// Verify checks a raw token string and returns its subject on success. Every failure mode —
// bad signature, unknown key, wrong issuer, expired, missing exp, missing sub, malformed — returns
// a non-nil error and an empty subject. Callers translate that into a single 401 that leaks nothing
// about which check failed.
func (v *Verifier) Verify(token string) (string, error) {
	var claims jwt.RegisteredClaims
	if _, err := jwt.ParseWithClaims(token, &claims, v.keyfunc.Keyfunc,
		jwt.WithValidMethods(allowedAlgs),
		jwt.WithIssuer(v.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(v.leeway),
	); err != nil {
		return "", err
	}
	if claims.Subject == "" {
		return "", ErrNoSubject
	}
	return claims.Subject, nil
}

// Ready reports whether the verifier holds at least one usable signing key. It backs the /readyz
// probe: a gateway with no keys cannot authenticate anyone, so it must not receive traffic.
func (v *Verifier) Ready(ctx context.Context) error {
	ks, err := v.keyfunc.VerificationKeySet(ctx)
	if err != nil {
		return fmt.Errorf("auth: reading JWKS: %w", err)
	}
	if len(ks.Keys) == 0 {
		return errors.New("auth: JWKS holds no verification keys")
	}
	return nil
}
