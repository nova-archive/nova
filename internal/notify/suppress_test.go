package notify

import (
	"context"
	"testing"

	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestScopedSuppressionNodeADoesNotSuppressNodeB(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	s := NewDBSuppressor(pool)
	const dest = "https://hook.example/1"

	fired, err := s.TryFire(ctx, "federation.concentrated", dest, "provider:aws", 3600)
	require.NoError(t, err)
	require.True(t, fired, "first scoped event fires")

	fired, err = s.TryFire(ctx, "federation.concentrated", dest, "provider:aws", 3600)
	require.NoError(t, err)
	require.False(t, fired, "same scope within window is suppressed")

	fired, err = s.TryFire(ctx, "federation.concentrated", dest, "provider:gcp", 3600)
	require.NoError(t, err)
	require.True(t, fired, "a distinct scope fires independently — A must not suppress B")
}

func TestSuppressionDurableAcrossRestart(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	const dest = "https://hook.example/2"

	fired, err := NewDBSuppressor(pool).TryFire(ctx, "federation.degraded", dest, "global", 3600)
	require.NoError(t, err)
	require.True(t, fired)

	// A brand-new store over the same DB (simulating a coordinator restart) stays
	// suppressed within the window.
	fired, err = NewDBSuppressor(pool).TryFire(ctx, "federation.degraded", dest, "global", 3600)
	require.NoError(t, err)
	require.False(t, fired, "durable once-per-window survives restart")
}

func TestZeroWindowAlwaysFires(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	s := NewDBSuppressor(pool)
	const dest = "https://hook.example/3"
	for i := 0; i < 3; i++ {
		fired, err := s.TryFire(ctx, "federation.node_revoked", dest, "node-x", 0)
		require.NoError(t, err)
		require.True(t, fired, "window 0 always fires (node_revoked is deduped upstream)")
	}
}
