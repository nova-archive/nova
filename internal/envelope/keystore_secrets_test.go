package envelope

// White-box tests for the master-key secret-resolution chain (env → _FILE →
// /run/secrets/master-key-<label>). These swap the unexported defaultSecretsDir,
// which is global mutable state, so this file must NOT use t.Parallel().

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func hexKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, KeySize)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

// useSecretsDir points defaultSecretsDir at a fresh temp dir for this test and
// restores it on cleanup. Returns the dir.
func useSecretsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig := defaultSecretsDir
	defaultSecretsDir = dir
	t.Cleanup(func() { defaultSecretsDir = orig })
	return dir
}

func TestKeystoreActiveFromInlineEnv(t *testing.T) {
	useSecretsDir(t)
	t.Setenv("NOVA_MASTER_KEY_V1", hexKey(t))
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := NewKeystoreFromEnv(nil)
	require.NoError(t, err)
	require.Equal(t, "v1", ks.ActiveLabel())
	require.Len(t, ks.masters["v1"], KeySize)
}

func TestKeystoreActiveFromFileEnv(t *testing.T) {
	dir := useSecretsDir(t)
	path := filepath.Join(dir, "v1.key")
	require.NoError(t, os.WriteFile(path, []byte(hexKey(t)+"\n"), 0o600)) // trailing newline trimmed
	t.Setenv("NOVA_MASTER_KEY_V1_FILE", path)
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := NewKeystoreFromEnv(nil)
	require.NoError(t, err)
	require.Len(t, ks.masters["v1"], KeySize)
}

func TestKeystoreActiveFromDefaultMountPath(t *testing.T) {
	dir := useSecretsDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "master-key-v1"), []byte(hexKey(t)), 0o600))
	// Only ACTIVE is set; key material is purely the convention mount file.
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := NewKeystoreFromEnv(nil)
	require.NoError(t, err)
	require.Len(t, ks.masters["v1"], KeySize)
}

func TestKeystoreDefaultMountPathIsLowercased(t *testing.T) {
	dir := useSecretsDir(t)
	// ACTIVE label "V1" (uppercase) must resolve to master-key-v1 (lowercase).
	require.NoError(t, os.WriteFile(filepath.Join(dir, "master-key-v1"), []byte(hexKey(t)), 0o600))
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "V1")
	ks, err := NewKeystoreFromEnv(nil)
	require.NoError(t, err)
	require.Equal(t, "v1", ks.ActiveLabel())
	require.Len(t, ks.masters["v1"], KeySize)
}

func TestKeystoreMultiLabelMixedSources(t *testing.T) {
	dir := useSecretsDir(t)
	v2path := filepath.Join(dir, "v2.key")
	require.NoError(t, os.WriteFile(v2path, []byte(hexKey(t)), 0o600))
	t.Setenv("NOVA_MASTER_KEY_V1", hexKey(t))   // inline
	t.Setenv("NOVA_MASTER_KEY_V2_FILE", v2path) // file
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := NewKeystoreFromEnv(nil)
	require.NoError(t, err)
	require.Len(t, ks.masters["v1"], KeySize)
	require.Len(t, ks.masters["v2"], KeySize)
}

func TestKeystoreUnreadableFileEnvRefuses(t *testing.T) {
	useSecretsDir(t)
	t.Setenv("NOVA_MASTER_KEY_V1_FILE", filepath.Join(t.TempDir(), "does-not-exist"))
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	_, err := NewKeystoreFromEnv(nil)
	require.Error(t, err, "a declared but unreadable _FILE must refuse to start")
}

func TestKeystoreActiveNoSourceRefuses(t *testing.T) {
	useSecretsDir(t) // empty dir → no default mount file
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	_, err := NewKeystoreFromEnv(nil)
	require.Error(t, err)
}

func TestKeystoreBadHexFromFileRefuses(t *testing.T) {
	dir := useSecretsDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "master-key-v1"), []byte("not-hex!!"), 0o600))
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	_, err := NewKeystoreFromEnv(nil)
	require.Error(t, err)
}
