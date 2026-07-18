package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/morisempai/wakewake/shared/contracts/events"
	"github.com/morisempai/wakewake/shared/platform/correlation"
)

// Publisher is the narrow surface the relay needs from the broker.
//
// Declared here, in the consumer of the dependency, rather than in the broker package — the same
// rule service-template applies to domain/infra. It also means outbox tests can supply a fake
// publisher without an AMQP container.
type Publisher interface {
	Publish(ctx context.Context, e events.Envelope, body []byte) error
}

// RelayConfig tunes the relay. Defaults are from ADR-0010; zero values take them.
type RelayConfig struct {
	PollInterval  time.Duration // 1s
	BatchSize     int           // 100
	MaxAttempts   int           // 10, then failed_at is set and the row is skipped
	PruneAfter    time.Duration // 168h (7d, docs/nfr.md)
	PruneInterval time.Duration // 1h
}

func (c *RelayConfig) applyDefaults() {
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 10
	}
	if c.PruneAfter <= 0 {
		c.PruneAfter = 7 * 24 * time.Hour
	}
	if c.PruneInterval <= 0 {
		c.PruneInterval = time.Hour
	}
}

// Relay forwards staged outbox rows to the broker.
//
// It runs in-process rather than as a separate deployable (ADR-0010): a separate binary would be
// three more images, compose entries, and CI matrix rows, each a gated `.github/` change. The
// Run(ctx) shape keeps extraction cheap if scale ever demands it.
//
// Multiple replicas are safe — rows are claimed with FOR UPDATE SKIP LOCKED. Note the ordering
// caveat recorded in ADR-0010: with N replicas, cross-replica publish order is not guaranteed,
// so consumers must be state-based rather than order-dependent.
type Relay struct {
	pool *pgxpool.Pool
	pub  Publisher
	log  *slog.Logger
	cfg  RelayConfig
	kick chan struct{}
}

// NewRelay builds a relay. It does not start it; call Run.
func NewRelay(pool *pgxpool.Pool, pub Publisher, log *slog.Logger, cfg RelayConfig) *Relay {
	cfg.applyDefaults()
	return &Relay{
		pool: pool,
		pub:  pub,
		log:  log.With(slog.String("component", "outbox-relay")),
		cfg:  cfg,
		// Buffered depth 1: a kick that arrives while one is already pending is redundant, since
		// the pending one will drain the whole backlog anyway.
		kick: make(chan struct{}, 1),
	}
}

// Kick wakes the relay immediately, cutting happy-path publish latency from up to PollInterval
// down to milliseconds. Call it after a transaction that staged events commits.
//
// Purely an optimisation — never required for correctness. If it is missed, the next poll picks
// the rows up. Non-blocking by design: a caller must never be delayed because the relay is busy.
func (r *Relay) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Run drains the outbox until ctx is cancelled.
func (r *Relay) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	pruner := time.NewTicker(r.cfg.PruneInterval)
	defer pruner.Stop()

	r.log.InfoContext(ctx, "outbox relay started",
		slog.Duration("poll_interval", r.cfg.PollInterval),
		slog.Int("batch_size", r.cfg.BatchSize))

	for {
		select {
		case <-ctx.Done():
			r.log.InfoContext(ctx, "outbox relay stopped")
			return ctx.Err()

		case <-ticker.C:
			r.drain(ctx)

		case <-r.kick:
			r.drain(ctx)

		case <-pruner.C:
			if err := r.prune(ctx); err != nil {
				r.log.ErrorContext(ctx, "pruning outbox failed", slog.String("error", err.Error()))
			}
		}
	}
}

// drain publishes batches until the backlog is empty or ctx ends. Looping rather than doing one
// batch per tick means a backlog after an outage clears at broker speed instead of
// BatchSize per PollInterval, which for a 10k backlog would be nearly two minutes.
func (r *Relay) drain(ctx context.Context) {
	for {
		n, err := r.publishBatch(ctx)
		if err != nil {
			if ctx.Err() == nil {
				r.log.ErrorContext(ctx, "publishing outbox batch failed", slog.String("error", err.Error()))
			}
			return
		}
		if n < r.cfg.BatchSize {
			return
		}
	}
}

const claimSQL = `
SELECT id, event, version, payload, correlation_id, occurred_at, attempts
  FROM outbox
 WHERE published_at IS NULL AND failed_at IS NULL
 ORDER BY occurred_at, id
 LIMIT $1
   FOR UPDATE SKIP LOCKED`

type pending struct {
	id            string
	event         string
	version       int
	payload       json.RawMessage
	correlationID string
	occurredAt    time.Time
	attempts      int
}

