package migrations

import (
	"io/fs"
	"testing"

	"github.com/stretchr/testify/require"
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
}

func TestMigrationsFSFirstFileHasGooseAnnotation(t *testing.T) {
	data, err := fs.ReadFile(Migrations, "0001_init.sql")
	require.NoError(t, err)
	require.Contains(t, string(data), "-- +goose Up")
	require.Contains(t, string(data), "-- +goose Down")
}
