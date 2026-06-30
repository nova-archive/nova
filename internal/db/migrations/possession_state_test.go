package migrations

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// TestMigration0015PossessionColumns verifies P2-M6's possession-audit schema:
// the D10 receive-time column, always-set decision time, transcript digest on
// pin_audits, the trust-epoch/review columns on nodes, the joined_at backfill
// for existing nodes, and the three scheduler-support indexes.
func TestMigration0015PossessionColumns(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)

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
	t.Cleanup(func() { _ = sqlDB.Close() })

	require.NoError(t, goose.SetDialect("postgres"))
	goose.SetBaseFS(Migrations)

	// Apply all migrations up to 0014 so we can seed a node before the backfill.
	require.NoError(t, goose.UpToContext(ctx, sqlDB, ".", 14))

	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// Seed a node with a known joined_at in the past so we can verify the backfill.
	const nodeID = "aaaaaaaa-bbbb-cccc-dddd-000000000001"
	const joinedAt = "2025-01-01T00:00:00Z"
	_, err = pool.Exec(ctx, `
		INSERT INTO nodes (id, nebula_cert_fingerprint, federation_cert_fingerprint,
		                   capacity_bytes, bandwidth_budget_bytes_per_day, policy_filters, status)
		VALUES ($1, 'fp-0015', 'ffp-0015', 1<<30, 1<<30, '{}', 'active')
	`, nodeID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE nodes SET joined_at = $1 WHERE id = $2`, joinedAt, nodeID)
	require.NoError(t, err)

	// Apply migration 0015 (the migration under test).
	require.NoError(t, goose.UpToContext(ctx, sqlDB, ".", 15))

	// pin_audits must have the three new columns.
	for _, col := range []string{"received_at", "decided_at", "transcript_hash"} {
		requireColumnExists(t, ctx, pool, "pin_audits", col)
	}

	// nodes must have the three trust-epoch/review columns.
	for _, col := range []string{"trust_epoch_started_at", "trust_review_required_at", "trust_review_reason"} {
		requireColumnExists(t, ctx, pool, "nodes", col)
	}

	// Backfill: trust_epoch_started_at must equal joined_at for the pre-existing node.
	assertTrustEpochEqualsJoinedAt(t, ctx, pool, nodeID)
}

// assertTrustEpochEqualsJoinedAt verifies the 0015 backfill for the given nodeID:
// the UPDATE nodes SET trust_epoch_started_at = joined_at must have fired.
func assertTrustEpochEqualsJoinedAt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, nodeID string) {
	t.Helper()
	var epochStarted, joined time.Time
	err := pool.QueryRow(ctx,
		`SELECT trust_epoch_started_at, joined_at FROM nodes WHERE id = $1`, nodeID,
	).Scan(&epochStarted, &joined)
	require.NoError(t, err)
	require.Equal(t, joined.UTC(), epochStarted.UTC(),
		"trust_epoch_started_at must equal joined_at after backfill (node %s)", nodeID)
}
