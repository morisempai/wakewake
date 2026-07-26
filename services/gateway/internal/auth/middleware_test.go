package auth_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/morisempai/wakewake/shared/platform/logging"

	"github.com/morisempai/wakewake/services/gateway/internal/auth"
	"github.com/morisempai/wakewake/services/gateway/internal/authtest"
)

func discardLogger() *slog.Logger {
	return logging.New(logging.Options{Service: "gateway-test", Out: io.Discard})
}

// envelope mirrors the shared error body enough to assert the code.
type envelope struct {
	Error struct {
		Code          string `json:"code"`
		Message       string `json:"message"`
		CorrelationID string `json:"correlation_id"`
	} `json:"error"`
}

func TestMiddleware_RejectsWithEnvelope(t *testing.T) {
	iss := authtest.NewIssuer(t)
	v := newVerifier(t, iss, testIssuer, 30*time.Second)

	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	})
	h := auth.Middleware(v, discardLogger())(next)

	cases := []struct {
		name   string
		header string
	}{
		{"missing header", ""},
		{"wrong scheme", "Basic Zm9vOmJhcg=="},
		{"empty bearer", "Bearer "},
		{"garbage token", "Bearer not.a.jwt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nextCalled = false
			req := httptest.NewRequest(http.MethodGet, "/v1/products", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if nextCalled {
				t.Fatalf("next handler must not run on a rejected request")
			}
			if got := rec.Header().Get("WWW-Authenticate"); got == "" {
				t.Errorf("missing WWW-Authenticate header")
			}
			var env envelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("response body is not the shared envelope: %v", err)
			}
			if env.Error.Code != "unauthenticated" {
				t.Errorf("error code = %q, want unauthenticated", env.Error.Code)
			}
		})
	}
}

func TestMiddleware_PassesSubjectDownstream(t *testing.T) {
	iss := authtest.NewIssuer(t)
	v := newVerifier(t, iss, testIssuer, 30*time.Second)

	var gotSub string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSub = auth.SubjectFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := auth.Middleware(v, discardLogger())(next)

	token := iss.Sign(t, claims(testIssuer, "user-777", time.Now().Add(time.Hour)))
	req := httptest.NewRequest(http.MethodGet, "/v1/products", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotSub != "user-777" {
		t.Fatalf("subject in context = %q, want user-777", gotSub)
	}
}
