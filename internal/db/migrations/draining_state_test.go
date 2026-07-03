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

// TestDrainingColumnAndUpgrade verifies P2-M7's migration 0016 (D-M7-6a):
// nodes.draining_at exists and defaults NULL, the partial covering index
// nodes_draining_idx exists, and — the lifecycle invariant D-M7-6d — the
// RegisterNode ON CONFLICT re-register does NOT clear draining_at (drain is
// set/cleared ONLY by novactl node drain/undrain, never by register/heartbeat).
func TestDrainingColumnAndUpgrade(t *testing.T) {
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

	// Full forward run 0001..HEAD — the DB-upgrade half of D-M7-3's coverage.
	require.NoError(t, goose.UpContext(ctx, sqlDB, "."))

	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	const nodeID = "11111111-1111-1111-1111-111111111111"
	registerTestNode(t, ctx, pool, nodeID)

	// Column exists, defaults NULL for a freshly registered node.
	var draining *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT draining_at FROM nodes WHERE id = $1`, nodeID).Scan(&draining))
	require.Nil(t, draining, "fresh node must not be draining")

	// Partial covering index exists.
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes
		 WHERE tablename = 'nodes' AND indexname = 'nodes_draining_idx'`).Scan(&n))
	require.Equal(t, 1, n, "nodes_draining_idx must exist")

	// Mark draining, then re-register (the real ON CONFLICT upsert): the drain
	// marker must survive (D-M7-6d).
	_, err = pool.Exec(ctx,
		`UPDATE nodes SET draining_at = now() WHERE id = $1`, nodeID)
	require.NoError(t, err)

	registerTestNode(t, ctx, pool, nodeID)

	require.NoError(t, pool.QueryRow(ctx,
		`SELECT draining_at FROM nodes WHERE id = $1`, nodeID).Scan(&draining))
	require.NotNil(t, draining, "re-register must not clear draining_at (D-M7-6d)")
}

// registerTestNode runs the production RegisterNode upsert (insert on first
// call, ON CONFLICT re-register on the second) with minimal valid params.
func registerTestNode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) {
	t.Helper()
	var pgID pgtype.UUID
	require.NoError(t, pgID.Scan(id))
	_, err := gen.New(pool).RegisterNode(ctx, gen.RegisterNodeParams{
		ID:                         pgID,
		NebulaCertFingerprint:      "fp-0016",
		FederationCertFingerprint:  "ffp-0016",
		CapacityBytes:              1 << 30,
		BandwidthBudgetBytesPerDay: 1 << 30,
		PolicyFilters:              []byte("{}"),
		AdvertisedCapabilities:     []string{},
		RequiredCapabilities:       []string{},
	})
	require.NoError(t, err)
}
