package migrations

import (
	"context"
	"database/sql"
	"io/fs"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestMigrationsFSContainsExpectedFiles(t *testing.T) {
	got, err := fs.ReadDir(Migrations, ".")
	require.NoError(t, err)

	names := make([]string, 0, len(got))
	for _, e := range got {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}

	require.Contains(t, names, "0001_init.sql")
	require.Contains(t, names, "0002_jobs.sql")
	require.Contains(t, names, "0003_partitions.sql")
	require.Contains(t, names, "0004_envelope_version.sql")
	require.Contains(t, names, "0005_upload_sessions.sql")
	require.Contains(t, names, "0006_auth.sql")
}

func TestMigrationsFSFirstFileHasGooseAnnotation(t *testing.T) {
	data, err := fs.ReadFile(Migrations, "0001_init.sql")
	require.NoError(t, err)
	require.Contains(t, string(data), "-- +goose Up")
	require.Contains(t, string(data), "-- +goose Down")
}

func TestMigrationsUpDown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

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

	sqlDB, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer sqlDB.Close()

	require.NoError(t, goose.SetDialect("postgres"))
	goose.SetBaseFS(Migrations)

	// Apply all migrations up.
	require.NoError(t, goose.UpContext(ctx, sqlDB, "."))

	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	defer pool.Close()

	// Assert tables from 0005.
	requireTableExists(t, ctx, pool, "upload_sessions")

	// Assert schema additions from 0006.
	requireTableExists(t, ctx, pool, "refresh_tokens")
	requireColumnExists(t, ctx, pool, "users", "password_hash")
	requireColumnExists(t, ctx, pool, "users", "disabled")

	// Assert schema additions from 0012 (P2-M3 assignment sync).
	requireColumnExists(t, ctx, pool, "pin_assignments", "assignment_id")
	requireColumnExists(t, ctx, pool, "pin_assignments", "generation")
	requireTableExists(t, ctx, pool, "pin_changes")
	requireTableExists(t, ctx, pool, "federation_change_log_state")
	var prunedThroughSeq int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT pruned_through_seq FROM federation_change_log_state`).Scan(&prunedThroughSeq))
	require.Equal(t, int64(0), prunedThroughSeq, "watermark must seed to 0")

	// Roll back all migrations to verify down migrations are clean.
	require.NoError(t, goose.DownToContext(ctx, sqlDB, ".", 0))
}

func requireTableExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) {
	t.Helper()
	var n int
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_name=$1 AND table_schema='public'`,
		table).Scan(&n)
	require.NoError(t, err)
	require.Equal(t, 1, n, "expected table %s to exist", table)
}

func requireColumnExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table, column string) {
	t.Helper()
	var n int
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns WHERE table_name=$1 AND column_name=$2`,
		table, column).Scan(&n)
	require.NoError(t, err)
	require.Equal(t, 1, n, "expected column %s.%s", table, column)
}
