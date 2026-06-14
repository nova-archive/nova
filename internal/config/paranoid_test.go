package config_test

import (
	"strings"
	"testing"

	"github.com/nova-archive/nova/internal/config"
	"github.com/stretchr/testify/require"
)

func boolPtr(b bool) *bool { return &b }

// containsSubstr reports whether any warning contains sub.
func containsSubstr(warnings []string, sub string) bool {
	for _, w := range warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// P2-M0.2: paranoid is a PRESET, not a hard override. Explicit operator values
// win; relaxing a protective default warns rather than being silently nuked.

func TestParanoidPresetKeepsWebhooksWithWarning(t *testing.T) {
	cfg, err := config.LoadFromFile("testdata/operator.paranoid.yaml")
	require.NoError(t, err)
	require.True(t, cfg.Auth.Paranoid)

	// The fixture configures a webhook destination. Warn-not-force: it is kept,
	// not dropped, and the operator is warned that egress remains active.
	require.NotEmpty(t, cfg.Webhooks,
		"paranoid must NOT silently drop explicitly-configured webhooks (warn-not-force)")
	require.True(t, containsSubstr(cfg.PrivacyWarnings(), "webhook"),
		"expected a consequence warning about active webhook egress")
}

func TestParanoidPresetKeepsHigherRetentionWithWarning(t *testing.T) {
	cfg, err := config.LoadFromFile("testdata/operator.paranoid.yaml")
	require.NoError(t, err)

	// Fixture sets source_ip_retention_days: 30. Warn-not-force keeps it.
	require.Equal(t, 30, cfg.SourceIPRetentionDays,
		"paranoid must keep an explicit retention window, not cap it")
	require.True(t, containsSubstr(cfg.PrivacyWarnings(), "source_ip_retention_days"),
		"expected a consequence warning that retention exceeds the hardened default")
}

func TestParanoidPresetDefaultsRecordSourceIPOff(t *testing.T) {
	cfg, err := config.LoadFromFile("testdata/operator.paranoid.yaml")
	require.NoError(t, err)

	// The fixture leaves record_source_ip unset → the preset fills it false.
	require.NotNil(t, cfg.Coordinator.RecordSourceIP)
	require.False(t, *cfg.Coordinator.RecordSourceIP,
		"paranoid preset should default source-IP recording off when unset")
}

func TestNonParanoidNoPrivacyWarnings(t *testing.T) {
	cfg, err := config.LoadFromFile("testdata/operator.minimal.yaml")
	require.NoError(t, err)
	require.False(t, cfg.Auth.Paranoid)
	require.Empty(t, cfg.PrivacyWarnings(),
		"default posture must produce no privacy warnings")
	require.Nil(t, cfg.Coordinator.RecordSourceIP,
		"non-paranoid load must not synthesize record_source_ip (effective default: record)")
}

func TestNonParanoidPreservesWebhooks(t *testing.T) {
	cfg, err := config.LoadFromFile("testdata/operator.minimal.yaml")
	require.NoError(t, err)
	require.False(t, cfg.Auth.Paranoid)
	require.NotPanics(t, func() { _ = cfg.Webhooks })
}

// Direct ApplyPrivacyPreset unit coverage for the edge cases the fixtures don't
// exercise.

func TestParanoidWarnsWhenRecordSourceIPExplicitlyTrue(t *testing.T) {
	cfg := &config.Config{}
	cfg.Auth.Paranoid = true
	cfg.Coordinator.RecordSourceIP = boolPtr(true)

	warnings := config.ApplyPrivacyPreset(cfg)

	require.NotNil(t, cfg.Coordinator.RecordSourceIP)
	require.True(t, *cfg.Coordinator.RecordSourceIP, "explicit value must win over the preset")
	require.True(t, containsSubstr(warnings, "record_source_ip"),
		"expected a warning that source IPs will be recorded despite paranoid")
}

func TestRecordSourceIPDecoupledFromParanoid(t *testing.T) {
	// An operator can stop source-IP recording WITHOUT enabling the full preset.
	cfg := &config.Config{}
	cfg.Auth.Paranoid = false
	cfg.Coordinator.RecordSourceIP = boolPtr(false)

	warnings := config.ApplyPrivacyPreset(cfg)

	require.Empty(t, warnings, "paranoid off must produce no warnings")
	require.NotNil(t, cfg.Coordinator.RecordSourceIP)
	require.False(t, *cfg.Coordinator.RecordSourceIP,
		"explicit record_source_ip:false must be preserved independent of paranoid")
}

func TestParanoidWarnsOnPublicIpfsDht(t *testing.T) {
	cfg := &config.Config{}
	cfg.Auth.Paranoid = true
	cfg.Coordinator.PublicIpfsDht = true

	warnings := config.ApplyPrivacyPreset(cfg)
	require.True(t, containsSubstr(warnings, "public_ipfs_dht"),
		"expected a warning that pinned CIDs are advertised to the public DHT")
}
