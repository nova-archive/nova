package wire_test

import (
	"testing"

	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/stretchr/testify/require"
)

func TestNegotiateCapabilitiesAllPresent(t *testing.T) {
	offered := []string{wire.CapPinChangeLog, wire.CapSnapshot, wire.CapRepairStream}
	required := []string{wire.CapPinChangeLog, wire.CapSnapshot}
	missing, ok := wire.NegotiateCapabilities(offered, required)
	require.True(t, ok)
	require.Empty(t, missing)
}

func TestNegotiateCapabilitiesFailsClosedOnMissing(t *testing.T) {
	offered := []string{wire.CapPinChangeLog}
	required := []string{wire.CapPinChangeLog, wire.CapSnapshot}
	missing, ok := wire.NegotiateCapabilities(offered, required)
	require.False(t, ok)
	require.Equal(t, []string{wire.CapSnapshot}, missing)
}
