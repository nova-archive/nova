//go:build !nova_dev

package integrity_test

import (
	"testing"

	"github.com/nova-archive/nova/internal/audit/integrity"
	"github.com/stretchr/testify/require"
)

// In production builds a zero interval (a disabled kind) is refused;
// INTEGRITY_AUDIT.md permits disabling only under the nova_dev build tag.
func TestEnforceAuditPolicyRejectsZeroIntervalInProd(t *testing.T) {
	c := map[integrity.Kind]integrity.Cadence{
		integrity.KindEnvelopeDecode: {Interval: 0, SampleSize: 100},
	}
	require.Error(t, integrity.EnforceAuditPolicy(c))
}
