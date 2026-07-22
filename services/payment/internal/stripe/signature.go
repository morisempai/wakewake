// Package stripe holds the two pieces of Stripe integration that carry PCI weight: verifying the
// webhook signature before any webhook body is trusted, and creating a PaymentIntent with an
// idempotency key. Neither ever handles card data — the browser sends the card straight to Stripe.
//
// Signature verification lives here, apart from HTTP wiring, because it is pure and security-
// critical and deserves its own focused tests (valid signed payload accepted, tampered/absent/
// expired rejected) with no network involved.
package stripe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidSignature is returned for every verification failure — a missing header, a malformed
// header, a signature that does not match, or a timestamp outside tolerance. The webhook maps all
// of them to the contract's single 400 with signature_verification_failed, deliberately without
// distinguishing which: a verbose error here is an oracle for forging signatures (payment.yaml).
var ErrInvalidSignature = errors.New("stripe: webhook signature verification failed")

// DefaultTolerance is Stripe's own default replay window. A signature whose timestamp is older than
// this is rejected, so a captured-and-replayed webhook cannot be accepted indefinitely.
const DefaultTolerance = 5 * time.Minute

// VerifySignature checks a Stripe-Signature header against the raw request body.
//
// The scheme (Stripe's): the header is `t=<unix ts>,v1=<hex hmac>[,v1=<hex hmac>...]`. The signed
// payload is `<t>.<rawBody>`, and the expected signature is HMAC-SHA256 of that with the endpoint's
// signing secret, hex-encoded. Verification MUST run over the RAW bytes, before any parse or
// re-serialisation, or the signature will not match — which is why the webhook handler reads the
// body itself rather than letting the generated server decode it.
//
// The comparison is constant-time (hmac.Equal), and the timestamp is checked against tolerance to
// reject replays. secret must be non-empty; an empty signing secret is a misconfiguration, not a
// pass.
func VerifySignature(payload []byte, header, secret string, now time.Time, tolerance time.Duration) error {
	if secret == "" {
		return fmt.Errorf("%w: no signing secret configured", ErrInvalidSignature)
	}
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}

	timestamp, signatures, err := parseSignatureHeader(header)
	if err != nil {
		return err
	}

	// Reject a timestamp outside the tolerance window in either direction: too old is a replay, too
	// far in the future is a forged or badly-clocked sender.
	skew := now.Sub(time.Unix(timestamp, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > tolerance {
		return fmt.Errorf("%w: timestamp outside tolerance", ErrInvalidSignature)
	}

	expected := computeSignature(timestamp, payload, secret)
	for _, sig := range signatures {
		// Decode to bytes so hmac.Equal compares equal-length byte slices in constant time; a hex
		// string compare would both be non-constant-time and mishandle case.
		got, err := hex.DecodeString(sig)
		if err != nil {
			continue
		}
		if hmac.Equal(got, expected) {
			return nil
		}
	}
	return fmt.Errorf("%w: no matching v1 signature", ErrInvalidSignature)
}

// parseSignatureHeader extracts the timestamp and the list of v1 signatures.
func parseSignatureHeader(header string) (timestamp int64, v1 []string, err error) {
	if strings.TrimSpace(header) == "" {
		return 0, nil, fmt.Errorf("%w: missing signature header", ErrInvalidSignature)
	}

	var haveTS bool
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			ts, convErr := strconv.ParseInt(kv[1], 10, 64)
			if convErr != nil {
				return 0, nil, fmt.Errorf("%w: unparseable timestamp", ErrInvalidSignature)
			}
			timestamp, haveTS = ts, true
		case "v1":
			v1 = append(v1, kv[1])
		}
	}

	if !haveTS || len(v1) == 0 {
		return 0, nil, fmt.Errorf("%w: header missing t or v1", ErrInvalidSignature)
	}
	return timestamp, v1, nil
}

// computeSignature is HMAC-SHA256 over "<timestamp>.<payload>" with the signing secret.
func computeSignature(timestamp int64, payload []byte, secret string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte{'.'})
	mac.Write(payload)
	return mac.Sum(nil)
}

// Sign produces a valid Stripe-Signature header for a payload. It exists for TESTS: they sign
// fixtures locally with the test signing secret to produce valid AND (by mutating the result)
// invalid payloads, so CI never reaches the real Stripe. It is not used by the running service.
func Sign(payload []byte, secret string, at time.Time) string {
	sig := computeSignature(at.Unix(), payload, secret)
	return fmt.Sprintf("t=%d,v1=%s", at.Unix(), hex.EncodeToString(sig))
}
