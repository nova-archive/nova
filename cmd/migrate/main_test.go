package main_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// repoRoot finds the project root by walking up from this test file.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	require.True(t, ok)
	dir := filepath.Dir(here)
	for i := 0; i < 5; i++ {
		if _, err := exec.Command("test", "-f", filepath.Join(dir, "go.mod")).Output(); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("repo root not found")
	return ""
}

func TestIntegrationMigrateUpProducesExpectedTables(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	container, err := postgres.RunContainer(ctx,
		postgres.WithDatabase("nova"),
		postgres.WithUsername("nova"),
		postgres.WithPassword("test-password"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	root := repoRoot(t)
	build := exec.Command("go", "build", "-o", filepath.Join(root, "bin/migrate-test"), "./cmd/migrate")
	build.Dir = root
	require.NoError(t, build.Run())

	run := exec.Command(filepath.Join(root, "bin/migrate-test"), "up")
	run.Env = append(run.Env, "DATABASE_URL="+dsn)
	out, err := run.CombinedOutput()
	require.NoError(t, err, "migrate output: %s", out)

	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close(ctx)

	expectedTables := []string{
		"users", "master_key_versions", "data_encryption_keys", "signing_keys",
		"collections", "blobs", "blob_manifests", "blob_blocks",
		"image_metadata", "collection_items",
		"nodes", "pin_assignments", "pin_audits",
		"integrity_audits", "moderation_decisions", "dmca_cases",
		"takedown_repeat_infringers", "signed_url_revocations",
		"audit_log", "jobs", "upload_sessions",
	}
	for _, table := range expectedTables {
		var exists bool
		err := conn.QueryRow(ctx,
			`SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				 WHERE table_schema='public' AND table_name=$1
			)`, table).Scan(&exists)
		require.NoError(t, err, "table %s", table)
		require.True(t, exists, "expected table %s to exist after migrate up", table)
	}

	// envelope_version column from migration 0004
	var hasCol bool
	err = conn.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			 WHERE table_name='blobs' AND column_name='envelope_version'
		)`).Scan(&hasCol)
	require.NoError(t, err)
	require.True(t, hasCol, "expected blobs.envelope_version to exist")

	// integrity_audits is partitioned
	var isPartitioned bool
	err = conn.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM pg_partitioned_table pt
			 JOIN pg_class c ON c.oid = pt.partrelid
			 WHERE c.relname = 'integrity_audits'
		)`).Scan(&isPartitioned)
	require.NoError(t, err)
	require.True(t, isPartitioned, "expected integrity_audits to be partitioned")
}
