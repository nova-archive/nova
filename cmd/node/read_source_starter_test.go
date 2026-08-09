package main

import (
	"io"
	"testing"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/federation/ca"
	nodeconfig "github.com/nova-archive/nova/internal/node/config"
	"github.com/nova-archive/nova/internal/node/source"
	"github.com/stretchr/testify/require"
)

// newTestStarter builds a readSourceStarter bound to an ephemeral port with
// real PEM material, since transport.ServerTLSConfig parses it for real.
func newTestStarter(t *testing.T) *readSourceStarter {
	t.Helper()
	caCert, caKey, err := ca.GenerateCA()
	require.NoError(t, err)
	cert, key, err := ca.IssueClientCert(caCert, caKey, uuid.New(), "test-donor")
	require.NoError(t, err)

	return &readSourceStarter{
		cfg: &nodeconfig.Config{
			SourceReadListenAddr:    "127.0.0.1:0",
			EgressBudgetBytesPerDay: 1 << 30,
			AuditBudgetFraction:     0.01,
		},
		caPEM: caCert, certPEM: cert, keyPEM: key,
		keyProvider: &source.KeyProvider{},
		stdout:      io.Discard,
		srvErr:      make(chan error, 1),
	}
}

// TestReadSourceStarterIsIdempotent pins the D-M7.2-7 contract: the boot path
// and the agent's onRegistered hook may both call Start, and must not
// double-bind.
func TestReadSourceStarterIsIdempotent(t *testing.T) {
	rs := newTestStarter(t)
	t.Cleanup(rs.Close)

	require.NoError(t, rs.Start("node-1"))
	require.NoError(t, rs.Start("node-1"), "second Start must be a no-op")
	require.NoError(t, rs.Start("node-1"))

	require.Equal(t, 1, rs.listens, "read-source must bind exactly once")
}

// TestReadSourceStarterSkipsWithoutNodeID proves a not-yet-registered donor
// does not start a server that would refuse every request.
func TestReadSourceStarterSkipsWithoutNodeID(t *testing.T) {
	rs := newTestStarter(t)
	t.Cleanup(rs.Close)

	require.NoError(t, rs.Start(""))
	require.Equal(t, 0, rs.listens, "no node_id means nothing to bind grants to")

	// ...and it can still start later, in-process, once registration lands.
	require.NoError(t, rs.Start("node-1"))
	require.Equal(t, 1, rs.listens, "registration must be able to start it without a restart")
}

// TestReadSourceStarterSkipsWhenUnconfigured covers the replication-only donor.
func TestReadSourceStarterSkipsWhenUnconfigured(t *testing.T) {
	rs := newTestStarter(t)
	rs.cfg.SourceReadListenAddr = ""
	t.Cleanup(rs.Close)

	require.NoError(t, rs.Start("node-1"))
	require.Equal(t, 0, rs.listens)
}
