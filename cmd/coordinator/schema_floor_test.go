package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/nova-archive/nova/internal/db/migrations"
)

// postgresImage is pinned by digest as resolved 2026-08-11 (P2-M7.3).
const postgresImage = "postgres:16-alpine@sha256:57c72fd2a128e416c7fcc499958864df5301e940bca0a56f58fddf30ffc07777"

// TestCoordinatorRefusesStaleSchema (P2-M7.3, D-M7.3-9a).
//
// NOVA_MIGRATE_ON_START=false hands migration control to the operator. Without
// this floor, the coordinator starts against a schema missing the tables its
// queries name, reports itself healthy, and fails per-request hours later with
// nothing pointing back at the upgrade.
func TestCoordinatorRefusesStaleSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	container, err := postgres.Run(ctx, postgresImage,
		postgres.WithDatabase("nova"),
		postgres.WithUsername("nova"),
		postgres.WithPassword("test-password"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		shutdown, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = container.Terminate(shutdown)
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	sqlDB, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, goose.SetDialect("postgres"))
	goose.SetBaseFS(migrations.Migrations)

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	var want int64
	for _, o := range migrations.All() {
		if o.Schema > want {
			want = o.Schema
		}
	}

	// One behind is enough. The failure is not "very stale", it is "not the
	// schema this binary's queries were written against".
	require.NoError(t, goose.UpToContext(ctx, sqlDB, ".", want-1))

	err = assertSchemaIsCurrent(ctx, pool)
	if err == nil {
		t.Fatal("the coordinator must refuse to start one migration behind")
	}
	for _, want := range []string{"refusing to start", "migrate apply --to"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not contain %q:\n%v", want, err)
		}
	}

	// Current is fine.
	require.NoError(t, goose.UpToContext(ctx, sqlDB, ".", want))
	require.NoError(t, assertSchemaIsCurrent(ctx, pool))
}

// TestCoordinatorDoesNotRefuseWhenItCannotTell. A database this process cannot
// introspect is not evidence of a stale schema, and refusing on it would turn a
// permissions problem into an outage.
func TestCoordinatorDoesNotRefuseWhenItCannotTell(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	container, err := postgres.Run(ctx, postgresImage,
		postgres.WithDatabase("nova"),
		postgres.WithUsername("nova"),
		postgres.WithPassword("test-password"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		shutdown, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = container.Terminate(shutdown)
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// No goose table at all: the query errors, and the floor warns rather than
	// refusing.
	if err := assertSchemaIsCurrent(ctx, pool); err != nil {
		t.Fatalf("an unreadable schema version must warn, not refuse: %v", err)
	}
}
