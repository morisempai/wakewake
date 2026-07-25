package stripe

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const testWebhookSecret = "whsec_test_secret_value"

var signedAt = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

func payload() []byte {
	return []byte(`{"id":"evt_123","type":"payment_intent.succeeded","data":{"object":{"id":"pi_123"}}}`)
}

// AC1 — a valid signed payload verifies.
func TestVerifySignatureAcceptsAValidSignedPayload_Issue18_AC1(t *testing.T) {
	t.Parallel()

	header := Sign(payload(), testWebhookSecret, signedAt)

	if err := VerifySignature(payload(), header, testWebhookSecret, signedAt, DefaultTolerance); err != nil {
		t.Fatalf("a validly signed payload was rejected: %v", err)
	}
}

// AC1 — every way a signature can be wrong is rejected, and always as ErrInvalidSignature.
func TestVerifySignatureRejectsInvalidSignatures_Issue18_AC1(t *testing.T) {
	t.Parallel()

	good := Sign(payload(), testWebhookSecret, signedAt)

	cases := []struct {
		name      string
		payload   []byte
		header    string
		secret    string
		now       time.Time
		tolerance time.Duration
	}{
		{
			name:    "tampered body after signing",
			payload: []byte(`{"id":"evt_123","type":"payment_intent.succeeded","data":{"object":{"id":"pi_ATTACKER"}}}`),
			header:  good, secret: testWebhookSecret, now: signedAt, tolerance: DefaultTolerance,
		},
		{
			name:    "absent signature header",
			payload: payload(), header: "", secret: testWebhookSecret, now: signedAt, tolerance: DefaultTolerance,
		},
		{
			name:    "wrong signing secret",
			payload: payload(), header: good, secret: "whsec_a_different_secret", now: signedAt, tolerance: DefaultTolerance,
		},
		{
			name:    "expired timestamp beyond tolerance",
			payload: payload(), header: good, secret: testWebhookSecret,
			now: signedAt.Add(10 * time.Minute), tolerance: 5 * time.Minute,
		},
		{
			name:    "malformed header",
			payload: payload(), header: "not-a-signature-header", secret: testWebhookSecret, now: signedAt, tolerance: DefaultTolerance,
		},
		{
			name:    "signature of a different payload",
			payload: payload(), header: Sign([]byte(`{"id":"evt_other"}`), testWebhookSecret, signedAt),
			secret: testWebhookSecret, now: signedAt, tolerance: DefaultTolerance,
		},
		{
			name:    "empty signing secret is a misconfiguration, not a pass",
			payload: payload(), header: good, secret: "", now: signedAt, tolerance: DefaultTolerance,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := VerifySignature(tc.payload, tc.header, tc.secret, tc.now, tc.tolerance)
			if err == nil {
				t.Fatal("verification succeeded, want rejection")
			}
			if !errors.Is(err, ErrInvalidSignature) {
				t.Errorf("error = %v, want ErrInvalidSignature", err)
			}
		})
	}
}

// The error is deliberately terse — it must not become an oracle for forging signatures, and it
// must never contain the signing secret.
func TestVerifySignatureErrorNeverLeaksTheSecret_Issue18_AC1(t *testing.T) {
	t.Parallel()

	err := VerifySignature(payload(), "t=1,v1=deadbeef", testWebhookSecret, signedAt, DefaultTolerance)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), testWebhookSecret) {
		t.Errorf("the verification error leaks the signing secret: %v", err)
	}
}
