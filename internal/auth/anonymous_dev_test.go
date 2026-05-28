//go:build nova_dev

package auth_test

import (
	"testing"

	"github.com/nova-archive/nova/internal/auth"
	"github.com/stretchr/testify/require"
)

func TestAnonymousPermittedInDevBuild(t *testing.T) {
	t.Parallel()
	require.NoError(t, auth.EnforceAnonymousPolicy(true), "nova_dev build must permit auth.anonymous=true")
	require.NoError(t, auth.EnforceAnonymousPolicy(false))
}
