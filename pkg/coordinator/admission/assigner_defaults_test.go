package admission

import (
	"testing"

	"github.com/nova-archive/nova/internal/config"
	"github.com/stretchr/testify/require"
)

func TestNewDefaultsZeroFactor(t *testing.T) {
	// All-zero factor: every field should be set to the package default.
	a := New(nil, config.ReplicationFactor{})
	require.Equal(t, config.DefaultReplicationImportant, a.factor.Important)
	require.Equal(t, config.DefaultReplicationNormal, a.factor.Normal)
	require.Equal(t, config.DefaultReplicationCache, a.factor.Cache)

	// Partial zero: explicit non-zero value is preserved; zero fields default.
	a2 := New(nil, config.ReplicationFactor{Important: 7, Normal: 0, Cache: 0})
	require.Equal(t, 7, a2.factor.Important, "explicit value must be preserved")
	require.Equal(t, config.DefaultReplicationNormal, a2.factor.Normal)
	require.Equal(t, config.DefaultReplicationCache, a2.factor.Cache)
}
