package auth_test

import (
	"context"
	"testing"

	"github.com/nova-archive/nova/internal/auth"
	"github.com/stretchr/testify/require"
)

func TestIdentityContextRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, ok := auth.IdentityFromContext(ctx)
	require.False(t, ok, "empty ctx has no identity")

	want := auth.Identity{UserID: "u1", Role: "operator", Issuer: "https://nova.example/"}
	ctx = auth.ContextWithIdentity(ctx, want)
	got, ok := auth.IdentityFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, want, got)
}
