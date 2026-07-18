// Package pgxx holds the small amount of pgx wiring every service repeats.
//
// It is deliberately thin. There is no repository abstraction, no query builder, and no ORM —
// ADR-0003 puts the core invariant in a Postgres exclusion constraint, and any layer that hides
// SQLSTATE codes hides the moment that invariant fires. `service-template` requires an ADR
// before adding one.
package pgxx

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig is the connection settings a service supplies.
type PoolConfig struct {
	// URL is a libpq connection string or postgres:// URL.
	URL string

	// MaxConns defaults to 10. Sized per service replica: Postgres' own max_connections is the
	// shared ceiling, so a large pool per replica starves the others long before it helps.
	MaxConns int32

	// MaxConnLifetime defaults to 1h, so connections recycle through a rolling restart of the
	// database rather than pinning to one backend forever.
	MaxConnLifetime time.Duration

	// ConnectTimeout defaults to 5s.
	ConnectTimeout time.Duration
}

// NewPool opens a pool and verifies it can actually reach the database.
//
// pgxpool.New is lazy — it returns a usable pool without contacting the server, so a service
// with a wrong password starts happily and fails on its first request instead of at boot. The
// Ping here converts that into a startup failure, which is what `service-template`'s "fail fast
// on missing config" is asking for.
func NewPool(ctx context.Context, c PoolConfig) (*pgxpool.Pool, error) {
	if c.URL == "" {
		return nil, fmt.Errorf("pgxx: database URL is empty")
	}

	cfg, err := pgxpool.ParseConfig(c.URL)
	if err != nil {
		return nil, fmt.Errorf("pgxx: parsing database url: %w", err)
	}

	cfg.MaxConns = c.MaxConns
	if cfg.MaxConns == 0 {
		cfg.MaxConns = 10
	}
	cfg.MaxConnLifetime = c.MaxConnLifetime
	if cfg.MaxConnLifetime == 0 {
		cfg.MaxConnLifetime = time.Hour
	}

	timeout := c.ConnectTimeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgxx: creating pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgxx: database unreachable: %w", err)
	}
	return pool, nil
}

// WithinTx runs fn inside a transaction, committing on success and rolling back on error or
// panic.
//
// This is the function the outbox depends on: the domain write and outbox.Enqueue must share one
// transaction, and hand-rolled begin/defer-rollback/commit is exactly the sequence people get
// subtly wrong — usually by returning early on an error path before the rollback is registered.
//
// The panic path re-panics after rolling back. Swallowing it would turn a bug into a silently
// abandoned transaction holding locks until the connection is reaped.
func WithinTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) (err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgxx: begin: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if err != nil {
			// Rollback's error is deliberately dropped: the caller needs the error that caused
			// the rollback, not the rollback's own. Rollback after a failed commit is a no-op.
			_ = tx.Rollback(ctx)
		}
	}()

	if err = fn(tx); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("pgxx: commit: %w", err)
	}
	return nil
}
