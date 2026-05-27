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
	t.Cleanup(pool.Close)
	return pool
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
