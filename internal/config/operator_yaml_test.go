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

// minimalYAML is a valid operator.yaml that satisfies all required fields.
// Tests that need to mutate a single field can append overrides after this block
// or construct a fresh call with modified YAML.
const minimalYAML = `
operator:
  hostname: nova.example.test
  contact_email: admin@example.test
tls:
  mode: dev-self-signed
auth:
  issuer_url: ""
  paranoid: false
orchestrator:
  tick_interval_seconds: 60
  step_seconds: 60
  replication:
    factor:
      important: 3
      normal: 3
      cache: 2
  mass_casualty_threshold_ratio: 0.20
  mass_casualty_window_seconds: 3600
  capacity_runway_floor_days: 7
federation:
  heartbeat_interval_seconds: 300
  pins_poll_interval_seconds: 600
  max_pin_concurrency: 16
  suspect_after_missed_heartbeats: 3
  unreachable_after_seconds: 3600
  evicted_after_seconds: 2592000
integrity_audit:
  envelope_decode: { interval_seconds: 3600, sample_size: 100 }
  key_unwrap: { interval_seconds: 3600, sample_size: 100 }
  sample_decrypt: { interval_seconds: 3600, sample_size: 50 }
  kubo_pin_present: { interval_seconds: 900, sample_size: 200 }
  derivative_state_consistent: { interval_seconds: 3600, sample_size: 100 }
  block_hash_valid: { interval_seconds: 86400, sample_size: 100 }
  manifest_consistent: { interval_seconds: 86400, sample_size: 100 }
moderation:
  takedown_default_action: quarantine
  dmca_counter_notification_days: 14
coordinator:
  public_ipfs_dht: false
`

func TestPublicUploadsRequiresTosURL(t *testing.T) {
	t.Parallel()

	// public_uploads=true without tos_url must be rejected (T1.20).
	withoutTos := minimalYAML + `
uploads:
  public_uploads: true
`
	_, err := config.LoadFromBytes([]byte(withoutTos))
	require.Error(t, err, "T1.20: public uploads require tos_url")

	// public_uploads=true with tos_url must be accepted.
	withTos := minimalYAML + `
uploads:
  public_uploads: true
tos_url: "https://nova.example/tos"
`
	_, err = config.LoadFromBytes([]byte(withTos))
	require.NoError(t, err)
}
