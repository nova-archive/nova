package storage

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSetWriteLimitsRaisesAssemblyCeilingLive(t *testing.T) {
	s := NewService(nil, nil, nil, WithWriteLimits(100, 1))
	// Occupy the only assembly slot, assert the next acquire is refused.
	rel, ok := s.tryAcquireAssembly()
	require.True(t, ok)
	_, ok2 := s.tryAcquireAssembly()
	require.False(t, ok2)
	// Raise the live ceiling; a new acquire now succeeds without reconstruction.
	s.SetWriteLimits(100, 2)
	rel3, ok3 := s.tryAcquireAssembly()
	require.True(t, ok3)
	rel()
	rel3()
}
