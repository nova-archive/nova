package config_test

import (
	"testing"

	"github.com/nova-archive/nova/internal/config"
	"github.com/stretchr/testify/require"
)

func TestLoadMinimalOperatorYAML(t *testing.T) {
	cfg, err := config.LoadFromFile("testdata/operator.minimal.yaml")
	require.NoError(t, err)

	require.Equal(t, "nova.example.test", cfg.Operator.Hostname)
	require.Equal(t, "admin@example.test", cfg.Operator.ContactEmail)
	require.Equal(t, "dev-self-signed", cfg.TLS.Mode)
	require.Equal(t, "", cfg.Auth.IssuerURL, "empty issuer = use local issuer")
	require.False(t, cfg.Auth.Paranoid)

	require.Equal(t, 60, cfg.Orchestrator.TickIntervalSeconds)
	require.Equal(t, 3, cfg.Orchestrator.Replication.Factor.Important)
	require.Equal(t, 2, cfg.Orchestrator.Replication.Factor.Cache)

	require.Equal(t, 7, cfg.Orchestrator.CapacityRunwayFloorDays)

	require.Equal(t, "quarantine", cfg.Moderation.TakedownDefaultAction)

	require.False(t, cfg.Coordinator.PublicIpfsDht)

	// Uploads defaults are applied when the section is absent.
	require.Equal(t, config.DefaultMaxUploadSizeBytes, cfg.Uploads.MaxUploadSizeBytes)
	require.Equal(t, 86400, cfg.Uploads.SessionTTLSeconds)
	require.Equal(t, 8, cfg.Uploads.MaxConcurrentAssembly)
}

func TestLoadOperatorYAMLRejectsMissingFile(t *testing.T) {
	_, err := config.LoadFromFile("testdata/does-not-exist.yaml")
	require.Error(t, err)
}

func TestLoadOperatorYAMLRejectsInvalidYAML(t *testing.T) {
	_, err := config.LoadFromBytes([]byte("operator: [this is not a map"))
	require.Error(t, err)
}
