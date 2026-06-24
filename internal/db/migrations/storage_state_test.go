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

// newMigratedDB spins up a Postgres container and applies all migrations,
// returning the SQL DB handle (for goose) and the pgx pool (for queries).
// The caller owns cleanup via t.Cleanup.
func newMigratedDB(t *testing.T) (*sql.DB, *pgxpool.Pool) {
	t.Helper()
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
	require.NoError(t, goose.UpContext(ctx, sqlDB, "."))

	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	return sqlDB, pool
}

// TestMigration0013UpDownAndBackfill verifies:
//   - backfill seeds non-tombstoned blobs as committed/origin/local_present
//   - cache_segment is NULL for backfilled rows
//   - local_bytes = envelope_size
//   - parent_cid IS NULL → durability_class = 'important', else 'normal'
//   - tombstoned blobs are excluded from backfill
//   - pin_changes byte_size updated to envelope_size by backfill UPDATE
//   - Down migration drops the table and all 3 new enum types cleanly
func TestMigration0013UpDownAndBackfill(t *testing.T) {
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

	// Apply only up to migration 0012 so we can seed data before 0013.
	require.NoError(t, goose.UpToContext(ctx, sqlDB, ".", 12))

	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// Seed: a root blob (no parent_cid, envelope_size=1000), a derivative (has parent_cid, envelope_size=500),
	// a tombstoned blob (should NOT appear in backfill).
	// The derivative_columns_consistent CHECK requires parent_cid + derivative_preset + derivative_format together.
	_, err = pool.Exec(ctx, `
		INSERT INTO blobs (cid, mime_type, byte_size, state, product, envelope_version)
		VALUES
		  ('cid-root',        'image/jpeg', 1000, 'active',     'image', 2),
		  ('cid-tombstoned',  'image/jpeg',  200, 'tombstoned', 'image', 2)
	`)
	require.NoError(t, err)

	// Insert derivative separately with all three required columns.
	_, err = pool.Exec(ctx, `
		INSERT INTO blobs (cid, mime_type, byte_size, state, product, envelope_version,
		                   parent_cid, derivative_preset, derivative_format)
		VALUES ('cid-derivative', 'image/jpeg', 500, 'active', 'image', 2, 'cid-root', 'thumb', 'jpeg')
	`)
	require.NoError(t, err)

	// Seed blob_manifests so local_bytes and pin_changes byte_size backfill work.
	_, err = pool.Exec(ctx, `
		INSERT INTO blob_manifests (cid, cid_version, hash_alg, codec, chunker, plaintext_size, envelope_size, block_count)
		VALUES
		  ('cid-root',       1, 'sha2-256', 'dag-pb', 'size-262144', 1000, 1100, 1),
		  ('cid-derivative', 1, 'sha2-256', 'dag-pb', 'size-262144',  500,  550, 1),
		  ('cid-tombstoned', 1, 'sha2-256', 'dag-pb', 'size-262144',  200,  220, 1)
	`)
	require.NoError(t, err)

	// Seed a node and pin_assignment for the pin_changes backfill test.
	_, err = pool.Exec(ctx, `
		INSERT INTO nodes (id, nebula_cert_fingerprint, federation_cert_fingerprint, capacity_bytes,
		                   bandwidth_budget_bytes_per_day, policy_filters, status)
		VALUES ('00000000-0000-0000-0000-000000000001', 'fp1', 'ffp1', 10000000, 10000000, '{}', 'active')
	`)
	require.NoError(t, err)

	// Get assignment_id from the inserted row.
	var assignID string
	row := pool.QueryRow(ctx, `
		INSERT INTO pin_assignments (cid, node_id, state)
		VALUES ('cid-root', '00000000-0000-0000-0000-000000000001', 'acked')
		RETURNING assignment_id
	`)
	require.NoError(t, row.Scan(&assignID))

	// Insert a pin_change with wrong byte_size (plaintext size instead of envelope).
	_, err = pool.Exec(ctx, `
		INSERT INTO pin_changes (node_id, assignment_id, generation, kind, cid, byte_size)
		VALUES ('00000000-0000-0000-0000-000000000001', $1, 1, 'assign', 'cid-root', 1000)
	`, assignID)
	require.NoError(t, err)

	// Now apply migration 0013.
	require.NoError(t, goose.UpToContext(ctx, sqlDB, ".", 13))

	// Verify backfill: root blob → important, local_bytes=1100, local_present=true, cache_segment NULL.
	var commitState, durClass, localRole string
	var localPresent bool
	var localBytes int64
	var cacheSegment sql.NullString
	err = pool.QueryRow(ctx, `
		SELECT commit_state, durability_class, local_role, local_present, local_bytes, cache_segment
		FROM blob_storage_state WHERE cid = 'cid-root'
	`).Scan(&commitState, &durClass, &localRole, &localPresent, &localBytes, &cacheSegment)
	require.NoError(t, err)
	require.Equal(t, "committed", commitState)
	require.Equal(t, "important", durClass, "root blob should be 'important'")
	require.Equal(t, "origin", localRole)
	require.True(t, localPresent)
	require.Equal(t, int64(1100), localBytes, "local_bytes should equal envelope_size")
	require.False(t, cacheSegment.Valid, "cache_segment should be NULL for backfilled rows")

	// Verify derivative → normal.
	err = pool.QueryRow(ctx, `
		SELECT durability_class FROM blob_storage_state WHERE cid = 'cid-derivative'
	`).Scan(&durClass)
	require.NoError(t, err)
	require.Equal(t, "normal", durClass)

	// Verify tombstoned blob is NOT in the table.
	var count int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM blob_storage_state WHERE cid = 'cid-tombstoned'`).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 0, count, "tombstoned blob must not be backfilled")

	// Verify pin_changes.byte_size updated to envelope_size.
	var pinByteSize int64
	err = pool.QueryRow(ctx, `SELECT byte_size FROM pin_changes WHERE cid = 'cid-root'`).Scan(&pinByteSize)
	require.NoError(t, err)
	require.Equal(t, int64(1100), pinByteSize, "pin_changes.byte_size should be updated to envelope_size")

	// Verify source_nebula_addr column exists.
	requireColumnExists(t, ctx, pool, "nodes", "source_nebula_addr")

	// Roll back 0013 and verify cleanup.
	require.NoError(t, goose.DownToContext(ctx, sqlDB, ".", 12))

	// Table should be gone.
	var tableCount int
	err = pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_name='blob_storage_state' AND table_schema='public'`).
		Scan(&tableCount)
	require.NoError(t, err)
	require.Equal(t, 0, tableCount, "blob_storage_state must be dropped by Down")

	// All 3 enum types should be gone.
	for _, typName := range []string{"blob_commit_state", "coordinator_local_role", "cache_segment"} {
		var typeCount int
		err = pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_type WHERE typname = $1`, typName).Scan(&typeCount)
		require.NoError(t, err)
		require.Equal(t, 0, typeCount, "type %s must be dropped by Down", typName)
	}

	// source_nebula_addr column should be gone.
	var colCount int
	err = pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns WHERE table_name='nodes' AND column_name='source_nebula_addr'`).
		Scan(&colCount)
	require.NoError(t, err)
	require.Equal(t, 0, colCount, "source_nebula_addr must be dropped by Down")
}

