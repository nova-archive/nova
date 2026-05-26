package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nova-archive/nova/internal/config"
	"github.com/stretchr/testify/require"
)

func TestSecretsResolverPrefersEnvVar(t *testing.T) {
	t.Setenv("FOO", "from-env")
	t.Setenv("FOO_FILE", filepath.Join(t.TempDir(), "ignored"))

	got, err := config.ResolveSecret("FOO", "FOO_FILE", "/dev/null")
	require.NoError(t, err)
	require.Equal(t, "from-env", got)
}

func TestSecretsResolverFallsBackToFileEnv(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "secret.txt")
	require.NoError(t, os.WriteFile(file, []byte("from-file-env\n"), 0o600))

	t.Setenv("BAR", "")
	t.Setenv("BAR_FILE", file)

	got, err := config.ResolveSecret("BAR", "BAR_FILE", "/dev/null")
	require.NoError(t, err)
	require.Equal(t, "from-file-env", got, "trailing newline should be trimmed")
}

func TestSecretsResolverFallsBackToDefaultMountPath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "secret.txt")
	require.NoError(t, os.WriteFile(file, []byte("from-mount"), 0o600))

	t.Setenv("BAZ", "")
	t.Setenv("BAZ_FILE", "")

	got, err := config.ResolveSecret("BAZ", "BAZ_FILE", file)
	require.NoError(t, err)
	require.Equal(t, "from-mount", got)
}

func TestSecretsResolverReturnsErrorWhenNoneAvailable(t *testing.T) {
	t.Setenv("QUUX", "")
	t.Setenv("QUUX_FILE", "")

	_, err := config.ResolveSecret("QUUX", "QUUX_FILE", "/nonexistent")
	require.Error(t, err)
}
