package integrity_test

import (
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/audit/integrity"
	"github.com/stretchr/testify/require"
)

func TestDefaultCadencesCoverAllKinds(t *testing.T) {
	d := integrity.DefaultCadences()
	require.Len(t, d, len(integrity.AllKinds))
	for _, k := range integrity.AllKinds {
		c, ok := d[k]
		require.True(t, ok, "missing cadence for %s", k)
		require.Positive(t, c.Interval, "interval for %s", k)
		require.Positive(t, c.SampleSize, "sample size for %s", k)
	}
	// Spot-check the spec's tightest cadence (INTEGRITY_AUDIT.md § Schedule).
	require.Equal(t, 15*time.Minute, d[integrity.KindKuboPinPresent].Interval)
	require.Equal(t, 200, d[integrity.KindKuboPinPresent].SampleSize)
	require.Equal(t, time.Hour, d[integrity.KindSampleDecrypt].Interval)
	require.Equal(t, 50, d[integrity.KindSampleDecrypt].SampleSize)
	require.Equal(t, 24*time.Hour, d[integrity.KindBlockHashValid].Interval)
}

func TestEnforceAuditPolicyAcceptsDefaults(t *testing.T) {
	require.NoError(t, integrity.EnforceAuditPolicy(integrity.DefaultCadences()))
}

func TestEnforceAuditPolicyRejectsOutOfBoundsSample(t *testing.T) {
	c := integrity.DefaultCadences()
	c[integrity.KindEnvelopeDecode] = integrity.Cadence{Interval: time.Hour, SampleSize: 0}
	require.Error(t, integrity.EnforceAuditPolicy(c))
	c[integrity.KindEnvelopeDecode] = integrity.Cadence{Interval: time.Hour, SampleSize: 10001}
	require.Error(t, integrity.EnforceAuditPolicy(c))
}
