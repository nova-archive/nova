package migrations

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/nova-archive/nova/internal/db/gen"
)

// TestBelowFloorColumnAndUpgrade verifies P2-M7.1's migration 0018 (D-M7.1-3):
// nodes.below_floor_since exists and defaults NULL, the partial index
// nodes_below_floor_idx exists, and — the lifecycle invariant, mirroring
// drain's D-M7-6d — neither the RegisterNode ON CONFLICT re-register nor the
// UpdateNodeHeartbeat liveness path touches the marker. below_floor_since is
// set/cleared ONLY by the Task 11 hysteresis sweep, never by
// register/heartbeat.
func TestBelowFloorColumnAndUpgrade(t *testing.T) {
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

	// Full forward run 0001..HEAD — the DB-upgrade half of the coverage.
	require.NoError(t, goose.UpContext(ctx, sqlDB, "."))

	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	const nodeID = "22222222-2222-2222-2222-222222222222"
	registerTestNode(t, ctx, pool, nodeID)

	// Column exists, defaults NULL for a freshly registered node.
	var belowFloor *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT below_floor_since FROM nodes WHERE id = $1`, nodeID).Scan(&belowFloor))
	require.Nil(t, belowFloor, "fresh node must not be below floor")

	// Partial index exists.
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes
		 WHERE tablename = 'nodes' AND indexname = 'nodes_below_floor_idx'`).Scan(&n))
	require.Equal(t, 1, n, "nodes_below_floor_idx must exist")

	// Mark below floor, then re-register (the real ON CONFLICT upsert): the
	// marker must survive — register never touches it (D-M7.1-3).
	_, err = pool.Exec(ctx,
		`UPDATE nodes SET below_floor_since = now() WHERE id = $1`, nodeID)
	require.NoError(t, err)

	registerTestNode(t, ctx, pool, nodeID)

	require.NoError(t, pool.QueryRow(ctx,
		`SELECT below_floor_since FROM nodes WHERE id = $1`, nodeID).Scan(&belowFloor))
	require.NotNil(t, belowFloor, "re-register must not clear below_floor_since (D-M7.1-3)")

	// The heartbeat liveness path must not touch it either.
	var pgID pgtype.UUID
	require.NoError(t, pgID.Scan(nodeID))
	_, err = gen.New(pool).UpdateNodeHeartbeat(ctx, gen.UpdateNodeHeartbeatParams{
		ID:              pgID,
		LastFreeBytes:   pgtype.Int8{Int64: 1 << 29, Valid: true},
		LastStoredBytes: pgtype.Int8{Int64: 1 << 28, Valid: true},
	})
	require.NoError(t, err)

	require.NoError(t, pool.QueryRow(ctx,
		`SELECT below_floor_since FROM nodes WHERE id = $1`, nodeID).Scan(&belowFloor))
	require.NotNil(t, belowFloor, "heartbeat must not clear below_floor_since (D-M7.1-3)")
}
