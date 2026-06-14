package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nova-archive/nova/internal/config"
	"github.com/stretchr/testify/require"
)

func minimalCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.LoadFromBytes([]byte(
		"operator:\n  hostname: h.test\n  contact_email: a@b.test\n" +
			"tls:\n  mode: dev-self-signed\n" +
			"orchestrator:\n  replication:\n    factor:\n      important: 2\n"))
	require.NoError(t, err)
	return cfg
}

func TestWriteAtomicRoundTrips(t *testing.T) {
	cfg := minimalCfg(t)
	tru := true
	cfg.Coordinator.RecordSourceIP = &tru // tri-state must survive
	path := filepath.Join(t.TempDir(), "operator.yaml")

	require.NoError(t, config.WriteAtomic(path, cfg))

	got, err := config.LoadFromFile(path)
	require.NoError(t, err)
	require.NotNil(t, got.Coordinator.RecordSourceIP)
	require.True(t, *got.Coordinator.RecordSourceIP)
	require.Equal(t, "h.test", got.Operator.Hostname)
}

func TestWriteAtomicLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "operator.yaml")
	require.NoError(t, config.WriteAtomic(path, minimalCfg(t)))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "operator.yaml", entries[0].Name())
}

func TestWriteAtomicNoInlineSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operator.yaml")
	require.NoError(t, config.WriteAtomic(path, minimalCfg(t)))
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, strings.ToLower(string(b)), "begin private key")
}

func TestMergePatchDeepMerges(t *testing.T) {
	base := map[string]any{"uploads": map[string]any{"max_upload_size_bytes": 100, "public_uploads": false}}
	patch := map[string]any{"uploads": map[string]any{"public_uploads": true}}
	out := config.MergePatch(base, patch)
	up := out["uploads"].(map[string]any)
	require.Equal(t, 100, up["max_upload_size_bytes"]) // untouched key preserved
	require.Equal(t, true, up["public_uploads"])       // patched key applied
}
