package secret_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nova-archive/nova/internal/secret"
	"github.com/stretchr/testify/require"
)

func TestResolverPrefersEnvVar(t *testing.T) {
	t.Setenv("FOO", "from-env")
	t.Setenv("FOO_FILE", filepath.Join(t.TempDir(), "ignored"))
	got, src, err := secret.ResolveSecret("FOO", "FOO_FILE", "/dev/null")
	require.NoError(t, err)
	require.Equal(t, "from-env", got)
	require.Equal(t, secret.SourceEnv, src)
}

func TestResolverFallsBackToFileEnv(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "secret.txt")
	require.NoError(t, os.WriteFile(file, []byte("from-file-env\n"), 0o600))
	t.Setenv("BAR", "")
	t.Setenv("BAR_FILE", file)
	got, src, err := secret.ResolveSecret("BAR", "BAR_FILE", "/dev/null")
	require.NoError(t, err)
	require.Equal(t, "from-file-env", got, "trailing newline trimmed")
	require.Equal(t, secret.SourceFileEnv, src)
}

func TestResolverFallsBackToDefaultMountPath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "secret.txt")
	require.NoError(t, os.WriteFile(file, []byte("from-mount"), 0o600))
	t.Setenv("BAZ", "")
	t.Setenv("BAZ_FILE", "")
	got, src, err := secret.ResolveSecret("BAZ", "BAZ_FILE", file)
	require.NoError(t, err)
	require.Equal(t, "from-mount", got)
	require.Equal(t, secret.SourceMount, src)
}

func TestResolverErrorsWhenNoneAvailable(t *testing.T) {
	t.Setenv("QUUX", "")
	t.Setenv("QUUX_FILE", "")
	_, _, err := secret.ResolveSecret("QUUX", "QUUX_FILE", "/nonexistent")
	require.Error(t, err)
}
