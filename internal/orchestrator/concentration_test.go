package orchestrator

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/notify"
	"github.com/stretchr/testify/require"
)

func TestGiniEdgeCases(t *testing.T) {
	require.Equal(t, 0.0, gini(nil), "zero pins ⇒ 0")
	require.Equal(t, 0.0, gini([]int64{5}), "one node ⇒ 0")
	require.Equal(t, 0.0, gini([]int64{3, 3, 3}), "uniform ⇒ 0")
	require.Equal(t, 0.0, gini([]int64{0, 0, 0}), "all-zero ⇒ 0")
	require.InDelta(t, 0.667, gini([]int64{10, 0, 0}), 0.01, "fully concentrated on 1 of 3")
	require.Greater(t, gini([]int64{100, 10, 1}), 0.3, "skewed ⇒ high")
}

func TestEntropyNormalizationSingleGroupZero(t *testing.T) {
	m := dimensionMetrics(map[string]int64{"only": 100}, 3)
	require.Equal(t, 1, m.Groups)
	require.Equal(t, 1.0, m.LargestShare)
	require.Equal(t, 0.0, m.NormalizedEntropy, "single group ⇒ 0 (no ln(1) divide)")
}

func TestTopKClampedToGroupCount(t *testing.T) {
	m := dimensionMetrics(map[string]int64{"a": 5, "b": 3}, 5) // k=5 but only 2 groups
	require.Equal(t, 2, m.Groups)
	require.InDelta(t, 1.0, m.TopKShare, 1e-9, "top-k clamps to the group count")
	require.InDelta(t, 5.0/8.0, m.LargestShare, 1e-9)
}

func TestUniformTwoGroupsMaxEntropy(t *testing.T) {
	m := dimensionMetrics(map[string]int64{"a": 50, "b": 50}, 2)
	require.InDelta(t, 1.0, m.NormalizedEntropy, 1e-9, "perfectly even ⇒ normalized entropy 1")
	require.InDelta(t, 0.5, m.LargestShare, 1e-9)
}

// setNodeDim makes node id an operator-verified holder with `pins` acked pins in a
// given provider (verified=false leaves it unverified ⇒ collapses to "unknown").
func setNodeDim(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, provider string, verified bool, pins int) {
	t.Helper()
	ver := "NULL"
	if verified {
		ver = "now()"
	}
	_, err := pool.Exec(ctx,
		`UPDATE nodes SET provider=$2, operator_verified_at=`+ver+` WHERE id=$1::uuid`, id, provider)
	require.NoError(t, err)
	for i := 0; i < pins; i++ {
		cid := id + "-c" + string(rune('a'+i))
		seedBlob(t, ctx, pool, cid, "normal", false)
		assignPinState(t, ctx, pool, cid, id, "acked")
	}
}

func TestUnknownCollapsedBeforeMetrics(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	// Two UNVERIFIED nodes declaring DIFFERENT providers must collapse to one
	// "unknown" group — a node can't manufacture diversity by self-declaring.
	a := "a6000000-0000-0000-0000-000000000001"
	b := "a6000000-0000-0000-0000-000000000002"
	seedNode(t, ctx, pool, a, "active", "current", false)
	seedNode(t, ctx, pool, b, "active", "current", false)
	setNodeDim(t, ctx, pool, a, "self-declared-x", false, 2)
	setNodeDim(t, ctx, pool, b, "self-declared-y", false, 2)

	c, err := ComputeConcentration(ctx, pool, 3)
	require.NoError(t, err)
	prov := c.Dims["provider"]
	require.Equal(t, 1, prov.Groups, "unverified providers collapse to a single unknown group")
	require.Equal(t, "unknown", prov.LargestValue)
}

func TestConcentratedAndHomogeneousFire(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	// Heavily skewed verified providers: aws dominant, gcp tiny ⇒ both concentrated
	// (largest share high) and homogeneous (entropy low).
	aws := "a7000000-0000-0000-0000-000000000001"
	gcp := "a7000000-0000-0000-0000-000000000002"
	seedNode(t, ctx, pool, aws, "active", "current", false)
	seedNode(t, ctx, pool, gcp, "active", "current", false)
	setNodeDim(t, ctx, pool, aws, "aws", true, 8)
	setNodeDim(t, ctx, pool, gcp, "gcp", true, 1)

	rec := &recordingNotifier{}
	err := EmitConcentration(ctx, pool, rec, 3, ConcentrationThresholds{LargestShareMax: 0.5, NormalizedEntropyMin: 0.6})
	require.NoError(t, err)
	require.True(t, hasEventScope(rec, "federation.concentrated", "provider:aws"), "skewed provider ⇒ concentrated")
	require.True(t, hasEventScope(rec, "federation.homogeneous", "provider"), "low-entropy provider ⇒ homogeneous")
}

func hasEventScope(r *recordingNotifier, typ, scope string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Type == typ && e.ScopeKey == scope {
			return true
		}
	}
	return false
}

func findEvent(r *recordingNotifier, typ string) (notify.Event, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Type == typ {
			return e, true
		}
	}
	return notify.Event{}, false
}
