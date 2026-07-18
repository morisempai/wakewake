package httpx

import (
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"time"
)

// IdempotencyKeyHeader is the header the specs require on write endpoints.
const IdempotencyKeyHeader = "Idempotency-Key"

// retryTransport retries only requests that are provably safe to repeat.
type retryTransport struct {
	base       http.RoundTripper
	maxRetries int
	backoff    time.Duration
}

func (t *retryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !retryable(r) {
		return t.base.RoundTrip(r)
	}

	// The body must be replayable across attempts. If it cannot be rewound, a retry would send
	// an empty body — worse than not retrying, because it looks like a malformed request from
	// the caller rather than a transport bug.
	var body []byte
	if r.Body != nil {
		if r.GetBody == nil {
			return t.base.RoundTrip(r)
		}
		rc, err := r.GetBody()
		if err != nil {
			return t.base.RoundTrip(r)
		}
		body, err = io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return t.base.RoundTrip(r)
		}
	}

	var lastResp *http.Response
	var lastErr error

	for attempt := 0; attempt <= t.maxRetries; attempt++ {
		if attempt > 0 {
			wait := fullJitter(min(t.backoff<<(attempt-1), time.Second))
			select {
			case <-time.After(wait):
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			if body != nil {
				rc, err := r.GetBody()
				if err != nil {
					return nil, err
				}
				r.Body = rc
			}
		}

		resp, err := t.base.RoundTrip(r)
		if err != nil {
			lastErr, lastResp = err, nil
			continue
		}
		if !retryableStatus(resp.StatusCode) {
			return resp, nil
		}

		// Drain and close before retrying, or the connection cannot be reused and the pool
		// leaks one socket per retry.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
		lastResp, lastErr = resp, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}
	if lastResp != nil {
		// Re-issue once so the caller receives a live body rather than the drained one.
		return t.base.RoundTrip(r)
	}
	return nil, errors.New("httpx: retries exhausted with no response")
}

// retryable reports whether repeating this request is safe.
//
// GET/HEAD/OPTIONS/PUT/DELETE are idempotent by HTTP semantics. POST is not — unless it carries
// an Idempotency-Key, which is exactly the contract the specs establish for write endpoints:
// the server returns the original result for a replayed key instead of acting twice.
func retryable(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	case http.MethodPost, http.MethodPatch:
		return r.Header.Get(IdempotencyKeyHeader) != ""
	default:
		return false
	}
}

// retryableStatus covers transient server-side conditions only.
//
// 4xx is deliberately absent: a rejected request will be rejected identically on retry, and
// retrying a 409 reservation_overlap would hammer a slot that is legitimately taken.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func fullJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Float64() * float64(d))
}
