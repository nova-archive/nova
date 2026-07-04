package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// TestPartitionProvisioning verifies P2-M7.1's migration 0017 (D-M7.1-2):
// 0002/0003 committed FIXED monthly partitions ending 2026-07-01, so a fresh
// install in any later month was insert-broken for jobs, integrity_audits and
// audit_log until the runtime Maintainer ran (and nothing create-aheads jobs
// at all). After a full `migrate up` on a fresh database:
//   - an INSERT with now() timestamps must succeed on all three parents, and
//   - month-relative partitions parent_YYYY_MM must exist for the current UTC
//     month plus the next two (the retention.go lookahead convention).
func TestPartitionProvisioning(t *testing.T) {
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

	// Full forward run 0001..HEAD on a fresh database.
	require.NoError(t, goose.UpContext(ctx, sqlDB, "."))

	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// Partitions must exist for the current UTC month + the 2-month lookahead,
	// for every monthly-partitioned parent.
	now := time.Now().UTC()
	base := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	for _, parent := range []string{"jobs", "integrity_audits", "audit_log"} {
		for i := 0; i <= 2; i++ {
			m := base.AddDate(0, i, 0)
			name := fmt.Sprintf("%s_%04d_%02d", parent, m.Year(), int(m.Month()))
			var n int
			require.NoError(t, pool.QueryRow(ctx,
				`SELECT count(*) FROM pg_class WHERE relname = $1`, name).Scan(&n))
			require.Equal(t, 1, n, "partition %s must exist after migrate up", name)
		}
	}

	// The real proof: inserts with now() timestamps land in a live partition.
	_, err = pool.Exec(ctx, `INSERT INTO jobs (kind) VALUES ('noop')`)
	require.NoError(t, err, "jobs insert with created_at=now() must succeed on a fresh install")

	_, err = pool.Exec(ctx,
		`INSERT INTO integrity_audits (cid, audit_kind, result)
		 VALUES ('bafyProvision', 'kubo_pin_present', 'pass')`)
	require.NoError(t, err, "integrity_audits insert with audited_at=now() must succeed on a fresh install")

	_, err = pool.Exec(ctx,
		`INSERT INTO audit_log (action, target_type, target_id)
		 VALUES ('test.provision', 'cid', 'bafyProvision')`)
	require.NoError(t, err, "audit_log insert with at=now() must succeed on a fresh install")
}
