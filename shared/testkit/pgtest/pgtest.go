// Package pgtest starts a real Postgres for integration tests.
//
// It exists because ADR-0003's invariant CANNOT be tested any other way. The no-double-booking
// guarantee is a Postgres exclusion constraint, so a fake repository backed by a map will
// cheerfully report that it prevents overlaps — proving only that the fake works. The one test
// this whole architecture rests on needs a real database, and every service agent needing to
// build container setup from scratch is how that test lands late or not at all.
//
// btree_gist is installed here rather than left to each caller: the exclusion constraint cannot
// be created without it, and infra/postgres/init-databases.sh creates empty databases only.
package pgtest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

var (
	sharedOnce sync.Once
	sharedURL  string
	sharedErr  error
	sharedCtr  *postgres.PostgresContainer
)

// container starts one Postgres per test binary and reuses it.
//
// Per-binary rather than per-test: starting a container takes seconds, and a suite that pays
// that per test is a suite people stop running. Isolation is preserved by giving each test its
// own database inside the shared server.
func container(ctx context.Context) (string, error) {
	sharedOnce.Do(func() {
		ctr, err := postgres.Run(ctx,
			"postgres:16-alpine",
			postgres.WithDatabase("template"),
			postgres.WithUsername("test"),
			postgres.WithPassword("test"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(60*time.Second),
			),
		)
		if err != nil {
			sharedErr = fmt.Errorf("pgtest: starting postgres: %w", err)
			return
		}
		sharedCtr = ctr

		url, err := ctr.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			sharedErr = fmt.Errorf("pgtest: connection string: %w", err)
			return
		}
		sharedURL = url
	})
	return sharedURL, sharedErr
}

// Postgres returns a pool against a database unique to this test, with every .sql file in
// migrationsDir applied in filename order.
//
// Each test gets its own database so tests can run in parallel without seeing each other's rows
// — which matters here because the concurrency proof deliberately creates contention, and a
// shared schema would make unrelated tests flaky in a way that looks like a real bug.
func Postgres(t *testing.T, migrationsDir string) *pgxpool.Pool {
	t.Helper()

	ctx := context.Background()
	baseURL, err := container(ctx)
	if err != nil {
		t.Fatalf("%v", err)
	}

	admin, err := pgxpool.New(ctx, baseURL)
	if err != nil {
		t.Fatalf("pgtest: connecting as admin: %v", err)
	}
	defer admin.Close()

	dbName := uniqueDBName(t)
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %q", dbName)); err != nil {
		t.Fatalf("pgtest: creating database %s: %v", dbName, err)
	}

	url := replaceDBName(baseURL, dbName)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pgtest: connecting to %s: %v", dbName, err)
	}

	// Required by the ADR-0003 exclusion constraint: gist must index resource_id (compared with
	// =) alongside during (compared with &&) in one index, which plain gist cannot do.
	if _, err := pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS btree_gist"); err != nil {
		pool.Close()
		t.Fatalf("pgtest: creating btree_gist: %v", err)
	}

	if migrationsDir != "" {
		applyMigrations(t, ctx, pool, migrationsDir)
	}

	t.Cleanup(func() {
		pool.Close()
		// The database is deliberately NOT dropped: the container dies with the test binary
		// anyway, and keeping it lets you inspect state after a failure instead of staring at an
		// assertion message.
	})
	return pool
}

func applyMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("pgtest: reading migrations from %s: %v", dir, err)
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	// Filename order. Migrations are numbered (0001_, 0002_) precisely so this is deterministic
	// — applying them in directory order would work on one machine and fail on another.
	sort.Strings(files)

	if len(files) == 0 {
		t.Fatalf("pgtest: no .sql migrations found in %s — a test running against an empty schema "+
			"would pass by testing nothing", dir)
	}

	for _, name := range files {
		sql, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("pgtest: reading %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("pgtest: applying %s: %v", name, err)
		}
	}
}

// uniqueDBName derives a legal, unique database name from the test name.
func uniqueDBName(t *testing.T) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		default:
			return '_'
		}
	}, t.Name())

	// Postgres identifiers cap at 63 bytes; a long subtest name would otherwise be truncated
	// into a collision with its siblings.
	if len(name) > 40 {
		name = name[:40]
	}
	return fmt.Sprintf("t_%s_%d", name, time.Now().UnixNano())
}

func replaceDBName(url, db string) string {
	// testcontainers returns postgres://user:pass@host:port/template?sslmode=disable
	slash := strings.LastIndex(url, "/")
	if slash < 0 {
		return url
	}
	rest := url[slash+1:]
	if q := strings.Index(rest, "?"); q >= 0 {
		return url[:slash+1] + db + rest[q:]
	}
	return url[:slash+1] + db
}

// Terminate stops the shared container. Call from TestMain if you want deterministic cleanup;
// otherwise Ryuk reaps it when the test process exits.
func Terminate(ctx context.Context) error {
	if sharedCtr == nil {
		return nil
	}
	return sharedCtr.Terminate(ctx)
}
