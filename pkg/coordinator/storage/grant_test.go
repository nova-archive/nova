package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadAuthzGrant(t *testing.T) {
	t.Parallel()
	require.False(t, readAuthorized(context.Background()), "no grant by default")

	granted := WithReadAuthz(context.Background())
	require.True(t, readAuthorized(granted))

	// The grant is scoped to the derived context; the parent is unaffected.
	require.False(t, readAuthorized(context.Background()))
}
