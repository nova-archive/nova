package main

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeValidConfig(t *testing.T, healthAddr string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range []string{"fed.ca", "fed.crt", "fed.key", "neb.crt", "neb.key", "swarm.key"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600))
	}
	storage := filepath.Join(dir, "data")
	require.NoError(t, os.MkdirAll(storage, 0o700))
	cfg := "coordinator_url: https://coord.example\n" +
		"federation_ca_path: " + filepath.Join(dir, "fed.ca") + "\n" +
		"federation_cert_path: " + filepath.Join(dir, "fed.crt") + "\n" +
		"federation_key_path: " + filepath.Join(dir, "fed.key") + "\n" +
		"nebula_cert_path: " + filepath.Join(dir, "neb.crt") + "\n" +
		"nebula_key_path: " + filepath.Join(dir, "neb.key") + "\n" +
		"swarm_key_path: " + filepath.Join(dir, "swarm.key") + "\n" +
		"storage_dir: " + storage + "\n" +
		"bandwidth_budget_bytes_per_day: 53687091200\n"
	if healthAddr != "" {
		cfg += "health_listen_addr: " + healthAddr + "\n"
	}
	path := filepath.Join(dir, "node.yaml")
	require.NoError(t, os.WriteFile(path, []byte(cfg), 0o600))
	return path
}

func TestValidateAcceptsValidConfig(t *testing.T) {
	path := writeValidConfig(t, "")
	err := run([]string{"--validate", "--config", path}, io.Discard, io.Discard)
	require.NoError(t, err)
}

func TestValidateRejectsMissingConfigFlag(t *testing.T) {
	err := run([]string{"--validate"}, io.Discard, io.Discard)
	require.ErrorContains(t, err, "--config is required")
}

func TestValidateRejectsMissingCertFile(t *testing.T) {
	path := writeValidConfig(t, "")
	require.NoError(t, os.Remove(filepath.Join(filepath.Dir(path), "fed.crt")))
	err := run([]string{"--validate", "--config", path}, io.Discard, io.Discard)
	require.ErrorContains(t, err, "federation_cert_path")
}

func TestServeFailsFastOnBindError(t *testing.T) {
	// Occupy a port, then point the donor's health addr at it. Bare run() →
	// serve() must return the bind error (not block) because net.Listen fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	path := writeValidConfig(t, ln.Addr().String())
	err = run([]string{"--config", path}, io.Discard, io.Discard)
	require.ErrorContains(t, err, "health listen")
}
