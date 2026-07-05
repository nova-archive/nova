// Package dbtest provides a one-call helper to spin up Postgres in a
// testcontainer, apply Nova's embedded migrations, and return a
// pgxpool. Integration tests across internal/envelope, internal/jobs,
// and internal/integration use it.
//
// The helper is deliberately not in internal/db so the production code
// path does not import testcontainers (which transitively pulls Docker
// client libraries). Importing testcontainers from non-test packages
// would balloon every binary's link footprint.
package dbtest

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/nova-archive/nova/internal/db"
	"github.com/nova-archive/nova/internal/db/migrations"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// New returns a pgxpool against a freshly-migrated Postgres container.
// The container is terminated on t.Cleanup.
//
// Caller MUST treat the returned pool as scoped to the current test.
// Spawning multiple containers in parallel tests is supported but
// slow; prefer reusing the pool within a single test via subtests.
func New(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("nova"),
		postgres.WithUsername("nova"),
		postgres.WithPassword("test-password"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = container.Terminate(shutdownCtx)
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	applyMigrations(t, ctx, dsn)

	pool, err := db.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { closeBounded(t, pool) })
	return pool
}

// poolCloseGrace bounds how long a test cleanup will wait for
// pgxpool.Pool.Close. Close blocks until every acquired connection is
// released (puddle waits on a WaitGroup), so a test that t.Fatal's while
// holding an open Tx/Conn would otherwise hang the whole package until
// go test's timeout (default 10m) — the P2-M7.1 "2 packages hang 15m"
// failure mode.
const poolCloseGrace = 5 * time.Second

// closeBounded closes the pool but gives up after poolCloseGrace,
// logging loudly instead of hanging. On timeout the abandoned Close
// goroutine does NOT recover: puddle's Close waits on a WaitGroup that
// only decrements when the leaked resource is released or destroyed,
// and the container terminate that runs next severs the TCP connection
// without doing either — the goroutine stays blocked until the test
// process exits. That is benign under `go test`, where each package
// runs in its own short-lived process.
func closeBounded(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		pool.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(poolCloseGrace):
		t.Logf("dbtest: WARNING: pgxpool.Close still blocked after %s — this test leaked an acquired Tx/Conn (t.Fatal before Rollback/Release?). Abandoning the close so the test fails fast; the container terminate cleanup will sever the leaked connection.", poolCloseGrace)
	}
}

func applyMigrations(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	sqlDB, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer sqlDB.Close()

	require.NoError(t, goose.SetDialect("postgres"))
	goose.SetBaseFS(migrations.Migrations)
	require.NoError(t, goose.UpContext(ctx, sqlDB, "."))
}

// suppress unused-import warning for stdlib's pgx driver in case the
// linker tries to trim it (we need its init() side effect).
var _ = stdlib.GetDefaultDriver
