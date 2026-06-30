package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestMassCasualtyDegradedNoOverride(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	// 5 surviving active, 5 just gone unreachable within the window ⇒ recent(5) >
	// 0.2 × baseline(10).
	for i := 0; i < 5; i++ {
		id := "c0000000-0000-0000-0000-00000000000" + string(rune('1'+i))
		seedNode(t, ctx, pool, id, "active", "current", false)
	}
	for i := 0; i < 5; i++ {
		id := "c1000000-0000-0000-0000-00000000000" + string(rune('1'+i))
		seedNode(t, ctx, pool, id, "unreachable", "current", false)
		_, err := pool.Exec(ctx, `UPDATE nodes SET last_status_change_at = now() WHERE id=$1::uuid`, id)
		require.NoError(t, err)
	}

	rec := &recordingNotifier{}
	cfg := AttritionConfig{
		Targets:                 healTargets,
		MassCasualtyWindow:      time.Hour,
		MassCasualtyRatio:       0.2,
		CapacityRunwayFloorDays: 7,
	}
	require.NoError(t, EvaluateAttrition(ctx, pool, rec, cfg))

	ev, ok := findEvent(rec, "federation.degraded")
	require.True(t, ok, "active→unreachable burst ⇒ degraded")
	require.Equal(t, "global", ev.ScopeKey)
}

func TestShrinkingUsesRepairTimeAndStorageHeadroom(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	// One repair-eligible normal blob (envelope 100, R=3 ⇒ desired 300) and a single
	// surviving donor with a tiny daily egress + free space.
	seedHealBlob(t, ctx, pool, "sk1", "normal")
	node := "c2000000-0000-0000-0000-000000000001"
	seedNode(t, ctx, pool, node, "active", "current", false)
	_, err := pool.Exec(ctx, `
		UPDATE nodes SET last_egress_capacity_bytes=10, last_free_bytes=10,
			bandwidth_budget_bytes_per_day=10 WHERE id=$1::uuid`, node)
	require.NoError(t, err)

	rec := &recordingNotifier{}
	cfg := AttritionConfig{
		Targets:                 healTargets,
		MassCasualtyWindow:      time.Hour,
		MassCasualtyRatio:       0.2,
		CapacityRunwayFloorDays: 7,
	}
	require.NoError(t, EvaluateAttrition(ctx, pool, rec, cfg))

	ev, ok := findEvent(rec, "federation.shrinking")
	require.True(t, ok, "long repair time + low headroom ⇒ shrinking")
	require.Equal(t, "normal", ev.ScopeKey, "scoped to the limiting class")
	// The corrected, dimensionally-honest metric names — NEVER runway_days.
	require.Contains(t, ev.Payload, "repair_time_days")
	require.Contains(t, ev.Payload, "storage_headroom")
	require.NotContains(t, ev.Payload, "runway_days")
	require.InDelta(t, 30.0, ev.Payload["repair_time_days"].(float64), 0.001, "desired 300 / egress 10 = 30 days")
	require.Less(t, ev.Payload["storage_headroom"].(float64), 1.0)
}
