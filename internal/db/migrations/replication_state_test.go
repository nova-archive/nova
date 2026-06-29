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

// TestMigration0014UpDownAndBackfill verifies P2-M5's liveness/healing schema:
// the blob_replication_state projection (donor-replica health, distinct from
// M4.1's blob_storage_state), the reconcile queue + webhook suppression tables,
// the D8/D9 placement + sync-state + egress-telemetry columns on nodes, and the
// repair source-binding columns on pin_assignments/pin_changes. The backfill is
// anchored on blob_storage_state (NOT pin_assignments), so an active or
// quarantined blob with ZERO donor assignments is represented as donor_lost.
func TestMigration0014UpDownAndBackfill(t *testing.T) {
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

	// Apply up to 0012, then seed blobs/manifests so the 0013 backfill populates
	// blob_storage_state (the anchor for the 0014 projection backfill).
	require.NoError(t, goose.UpToContext(ctx, sqlDB, ".", 12))

	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// active root blob with NO pin_assignments → must backfill as donor_lost/important.
	// quarantined blob → repair-eligible, must backfill. tombstoned → must NOT.
	_, err = pool.Exec(ctx, `
		INSERT INTO blobs (cid, mime_type, byte_size, state, product, envelope_version)
		VALUES
		  ('cid-active-zero', 'image/jpeg', 1000, 'active',      'image', 2),
		  ('cid-quarantined', 'image/jpeg',  800, 'quarantined', 'image', 2),
		  ('cid-tombstoned',  'image/jpeg',  200, 'tombstoned',  'image', 2)
	`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO blob_manifests (cid, cid_version, hash_alg, codec, chunker, plaintext_size, envelope_size, block_count)
		VALUES
		  ('cid-active-zero', 1, 'sha2-256', 'dag-pb', 'size-262144', 1000, 1100, 1),
		  ('cid-quarantined', 1, 'sha2-256', 'dag-pb', 'size-262144',  800,  880, 1),
		  ('cid-tombstoned',  1, 'sha2-256', 'dag-pb', 'size-262144',  200,  220, 1)
	`)
	require.NoError(t, err)

	// Apply 0013 (populates blob_storage_state for non-tombstoned blobs).
	require.NoError(t, goose.UpToContext(ctx, sqlDB, ".", 13))
	// Apply 0014 (the migration under test).
	require.NoError(t, goose.UpToContext(ctx, sqlDB, ".", 14))

	// New tables exist.
	requireTableExists(t, ctx, pool, "blob_replication_state")
	requireTableExists(t, ctx, pool, "blob_replication_reconcile_queue")
	requireTableExists(t, ctx, pool, "webhook_suppression")

	// D8/D9 + sync-state + egress-telemetry columns on nodes.
	for _, col := range []string{
		"failure_domain_id", "donor_principal_id", "provider", "asn", "region",
		"operator_verified_at", "placement_weight", "assignment_sync_state",
		"revoked_signaled_at", "last_egress_remaining_bytes",
		"last_egress_capacity_bytes", "last_egress_refill_bps",
	} {
		requireColumnExists(t, ctx, pool, "nodes", col)
	}
	// Repair source-binding columns.
	requireColumnExists(t, ctx, pool, "pin_assignments", "source_node_id")
	requireColumnExists(t, ctx, pool, "pin_assignments", "source_attempts")
	requireColumnExists(t, ctx, pool, "pin_assignments", "source_next_attempt_at")
	requireColumnExists(t, ctx, pool, "pin_changes", "source_node_id")

	// Backfill: the zero-donor active root blob is donor_lost/important, dirty,
	// healthy_acked_count 0, target_count 5 (important default).
	var safetyTier, durClass string
	var dirty bool
	var healthy, target int
	err = pool.QueryRow(ctx, `
		SELECT safety_tier, durability_class, dirty, healthy_acked_count, target_count
		FROM blob_replication_state WHERE cid = 'cid-active-zero'
	`).Scan(&safetyTier, &durClass, &dirty, &healthy, &target)
	require.NoError(t, err)
	require.Equal(t, "donor_lost", safetyTier, "zero-donor blob must be donor_lost")
	require.Equal(t, "important", durClass, "root blob class from blob_storage_state")
	require.True(t, dirty, "backfilled rows seed dirty=true for Task-2 recompute")
	require.Equal(t, 0, healthy)
	require.Equal(t, 5, target, "important target_count default is 5")

	// Quarantined blob is repair-eligible → present.
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM blob_replication_state WHERE cid = 'cid-quarantined'`).Scan(&n))
	require.Equal(t, 1, n, "quarantined blob is repair-eligible")

	// Tombstoned blob is not repair-eligible → absent.
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM blob_replication_state WHERE cid = 'cid-tombstoned'`).Scan(&n))
	require.Equal(t, 0, n, "tombstoned blob must not be backfilled")

	// source_node_id is a nullable FK: the synthetic coordinator id is never stored.
	// A NULL insert must succeed; an arbitrary non-node UUID must violate the FK.
	_, err = pool.Exec(ctx, `
		INSERT INTO nodes (id, nebula_cert_fingerprint, federation_cert_fingerprint, capacity_bytes,
		                   bandwidth_budget_bytes_per_day, policy_filters, status)
		VALUES ('00000000-0000-0000-0000-000000000001', 'fp1', 'ffp1', 1<<30, 1<<30, '{}', 'active')
	`)
	require.NoError(t, err)
	var aid string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO pin_assignments (cid, node_id, state, source_node_id)
		VALUES ('cid-active-zero', '00000000-0000-0000-0000-000000000001', 'pending', NULL)
		RETURNING assignment_id
	`).Scan(&aid))
	_, err = pool.Exec(ctx, `
		UPDATE pin_assignments SET source_node_id = '99999999-9999-9999-9999-999999999999'
		WHERE assignment_id = $1
	`, aid)
	require.Error(t, err, "a non-node UUID in source_node_id must violate the FK")

	// Down to 0013 → projection gone.
	require.NoError(t, goose.DownToContext(ctx, sqlDB, ".", 13))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_name='blob_replication_state'`).Scan(&n))
	require.Equal(t, 0, n, "down must drop blob_replication_state")
}
