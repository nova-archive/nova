package migrations

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// postgresImage is pinned by digest here rather than by tag (P2-M7.3). This
// milestone is about knowing exactly which bytes produced a result, and a test
// that silently changes its own Postgres out from under a schema assertion is
// the same class of problem at a smaller scale.
//
// postgres:16-alpine as resolved 2026-08-11.
const postgresImage = "postgres:16-alpine@sha256:57c72fd2a128e416c7fcc499958864df5301e940bca0a56f58fddf30ffc07777"

// migratedPool runs the FULL forward sequence against a fresh container and
// returns a pool. This test lives in `package migrations` and cannot use
// internal/dbtest, which imports this package.
func migratedPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()

	container, err := postgres.Run(ctx, postgresImage,
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
	return pool
}

func seedNodeRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, caps []string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(ctx, `
		INSERT INTO nodes (id, nebula_cert_fingerprint, federation_cert_fingerprint,
		                   capacity_bytes, bandwidth_budget_bytes_per_day, advertised_capabilities)
		VALUES ($1::uuid, 'neb-'||$1::text, 'fed-'||$1::text, 0, 0, $2::text[])`, id, caps)
	require.NoError(t, err)
	return id
}

func newRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, state string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(ctx, `
		INSERT INTO upgrade_runs (id, to_release, from_schema, to_schema, state)
		VALUES ($1::uuid, 'v0.3.0', 18, 19, $2)`, id, state)
	require.NoError(t, err)
	return id
}

// TestUpgradeRunRecordsAnInterruptedUpgrade. A flat "upgraded at T" row cannot
// describe the case the record exists for: the operator's terminal died between
// migrate and verify. The run carries phases, and 'interrupted' is a state a
// later run can find and reconcile.
func TestUpgradeRunRecordsAnInterruptedUpgrade(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	pool := migratedPool(t, ctx)

	run := newRun(t, ctx, pool, "started")
	for i, ev := range []struct{ phase, state string }{
		{"preflight", "passed"},
		{"apply", "started"},
	} {
		_, err := pool.Exec(ctx, `
			INSERT INTO upgrade_events (run_id, sequence, phase, state)
			VALUES ($1::uuid, $2, $3, $4)`, run, i+1, ev.phase, ev.state)
		require.NoError(t, err)
	}

	_, err := pool.Exec(ctx,
		`UPDATE upgrade_runs SET state = 'interrupted', completed_at = now() WHERE id = $1::uuid`, run)
	require.NoError(t, err)

	var state string
	var events int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT r.state, (SELECT count(*) FROM upgrade_events e WHERE e.run_id = r.id)
		 FROM upgrade_runs r WHERE r.id = $1::uuid`, run).Scan(&state, &events))
	require.Equal(t, "interrupted", state)
	require.Equal(t, 2, events)
}

func TestUpgradeRunRejectsAnUnknownState(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	pool := migratedPool(t, ctx)

	_, err := pool.Exec(ctx, `
		INSERT INTO upgrade_runs (id, to_release, from_schema, to_schema, state)
		VALUES (gen_random_uuid(), 'v0.3.0', 18, 19, 'mostly-fine')`)
	require.Error(t, err, "an unconstrained state column would let a typo read as success")
}

// TestReplayCannotDuplicateAnEvent. The journal is replayed into upgrade_events
// after 0019 applies; a crash midway through that backfill would otherwise
// insert every already-written event a second time on restart. A bigserial id
// alone cannot express "this journal event is already here".
func TestReplayCannotDuplicateAnEvent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	pool := migratedPool(t, ctx)

	run := newRun(t, ctx, pool, "started")
	insert := func() error {
		_, err := pool.Exec(ctx, `
			INSERT INTO upgrade_events (run_id, sequence, phase, state)
			VALUES ($1::uuid, 7, 'apply', 'started')`, run)
		return err
	}
	require.NoError(t, insert())
	require.Error(t, insert(), "(run_id, sequence) must reject a duplicate replay")

	// And the idempotent form the backfill actually uses converges.
	_, err := pool.Exec(ctx, `
		INSERT INTO upgrade_events (run_id, sequence, phase, state)
		VALUES ($1::uuid, 7, 'apply', 'started')
		ON CONFLICT (run_id, sequence) DO NOTHING`, run)
	require.NoError(t, err)

	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM upgrade_events WHERE run_id = $1::uuid`, run).Scan(&n))
	require.Equal(t, 1, n)
}

