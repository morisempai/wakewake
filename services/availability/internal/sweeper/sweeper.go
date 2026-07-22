// Package sweeper releases holds whose TTL has run out.
//
// It is what makes the abandoned-checkout protection in ADR-0003 real: without it an unpaid hold
// keeps its window forever, and the exclusion constraint faithfully keeps everyone else out of a
// slot nobody is going to pay for.
package sweeper

import (
	"context"
	"log/slog"
	"time"

	"github.com/morisempai/wakewake/shared/platform/correlation"

	"github.com/morisempai/wakewake/services/availability/internal/domain"
)

// Sweeper runs the TTL sweep on an interval.
type Sweeper struct {
	svc      *domain.Service
	log      *slog.Logger
	interval time.Duration
	batch    int
}

// New builds a sweeper. It does not start it; call Run.
func New(svc *domain.Service, log *slog.Logger, interval time.Duration, batch int) *Sweeper {
	return &Sweeper{
		svc:      svc,
		log:      log.With(slog.String("component", "ttl-sweeper")),
		interval: interval,
		batch:    batch,
	}
}

// Run sweeps until ctx is cancelled.
//
// It runs in-process rather than as a separate deployable, for the same reason the outbox relay
// does (ADR-0010): a second binary is another image, compose entry, and gated CI matrix row, and
// the Run(ctx) shape keeps extraction cheap if that ever changes.
//
// Multiple replicas are safe. Each candidate is re-checked and updated under its own row lock,
// so two sweepers racing on one hold produce one release and one no-op, not two events.
func (s *Sweeper) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	s.log.InfoContext(ctx, "ttl sweeper started",
		slog.Duration("interval", s.interval), slog.Int("batch", s.batch))

	for {
		select {
		case <-ctx.Done():
			s.log.InfoContext(ctx, "ttl sweeper stopped")
			return ctx.Err()
		case <-ticker.C:
			s.sweepOnce(ctx)
		}
	}
}

// sweepOnce runs one pass, draining the backlog rather than releasing one batch per tick.
//
// After an outage there may be far more due holds than a batch. Releasing `batch` per interval
// would mean a backlog of 10k holds took over an hour to clear at the default settings, with
// every one of those slots unbookable in the meantime.
func (s *Sweeper) sweepOnce(ctx context.Context) {
	// Each pass gets its own correlation ID, so the ReservationReleased events it stages and the
	// log lines it writes belong to one identifiable run. Without it the outbox rows would carry
	// a freshly minted id each, and a sweep would be unreconstructable after the fact.
	ctx, corrID := correlation.Ensure(ctx)

	total := 0
	for {
		released, err := s.svc.Sweep(ctx, s.batch)
		total += released

		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Logged and dropped rather than returned: a sweep failure must not stop the loop,
			// or one bad pass silently ends TTL enforcement for the life of the process.
			s.log.ErrorContext(ctx, "ttl sweep pass failed",
				slog.String("error", err.Error()), slog.Int("released_before_failure", released))
			return
		}
		if released < s.batch {
			break
		}
	}

	if total > 0 {
		s.log.InfoContext(ctx, "released expired holds",
			slog.Int("count", total), slog.String("sweep_id", corrID))
	}
}
