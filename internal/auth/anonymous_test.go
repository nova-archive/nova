//go:build !nova_dev

package auth_test

import (
	"testing"

	"github.com/nova-archive/nova/internal/auth"
	"github.com/stretchr/testify/require"
)

func TestAnonymousRefusedInProd(t *testing.T) {
	t.Parallel()
	require.Error(t, auth.EnforceAnonymousPolicy(true), "production build must refuse auth.anonymous=true")
	require.NoError(t, auth.EnforceAnonymousPolicy(false))
}
