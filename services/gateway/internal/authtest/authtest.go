// Package authtest provides an in-process RSA signer and a fake JWKS server for the gateway's tests.
//
// It exists so the auth and router tests validate REAL tokens against a REAL JWKS document without
// depending on a live Keycloak: the tests mint tokens with a key they generated and serve that
// key's public half over httptest, exactly as Keycloak would. It is imported only by test binaries,
// so none of it enters the shipped gateway.
package authtest

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MicahParks/jwkset"
	"github.com/golang-jwt/jwt/v5"
)

// DefaultKID is the key id the issuer stamps on both the JWKS entry and every token header, so the
// verifier can match a token to its signing key.
const DefaultKID = "gateway-test-key-1"

// Issuer is a self-contained token authority: an RSA key pair plus the metadata needed to publish a
// JWKS and sign tokens the gateway will accept.
type Issuer struct {
	Key *rsa.PrivateKey
	KID string
}

// NewIssuer generates a fresh 2048-bit RSA issuer.
func NewIssuer(t *testing.T) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("authtest: generating rsa key: %v", err)
	}
	return &Issuer{Key: key, KID: DefaultKID}
}

// JWKSJSON returns the public JWKS document for this issuer's key — the same shape Keycloak serves
// at /realms/<realm>/protocol/openid-connect/certs.
func (i *Issuer) JWKSJSON(t *testing.T) []byte {
	t.Helper()
	jwk, err := jwkset.NewJWKFromKey(i.Key, jwkset.JWKOptions{
		Metadata: jwkset.JWKMetadataOptions{KID: i.KID, ALG: jwkset.AlgRS256},
	})
	if err != nil {
		t.Fatalf("authtest: building jwk: %v", err)
	}
	store := jwkset.NewMemoryStorage()
	if err := store.KeyWrite(context.Background(), jwk); err != nil {
		t.Fatalf("authtest: writing jwk: %v", err)
	}
	raw, err := store.JSONPublic(context.Background())
	if err != nil {
		t.Fatalf("authtest: marshaling jwks: %v", err)
	}
	return raw
}

// StartJWKS starts an httptest server serving this issuer's public JWKS and registers it for
// cleanup. The returned URL is what the gateway's OIDC_JWKS_URL points at in tests — deliberately
// different from the issuer STRING, which mirrors the docker split the gateway must handle.
func (i *Issuer) StartJWKS(t *testing.T) *httptest.Server {
	t.Helper()
	body := i.JWKSJSON(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Sign issues a token signed with this issuer's key and stamped with its kid.
func (i *Issuer) Sign(t *testing.T, claims jwt.RegisteredClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = i.KID
	signed, err := tok.SignedString(i.Key)
	if err != nil {
		t.Fatalf("authtest: signing token: %v", err)
	}
	return signed
}
