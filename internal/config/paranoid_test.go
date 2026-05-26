package config_test

import (
	"testing"

	"github.com/nova-archive/nova/internal/config"
	"github.com/stretchr/testify/require"
)

func TestParanoidModeZerosWebhooks(t *testing.T) {
	cfg, err := config.LoadFromFile("testdata/operator.paranoid.yaml")
	require.NoError(t, err)
	require.True(t, cfg.Auth.Paranoid)

	require.Empty(t, cfg.Webhooks,
		"paranoid mode must drop all webhook destinations regardless of config")
}

func TestParanoidModeCapsSourceIPRetention(t *testing.T) {
	cfg, err := config.LoadFromFile("testdata/operator.paranoid.yaml")
	require.NoError(t, err)
	require.LessOrEqual(t, cfg.SourceIPRetentionDays, 1,
		"paranoid mode must cap source_ip_retention_days to <=1")
}

func TestNonParanoidPreservesWebhooks(t *testing.T) {
	cfg, err := config.LoadFromFile("testdata/operator.minimal.yaml")
	require.NoError(t, err)
	require.False(t, cfg.Auth.Paranoid)
	require.NotPanics(t, func() { _ = cfg.Webhooks })
}