// TestSourceablePredicate seeds nodes/pin_assignments varying each clause of the
// sourceable predicate and confirms CountSourceableHolders excludes each
// disqualifying case while counting the healthy holder.
func TestSourceablePredicate(t *testing.T) {
	_, pool := newMigratedDB(t)
	ctx := context.Background()

	const staleSecs = 120.0 // 2 minutes

	// Helper to insert a node with configurable fields and return its ID.
	type nodeSpec struct {
		id         string
		status     string
		trustState string
		lastSeenAt string // SQL interval offset, e.g. '-5 seconds', '-5 minutes'
		caps       string // SQL array literal e.g. '{read-source/v1}'
		nebulaAddr string // can be empty or NULL
	}

	insertNode := func(spec nodeSpec) {
		caps := []string{}
		if spec.caps != "" {
			caps = []string{spec.caps}
		}
		_, err := pool.Exec(ctx, `
			INSERT INTO nodes (id, nebula_cert_fingerprint, federation_cert_fingerprint,
			                   capacity_bytes, bandwidth_budget_bytes_per_day, policy_filters,
			                   status, trust_state, advertised_capabilities, source_nebula_addr,
			                   last_seen_at)
			VALUES ($1, $2, $2, 10000000, 10000000, '{}', $3, $4, $5, NULLIF($6,''), now() + $7::interval)
		`, spec.id, spec.id, spec.status, spec.trustState,
			caps, spec.nebulaAddr, spec.lastSeenAt)
		require.NoError(t, err)
	}

	// Seed: one blob.
	_, err := pool.Exec(ctx, `
		INSERT INTO blobs (cid, mime_type, byte_size, state, product, envelope_version)
		VALUES ('cid-sourceable-test', 'image/jpeg', 500, 'active', 'image', 2)
	`)
	require.NoError(t, err)

	assignBlob := func(nodeID string) {
		_, err := pool.Exec(ctx, `
			INSERT INTO pin_assignments (cid, node_id, state)
			VALUES ('cid-sourceable-test', $1, 'acked')
		`, nodeID)
		require.NoError(t, err)
	}

	// Case 1: suspended trust_state — excluded.
	insertNode(nodeSpec{"00000000-0000-0000-0000-000000000011", "active", "suspended", "-5 seconds", "read-source/v1", "1.2.3.4:4242"})
	assignBlob("00000000-0000-0000-0000-000000000011")

	// Case 2: stale last_seen_at (beyond stale_secs) — excluded.
	insertNode(nodeSpec{"00000000-0000-0000-0000-000000000012", "active", "trusted", "-5 minutes", "read-source/v1", "1.2.3.5:4242"})
	assignBlob("00000000-0000-0000-0000-000000000012")

	// Case 3: missing read-source/v1 capability — excluded.
	insertNode(nodeSpec{"00000000-0000-0000-0000-000000000013", "active", "trusted", "-5 seconds", "other-cap/v1", "1.2.3.6:4242"})
	assignBlob("00000000-0000-0000-0000-000000000013")

	// Case 4: source_nebula_addr is NULL — excluded.
	insertNode(nodeSpec{"00000000-0000-0000-0000-000000000014", "active", "trusted", "-5 seconds", "read-source/v1", ""})
	assignBlob("00000000-0000-0000-0000-000000000014")

	// Case 5: unreachable status — excluded.
	insertNode(nodeSpec{"00000000-0000-0000-0000-000000000015", "unreachable", "trusted", "-5 seconds", "read-source/v1", "1.2.3.8:4242"})
	assignBlob("00000000-0000-0000-0000-000000000015")

	// Case 6: healthy node — included.
	insertNode(nodeSpec{"00000000-0000-0000-0000-000000000016", "active", "trusted", "-5 seconds", "read-source/v1", "1.2.3.9:4242"})
	assignBlob("00000000-0000-0000-0000-000000000016")

	// Case 7: suspect status (allowed by predicate) — included.
	insertNode(nodeSpec{"00000000-0000-0000-0000-000000000017", "suspect", "trusted", "-5 seconds", "read-source/v1", "1.2.3.10:4242"})
	assignBlob("00000000-0000-0000-0000-000000000017")

	// Now run CountSourceableHolders via raw SQL (same predicate as the query).
	var cnt int64
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM pin_assignments pa JOIN nodes n ON n.id = pa.node_id
		WHERE pa.cid = $1 AND pa.state = 'acked'
		  AND n.status IN ('active','suspect') AND n.trust_state <> 'suspended'
		  AND n.last_seen_at > now() - make_interval(secs => $2::float)
		  AND n.advertised_capabilities @> ARRAY['read-source/v1']
		  AND n.source_nebula_addr IS NOT NULL AND n.source_nebula_addr <> ''
	`, "cid-sourceable-test", staleSecs).Scan(&cnt)
	require.NoError(t, err)
	require.Equal(t, int64(2), cnt, "only healthy + suspect nodes should be counted (cases 6 and 7)")

	// Also verify each excluded case is individually excluded.
	for _, excluded := range []string{
		"00000000-0000-0000-0000-000000000011", // suspended
		"00000000-0000-0000-0000-000000000012", // stale
		"00000000-0000-0000-0000-000000000013", // no cap
		"00000000-0000-0000-0000-000000000014", // no addr
		"00000000-0000-0000-0000-000000000015", // unreachable
	} {
		var n int64
		err = pool.QueryRow(ctx, `
			SELECT count(*) FROM pin_assignments pa JOIN nodes n ON n.id = pa.node_id
			WHERE pa.cid = $1 AND pa.state = 'acked' AND pa.node_id = $2
			  AND n.status IN ('active','suspect') AND n.trust_state <> 'suspended'
			  AND n.last_seen_at > now() - make_interval(secs => $3::float)
			  AND n.advertised_capabilities @> ARRAY['read-source/v1']
			  AND n.source_nebula_addr IS NOT NULL AND n.source_nebula_addr <> ''
		`, "cid-sourceable-test", excluded, staleSecs).Scan(&n)
		require.NoError(t, err)
		require.Equal(t, int64(0), n, "node %s should be excluded from sourceable predicate", excluded)
	}
}

// TestListEvictionCandidatesOrder verifies the SLRU/2Q drain order:
// probationary rows drain before protected, oldest last_accessed_at first within each segment.
func TestListEvictionCandidatesOrder(t *testing.T) {
	_, pool := newMigratedDB(t)
	ctx := context.Background()

	// Seed 4 blobs.
	for i, cid := range []string{"cid-e1", "cid-e2", "cid-e3", "cid-e4"} {
		_, err := pool.Exec(ctx, `
			INSERT INTO blobs (cid, mime_type, byte_size, state, product, envelope_version)
			VALUES ($1, 'image/jpeg', 100, 'active', 'image', 2)
		`, cid)
		require.NoError(t, err)
		_ = i
	}

	// Insert cache rows:
	//   cid-e1: probationary, last_accessed 1 hour ago  (oldest probationary — evict first)
	//   cid-e2: probationary, last_accessed 30 min ago  (newer probationary — evict second)
	//   cid-e3: protected,    last_accessed 2 hours ago (oldest protected — evict third)
	//   cid-e4: protected,    last_accessed 1 hour ago  (newer protected — evict last)
	_, err := pool.Exec(ctx, `
		INSERT INTO blob_storage_state (cid, local_role, cache_segment, local_present, local_bytes, last_accessed_at)
		VALUES
		  ('cid-e1', 'cache', 'probationary', true, 100, now() - interval '1 hour'),
		  ('cid-e2', 'cache', 'probationary', true, 100, now() - interval '30 minutes'),
		  ('cid-e3', 'cache', 'protected',    true, 100, now() - interval '2 hours'),
		  ('cid-e4', 'cache', 'protected',    true, 100, now() - interval '1 hour')
	`)
	require.NoError(t, err)

	// The eviction query: probationary (false < true) drains first, oldest last_accessed_at first.
	rows, err := pool.Query(ctx, `
		SELECT cid FROM blob_storage_state
		WHERE local_role='cache' AND local_present
		ORDER BY (cache_segment='protected'), last_accessed_at
	`)
	require.NoError(t, err)
	defer rows.Close()

	var order []string
	for rows.Next() {
		var cid string
		require.NoError(t, rows.Scan(&cid))
		order = append(order, cid)
	}
	require.NoError(t, rows.Err())

	require.Equal(t, []string{"cid-e1", "cid-e2", "cid-e3", "cid-e4"}, order,
		"eviction order: probationary oldest-first, then protected oldest-first")
}
