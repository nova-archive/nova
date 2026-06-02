package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadAuthzGrant(t *testing.T) {
	t.Parallel()
	require.False(t, ReadAuthorized(context.Background()), "no grant by default")

	granted := WithReadAuthz(context.Background())
	require.True(t, ReadAuthorized(granted))

	// The grant is scoped to the derived context; the parent is unaffected.
	require.False(t, ReadAuthorized(context.Background()))
}