// publishBatch claims a batch, publishes each row, and records the outcome — all inside one
// transaction.
//
// The ordering inside is the whole point and is not negotiable: publish, wait for the broker's
// confirm, and only then mark published_at. Marking first would lose the event whenever the
// broker dropped it, which would make the RPO=0 target in docs/nfr.md false. The cost of this
// ordering is that a crash after confirm but before commit republishes the event — which is
// fine, because at-least-once is the contract and consumers dedupe on the envelope id.
func (r *Relay) publishBatch(ctx context.Context) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("outbox relay: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, claimSQL, r.cfg.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("outbox relay: claiming rows: %w", err)
	}

	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.event, &p.version, &p.payload, &p.correlationID, &p.occurredAt, &p.attempts); err != nil {
			rows.Close()
			return 0, fmt.Errorf("outbox relay: scanning row: %w", err)
		}
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("outbox relay: reading rows: %w", err)
	}
	if len(batch) == 0 {
		return 0, nil
	}

	published := make([]string, 0, len(batch))
	for _, p := range batch {
		// Each row's own correlation ID goes into the context, so the relay's logs for this
		// event join the request that produced it rather than forming an orphan trail.
		rowCtx := correlation.WithID(ctx, p.correlationID)

		e := events.Envelope{
			Event:         p.event,
			Version:       p.version,
			ID:            p.id,
			OccurredAt:    p.occurredAt,
			CorrelationID: p.correlationID,
			Payload:       p.payload,
		}

		body, marshalErr := json.Marshal(e)
		if marshalErr == nil {
			marshalErr = e.Validate()
		}
		if marshalErr != nil {
			// A row that cannot even be turned into a valid envelope will never publish, so
			// retrying is pointless. Fail it immediately and keep the backlog moving.
			r.log.ErrorContext(rowCtx, "outbox row is unpublishable, failing it",
				slog.String("event", p.event), slog.String("event_id", p.id),
				slog.String("error", marshalErr.Error()))
			if err := r.failRow(ctx, tx, p.id, marshalErr); err != nil {
				return 0, err
			}
			continue
		}

		if err := r.pub.Publish(rowCtx, e, body); err != nil {
			if err := r.recordAttempt(ctx, tx, p, err); err != nil {
				return 0, err
			}
			// Stop the batch: a broker that just rejected one message will very likely reject
			// the rest, and hammering it turns a blip into a stampede. The next tick retries.
			break
		}
		published = append(published, p.id)
	}

	if len(published) > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE outbox SET published_at = now() WHERE id = ANY($1)`, published); err != nil {
			return 0, fmt.Errorf("outbox relay: marking published: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		// The events were published but not marked. They will republish on the next pass, and
		// consumers dedupe on the envelope id — at-least-once working as designed.
		return 0, fmt.Errorf("outbox relay: commit: %w", err)
	}

	if len(published) > 0 {
		r.log.DebugContext(ctx, "published outbox batch", slog.Int("count", len(published)))
	}
	return len(batch), nil
}

// recordAttempt increments attempts, and fails the row once MaxAttempts is reached.
func (r *Relay) recordAttempt(ctx context.Context, tx pgx.Tx, p pending, cause error) error {
	if p.attempts+1 >= r.cfg.MaxAttempts {
		r.log.ErrorContext(ctx, "outbox row exhausted its attempts; needs a human",
			slog.String("event", p.event), slog.String("event_id", p.id),
			slog.Int("attempts", p.attempts+1), slog.String("error", cause.Error()))
		return r.failRow(ctx, tx, p.id, cause)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE outbox SET attempts = attempts + 1, last_error = $2 WHERE id = $1`,
		p.id, cause.Error()); err != nil {
		return fmt.Errorf("outbox relay: recording attempt: %w", err)
	}
	return nil
}

// failRow marks a row failed so the relay stops retrying it.
//
// Skipping rather than blocking is deliberate: leaving a poison row at the head of the queue
// would stall every event behind it, converting one bad event into total delivery failure. The
// row stays in the table for a human to inspect, and failed rows should be alerted on.
func (r *Relay) failRow(ctx context.Context, tx pgx.Tx, id string, cause error) error {
	if _, err := tx.Exec(ctx,
		`UPDATE outbox SET failed_at = now(), attempts = attempts + 1, last_error = $2 WHERE id = $1`,
		id, cause.Error()); err != nil {
		return fmt.Errorf("outbox relay: failing row %s: %w", id, err)
	}
	return nil
}

// prune deletes rows published longer than PruneAfter ago (docs/nfr.md: 7 days).
//
// Bounded per pass so a large backlog cannot hold a long transaction and bloat the table's dead
// tuples faster than autovacuum reclaims them.
func (r *Relay) prune(ctx context.Context) error {
	tag, err := r.pool.Exec(ctx,
		`DELETE FROM outbox
		  WHERE id IN (
		    SELECT id FROM outbox
		     WHERE published_at IS NOT NULL AND published_at < now() - $1::interval
		     LIMIT 10000
		  )`,
		r.cfg.PruneAfter.String())
	if err != nil {
		return fmt.Errorf("outbox relay: pruning: %w", err)
	}
	if n := tag.RowsAffected(); n > 0 {
		r.log.InfoContext(ctx, "pruned published outbox rows", slog.Int64("count", n))
	}
	return nil
}
