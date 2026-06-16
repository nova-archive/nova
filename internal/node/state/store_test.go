package state_test

import (
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/node/state"
	"github.com/stretchr/testify/require"
)

func TestMemStoreCursor(t *testing.T) {
	s := state.NewMemStore()
	require.NoError(t, s.SetCursor(42))
	c, err := s.Cursor()
	require.NoError(t, err)
	require.Equal(t, int64(42), c)
}

func TestMemStoreJTI(t *testing.T) {
	s := state.NewMemStore()
	seen, err := s.SeenJTI("x")
	require.NoError(t, err)
	require.False(t, seen)
	require.NoError(t, s.RecordJTI("x", time.Now().Add(time.Hour)))
	seen, _ = s.SeenJTI("x")
	require.True(t, seen)
}

func TestMemStoreJTIExpiry(t *testing.T) {
	s := state.NewMemStore()
	require.NoError(t, s.RecordJTI("old", time.Now().Add(-time.Second)))
	seen, err := s.SeenJTI("old")
	require.NoError(t, err)
	require.False(t, seen, "expired jti must not count as seen")
}
