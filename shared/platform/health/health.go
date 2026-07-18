// Package health serves the liveness and readiness probes every service exposes.
//
// The distinction is not cosmetic and getting it backwards causes outages:
//
//   - /healthz (liveness) answers "is this process wedged?" Failing it gets the container KILLED.
//     It must therefore NOT check dependencies. A service that reports itself unhealthy because
//     Postgres is briefly unreachable gets restarted, loses its warm pool, and comes back to the
//     same unreachable Postgres — turning a database blip into a service-wide crash loop.
//   - /readyz (readiness) answers "can I serve traffic right now?" Failing it removes the
//     instance from the load balancer but leaves it running to recover. This is where dependency
//     checks belong.
//
// Response bodies match the schema in every OpenAPI spec: {"status":"ok"} on success, and the
// standard error envelope on 503.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/morisempai/wakewake/shared/platform/correlation"
)

// Check is one named readiness dependency. It should be cheap — `SELECT 1`, not a real query —
// because it runs on every probe interval, on every replica, forever.
type Check func(ctx context.Context) error

// Checker collects readiness checks.
type Checker struct {
	mu      sync.RWMutex
	checks  map[string]Check
	timeout time.Duration
}

// NewChecker builds a Checker. A zero timeout defaults to 2s: probes have their own deadline and
// a check that outlives it just wastes a connection.
func NewChecker(timeout time.Duration) *Checker {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &Checker{checks: map[string]Check{}, timeout: timeout}
}

// Register adds a named check. Safe to call after serving has started.
func (c *Checker) Register(name string, check Check) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks[name] = check
}

// LivenessHandler serves /healthz. It deliberately checks nothing: if this handler can run, the
// process is alive, which is the entire question being asked.
func LivenessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// ReadinessHandler serves /readyz, running every registered check.
func (c *Checker) ReadinessHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.RLock()
		checks := make(map[string]Check, len(c.checks))
		for name, check := range c.checks {
			checks[name] = check
		}
		c.mu.RUnlock()

		ctx, cancel := context.WithTimeout(r.Context(), c.timeout)
		defer cancel()

		failures := map[string]string{}
		for name, check := range checks {
			if err := check(ctx); err != nil {
				failures[name] = err.Error()
			}
		}

		if len(failures) == 0 {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}

		// The error envelope from the OpenAPI specs. Dependency names are included because
		// "not ready" without saying which dependency is unusable during an incident; the
		// underlying error strings go in details for the same reason.
		details := make([]map[string]string, 0, len(failures))
		for name, msg := range failures {
			details = append(details, map[string]string{"field": name, "issue": msg})
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{
				"code":           "internal_error",
				"message":        "one or more dependencies are unavailable",
				"details":        details,
				"correlation_id": correlation.FromContext(r.Context()),
			},
		})
	}
}

// Mount registers both probes on a mux. Probes are unauthenticated — the specs declare
// `security: []` on them, because an orchestrator has no token to present.
func (c *Checker) Mount(mux *http.ServeMux) {
	mux.Handle("GET /healthz", LivenessHandler())
	mux.Handle("GET /readyz", c.ReadinessHandler())
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
