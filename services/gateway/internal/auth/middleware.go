package auth

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/morisempai/wakewake/shared/platform/httpx"
)

// codeUnauthenticated is the error code every 401 from the gateway carries. Clients switch on the
// code, not the HTTP status alone, so it is a stable part of the contract.
const codeUnauthenticated = "unauthenticated"

// Middleware authenticates a request before it reaches an internal service. On success it attaches
// the subject to the context and calls next. On any failure it writes a 401 with the shared error
// envelope and stops — the request never touches an upstream.
func Middleware(v *Verifier, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				unauthorized(w, r, log, "missing or malformed Authorization header")
				return
			}
			sub, err := v.Verify(token)
			if err != nil {
				// The specific reason is logged for operators; the client is told only that
				// authentication is required, so a probing attacker learns nothing about which
				// check failed.
				unauthorized(w, r, log, err.Error())
				return
			}
			next.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), sub)))
		})
	}
}

// bearerToken extracts the token from an `Authorization: Bearer <token>` header, matching the scheme
// case-insensitively per RFC 7235.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(h[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

func unauthorized(w http.ResponseWriter, r *http.Request, log *slog.Logger, reason string) {
	log.InfoContext(r.Context(), "rejecting unauthenticated request",
		slog.String("reason", reason),
		slog.String("path", r.URL.Path))
	// Signals the auth scheme to a compliant client without revealing anything sensitive.
	w.Header().Set("WWW-Authenticate", "Bearer")
	httpx.WriteError(w, r, http.StatusUnauthorized, codeUnauthenticated, "Authentication is required.")
}
