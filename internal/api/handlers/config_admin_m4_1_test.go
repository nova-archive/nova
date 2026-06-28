package handlers_test

// config_admin_m4_1_test.go — /settings field-effect classification tests for
// the four first-class M4.1 storage/read redirect knobs (Task 14A).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/config"
	"github.com/nova-archive/nova/internal/config/reload"
)

// m41MinimalYAML extends the minimalYAML baseline with an explicit bounded_cache
// coordinator block so all four first-class M4.1 knobs appear in the serialised
// config map returned by GET /settings.
const m41MinimalYAML = `operator:
  hostname: h.test
  contact_email: a@b.test
tls:
  mode: dev-self-signed
orchestrator:
  replication:
    factor:
      important: 3
      normal: 3
      cache: 2
coordinator:
  public_ipfs_dht: false
  coordinator_storage_mode: bounded_cache
  bounded_cache_max_bytes: 536870912
  require_replication_quorum_before_commit: true
  prune_safety_floor: 2
`

// TestM41FirstClassKnobsAreRestart asserts that the four first-class M4.1
// operator.yaml knobs are classified as restart-effect in the /settings
// fieldEffect map. This verifies the explicit entries added for these knobs —
// all are read once at coordinator construction and require a restart.
func TestM41FirstClassKnobsAreRestart(t *testing.T) {
	t.Parallel()
	cfg, err := config.LoadFromBytes([]byte(m41MinimalYAML))
	require.NoError(t, err)
	store := reload.New(cfg, nil, nil)
	h := handlers.NewConfigAdminHandler(store, "/tmp/operator.yaml")

	rec := httptest.NewRecorder()
	h.Get(rec, operatorReq(http.MethodGet, "/api/v1/admin/config", "", uuid.New()))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		Fields map[string]map[string]any `json:"fields"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	firstClass := []string{
		"coordinator.coordinator_storage_mode",
		"coordinator.bounded_cache_max_bytes",
		"coordinator.require_replication_quorum_before_commit",
		"coordinator.prune_safety_floor",
	}
	for _, knob := range firstClass {
		meta, ok := body.Fields[knob]
		require.True(t, ok, "knob %q missing from /settings field metadata", knob)
		require.Equal(t, "restart", meta["effect"],
			"knob %q should be restart-effect (read at coordinator construction)", knob)
	}
}

// TestM41AdvancedKnobsInheritRestartViaPrefix asserts that advanced M4.1
// coordinator tuning knobs (those without an explicit fieldEffect entry) inherit
// the "coordinator" prefix match and are also classified restart-effect.
func TestM41AdvancedKnobsInheritRestartViaPrefix(t *testing.T) {
	t.Parallel()
	// Use a config that includes advanced knobs so they appear in the map.
	advancedYAML := m41MinimalYAML + `  bounded_cache_protected_ratio: 0.75
  bounded_cache_max_object_bytes: 10485760
  lru_touch_interval_seconds: 120
  commit_reconciler_interval_seconds: 60
  commit_fail_after_seconds: 7200
  pruner_interval_seconds: 120
  prune_stale_seconds: 3600
`
	cfg, err := config.LoadFromBytes([]byte(advancedYAML))
	require.NoError(t, err)
	store := reload.New(cfg, nil, nil)
	h := handlers.NewConfigAdminHandler(store, "/tmp/operator.yaml")

	rec := httptest.NewRecorder()
	h.Get(rec, operatorReq(http.MethodGet, "/api/v1/admin/config", "", uuid.New()))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		Fields map[string]map[string]any `json:"fields"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	advanced := []string{
		"coordinator.bounded_cache_protected_ratio",
		"coordinator.lru_touch_interval_seconds",
		"coordinator.commit_reconciler_interval_seconds",
		"coordinator.commit_fail_after_seconds",
		"coordinator.pruner_interval_seconds",
		"coordinator.prune_stale_seconds",
	}
	for _, knob := range advanced {
		meta, ok := body.Fields[knob]
		require.True(t, ok, "advanced knob %q missing from /settings field metadata", knob)
		require.Equal(t, "restart", meta["effect"],
			"advanced knob %q should inherit restart-effect from coordinator prefix", knob)
	}
}
