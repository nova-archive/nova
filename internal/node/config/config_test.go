package config_test

import (
	"os"
	"path/filepath"
	"testing"

	nodeconfig "github.com/nova-archive/nova/internal/node/config"
	"github.com/stretchr/testify/require"
)

// writeValid creates a temp dir with readable fixture files for every *_path
// field and returns rendered YAML plus the temp dir.
func writeValid(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"fed.ca", "fed.crt", "fed.key", "neb.crt", "neb.key", "swarm.key"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600))
	}
	storage := filepath.Join(dir, "data")
	require.NoError(t, os.MkdirAll(storage, 0o700))
	yaml := "coordinator_url: https://coord.example\n" +
		"federation_ca_path: " + filepath.Join(dir, "fed.ca") + "\n" +
		"federation_cert_path: " + filepath.Join(dir, "fed.crt") + "\n" +
		"federation_key_path: " + filepath.Join(dir, "fed.key") + "\n" +
		"nebula_cert_path: " + filepath.Join(dir, "neb.crt") + "\n" +
		"nebula_key_path: " + filepath.Join(dir, "neb.key") + "\n" +
		"swarm_key_path: " + filepath.Join(dir, "swarm.key") + "\n" +
		"storage_dir: " + storage + "\n" +
		"bandwidth_budget_bytes_per_day: 53687091200\n"
	return yaml, dir
}

func TestLoadMinimalValid(t *testing.T) {
	yaml, _ := writeValid(t)
	cfg, err := nodeconfig.LoadFromBytes([]byte(yaml))
	require.NoError(t, err)
	require.Equal(t, "https://coord.example", cfg.CoordinatorURL)
	require.Equal(t, int64(53687091200), cfg.BandwidthBudgetBytesPerDay)
	require.Equal(t, nodeconfig.DefaultHealthListenAddr, cfg.HealthListenAddr, "default applied")
}

func TestLoadRejectsMalformedYAML(t *testing.T) {
	_, err := nodeconfig.LoadFromBytes([]byte("coordinator_url: [unterminated"))
	require.Error(t, err)
}

func TestValidateRejectsMissingCoordinatorURL(t *testing.T) {
	yaml, _ := writeValid(t)
	yaml = "coordinator_url: \"\"\n" + yaml[len("coordinator_url: https://coord.example\n"):]
	_, err := nodeconfig.LoadFromBytes([]byte(yaml))
	require.ErrorContains(t, err, "coordinator_url")
}

func TestValidateRejectsMissingCertFile(t *testing.T) {
	yaml, dir := writeValid(t)
	require.NoError(t, os.Remove(filepath.Join(dir, "fed.crt")))
	_, err := nodeconfig.LoadFromBytes([]byte(yaml))
	require.ErrorContains(t, err, "federation_cert_path")
}

func TestValidateRejectsCertPathThatIsDirectory(t *testing.T) {
	yaml, dir := writeValid(t)
	require.NoError(t, os.Remove(filepath.Join(dir, "neb.key")))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "neb.key"), 0o700))
	_, err := nodeconfig.LoadFromBytes([]byte(yaml))
	require.ErrorContains(t, err, "nebula_key_path")
}

func TestValidateRejectsNonPositiveBudget(t *testing.T) {
	yaml, _ := writeValid(t)
	yaml = yaml[:len(yaml)-len("bandwidth_budget_bytes_per_day: 53687091200\n")] +
		"bandwidth_budget_bytes_per_day: 0\n"
	_, err := nodeconfig.LoadFromBytes([]byte(yaml))
	require.ErrorContains(t, err, "bandwidth_budget_bytes_per_day")
}

func TestValidateCreatesMissingStorageDir(t *testing.T) {
	yaml, dir := writeValid(t)
	storage := filepath.Join(dir, "data")
	require.NoError(t, os.RemoveAll(storage)) // absent but creatable under dir
	_, err := nodeconfig.LoadFromBytes([]byte(yaml))
	require.NoError(t, err)
	info, statErr := os.Stat(storage)
	require.NoError(t, statErr)
	require.True(t, info.IsDir())
}

func TestValidateRejectsBadCoordinatorURL(t *testing.T) {
	yaml, _ := writeValid(t)
	yaml = "coordinator_url: \"not a url\"\n" + yaml[len("coordinator_url: https://coord.example\n"):]
	_, err := nodeconfig.LoadFromBytes([]byte(yaml))
	require.ErrorContains(t, err, "coordinator_url")
}

func TestValidateRejectsBadHealthAddr(t *testing.T) {
	yaml, _ := writeValid(t)
	yaml += "health_listen_addr: not-a-host-port\n"
	_, err := nodeconfig.LoadFromBytes([]byte(yaml))
	require.ErrorContains(t, err, "health_listen_addr")
}

func TestStorageMaxBytesOptional(t *testing.T) {
	// Omitted storage_max_bytes defaults to 0 (unlimited).
	yaml, _ := writeValid(t)
	cfg, err := nodeconfig.LoadFromBytes([]byte(yaml))
	require.NoError(t, err)
	require.Equal(t, int64(0), cfg.StorageMaxBytes, "omitted storage_max_bytes should be 0 (unlimited)")
}

func TestStorageMaxBytesNegativeRejected(t *testing.T) {
	yaml, _ := writeValid(t)
	yaml += "storage_max_bytes: -1\n"
	_, err := nodeconfig.LoadFromBytes([]byte(yaml))
	require.ErrorContains(t, err, "storage_max_bytes")
}

func TestKuboAPIAddrDefault(t *testing.T) {
	yaml, _ := writeValid(t)
	cfg, err := nodeconfig.LoadFromBytes([]byte(yaml))
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:5001", cfg.KuboAPIAddr, "default kubo_api_addr should be applied when omitted")
}

func TestKuboAPIAddrExplicit(t *testing.T) {
	yaml, _ := writeValid(t)
	yaml += "kubo_api_addr: http://127.0.0.1:5099\n"
	cfg, err := nodeconfig.LoadFromBytes([]byte(yaml))
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:5099", cfg.KuboAPIAddr)
}