// TestDeletingARunCannotEraseItsEvents. Upgrade history is evidence (T1.24).
// ON DELETE CASCADE would make removing the record of a bad upgrade a one-liner.
func TestDeletingARunCannotEraseItsEvents(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	pool := migratedPool(t, ctx)

	run := newRun(t, ctx, pool, "failed")
	_, err := pool.Exec(ctx, `
		INSERT INTO upgrade_events (run_id, sequence, phase, state)
		VALUES ($1::uuid, 1, 'apply', 'failed')`, run)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `DELETE FROM upgrade_runs WHERE id = $1::uuid`, run)
	require.Error(t, err, "deleting a run must not cascade away its own events")

	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM upgrade_events WHERE run_id = $1::uuid`, run).Scan(&n))
	require.Equal(t, 1, n)
}

// TestNodeMetadataColumnsExistAndSeparateTheirConcerns.
func TestNodeMetadataColumnsExistAndSeparateTheirConcerns(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	pool := migratedPool(t, ctx)

	want := []string{
		// identity claims — census only
		"reported_client_version", "reported_image_digest", "reported_bundle_lock_digest",
		// capability advertisements — gate routing
		"effective_capabilities", "reported_protocols",
		// the marker that separates legacy omission from rollback
		"runtime_contract_observed_at",
		// what the operator authorized
		"expected_image_digest", "expected_bundle_lock_digest", "expected_at", "expected_by",
	}
	for _, col := range want {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM information_schema.columns
			               WHERE table_name = 'nodes' AND column_name = $1)`, col).Scan(&exists))
		require.Truef(t, exists, "nodes.%s is missing", col)
	}

	// The legacy column stays: an old registration still writes it.
	var legacy bool
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_name = 'nodes' AND column_name = 'client_version')`).Scan(&legacy))
	require.True(t, legacy, "nodes.client_version must survive; old registrations still write it")

	// The marker starts NULL, which is what makes a pre-M7.3 donor's silence
	// mean "legacy" rather than "rolled back".
	id := seedNodeRow(t, ctx, pool, []string{"pin-change-log/v1"})
	var observed *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT runtime_contract_observed_at FROM nodes WHERE id = $1::uuid`, id).Scan(&observed))
	require.Nil(t, observed)
}

// TestEffectiveCapabilitiesAreBackfilled. effective_capabilities becomes the
// single operational source, so every node that existed before 0019 must carry
// its advertised set forward — otherwise the migration silently makes every
// existing donor ineligible for every capability-gated role at once.
func TestEffectiveCapabilitiesAreBackfilled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)

	// Bring the schema to 18 first, insert a pre-0019 donor, then cross to 19.
	container, err := postgres.Run(ctx, postgresImage,
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
	require.NoError(t, goose.UpToContext(ctx, sqlDB, ".", 18))

	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	caps := []string{"pin-change-log/v1", "snapshot/v1", "read-source/v1"}
	id := seedNodeRow(t, ctx, pool, caps)

	require.NoError(t, goose.UpContext(ctx, sqlDB, "."))

	var effective []string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT effective_capabilities FROM nodes WHERE id = $1::uuid`, id).Scan(&effective))
	require.ElementsMatch(t, caps, effective,
		"a donor that existed before 0019 must keep its capabilities; an empty "+
			"effective set would make it ineligible for every gated role at once")
}

// Test0019DeclaresItsObligations: the classification table and the shipped
// migrations are checked against each other in obligations_test, but 0019's
// own values are load-bearing for auto-apply and are asserted explicitly.
func Test0019DeclaresItsObligations(t *testing.T) {
	o, err := Lookup("0019_upgrade_runs.sql")
	require.NoError(t, err)
	require.True(t, o.OldBinaryCompatible,
		"a baseline coordinator must run against schema 19; that is what makes a "+
			"coordinator rollback survivable")
	require.Equal(t, "0018", o.RelativeTo)
	require.True(t, o.OnlineApplicable)
	require.False(t, o.RequiresMaintenance)
	require.False(t, o.RestoreToRevert,
		"0019 is additive: it creates two tables and adds nullable columns, so "+
			"dropping them loses nothing the baseline binary had")

	b, err := Between(18, 19)
	require.NoError(t, err)
	require.True(t, b.OldBinaryCompatible)
	require.True(t, b.OnlineApplicable)
	require.False(t, b.RequiresMaintenance)
	require.False(t, b.RestoreToRevert)
	require.Empty(t, b.Procedures,
		"the baseline transition must be auto-appliable; a procedure here would "+
			"make every existing deployment stop and read a runbook")
}
