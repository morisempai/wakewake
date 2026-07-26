// Package ratelimit applies a per-caller token bucket at the edge.
//
// The limiter is keyed on the authenticated subject where there is one, and falls back to the
// client IP otherwise (the public Stripe webhook has no subject). Keying on the subject means one
// noisy user cannot exhaust the budget for everyone behind the same NAT, and one abusive IP cannot
// be laundered through many anonymous requests.
package ratelimit

import (
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/morisempai/wakewake/services/gateway/internal/auth"
	"github.com/morisempai/wakewake/shared/platform/httpx"
)

// codeRateLimited is the error code every 429 from the gateway carries.
const codeRateLimited = "rate_limited"

// evictThreshold is the bucket count above which Allow opportunistically drops idle buckets. A
// gateway sees a bounded set of active subjects at any moment; without eviction a flood of unique
// IPs would grow the map without limit.
const evictThreshold = 4096

// Limiter holds one token bucket per key. It is safe for concurrent use.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rps     rate.Limit
	burst   int
	idleTTL time.Duration
	now     func() time.Time
}

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

// New builds a Limiter allowing rps requests per second per key with the given burst. Non-positive
// values are clamped to a safe minimum so a misconfiguration cannot disable limiting entirely.
func New(rps float64, burst int) *Limiter {
	if rps <= 0 {
		rps = 1
	}
	if burst <= 0 {
		burst = 1
	}
	return &Limiter{
		buckets: make(map[string]*bucket),
		rps:     rate.Limit(rps),
		burst:   burst,
		idleTTL: 10 * time.Minute,
		now:     time.Now,
	}
}

// Allow reports whether a request for key may proceed, consuming a token if so.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{lim: rate.NewLimiter(l.rps, l.burst)}
		l.buckets[key] = b
		l.evictIdle(now)
	}
	b.seen = now
	return b.lim.AllowN(now, 1)
}

// evictIdle drops buckets untouched for longer than idleTTL. It runs only when the map has grown
// past evictThreshold and under the lock Allow already holds, so it adds no contention on the hot
// path where the key already exists.
func (l *Limiter) evictIdle(now time.Time) {
	if len(l.buckets) < evictThreshold {
		return
	}
	for k, b := range l.buckets {
		if now.Sub(b.seen) > l.idleTTL {
			delete(l.buckets, k)
		}
	}
}

// Middleware rejects a request that exceeds its key's budget with a 429 and the shared error
// envelope. It must run AFTER auth on protected routes, so the subject is available in the context
// and the bucket keys on identity rather than IP.
func Middleware(l *Limiter, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.Allow(keyFor(r)) {
				log.WarnContext(r.Context(), "rate limit exceeded",
					slog.String("path", r.URL.Path))
				w.Header().Set("Retry-After", "1")
				httpx.WriteError(w, r, http.StatusTooManyRequests, codeRateLimited,
					"Too many requests. Please retry shortly.")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func keyFor(r *http.Request) string {
	if sub := auth.SubjectFromContext(r.Context()); sub != "" {
		return "sub:" + sub
	}
	return "ip:" + clientIP(r)
}

// clientIP takes the address the connection actually came from. The gateway is the true edge, so
// RemoteAddr is the real client; trusting a forwarded header here would let a caller spoof its key.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
