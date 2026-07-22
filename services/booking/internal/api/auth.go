package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
)

// customerKey carries the authenticated customer id (the JWT `sub`) through the request context.
type customerKeyType struct{}

var customerKey customerKeyType

// authMiddleware extracts the customer id from the Bearer token's `sub` claim and puts it in the
// context. Handlers that require authentication read it with customerID; the two health probes do
// not, matching the spec's `security: []` on them.
//
// DEVIATION, flagged deliberately (see README): this decodes the JWT claims but does NOT verify the
// signature. ADR-0006 makes the API gateway the sole external ingress and the place OIDC tokens are
// validated; no gateway is wired in this slice, and Keycloak is not reachable from the test
// environment. Trusting the gateway to have verified the token, this reads `sub` to scope every
// booking to its owner. When the gateway lands, signature verification belongs there, or a shared
// verifier belongs in front of this — a shared-change issue, not a booking-local one.
func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sub := subFromAuthHeader(r.Header.Get("Authorization")); sub != "" {
			r = r.WithContext(context.WithValue(r.Context(), customerKey, sub))
		}
		next.ServeHTTP(w, r)
	})
}

// customerID returns the authenticated customer id, or "" and false when the request carried no
// usable token.
func customerID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(customerKey).(string)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// subFromAuthHeader pulls the `sub` claim out of a `Bearer <jwt>` header, or returns "".
func subFromAuthHeader(header string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, prefix))

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Sub
}
