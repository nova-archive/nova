package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/federation/ca"
	"github.com/nova-archive/nova/internal/federation/transport"
	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/node/agent"
	nodeconfig "github.com/nova-archive/nova/internal/node/config"
	"github.com/nova-archive/nova/internal/node/state"
)

func TestEndToEndRegisterHeartbeatRevoke(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	q := gen.New(pool)
	caPEM, caKeyPEM, err := ca.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	srvPEM, srvKeyPEM, err := ca.IssueServerCert(caPEM, caKeyPEM, ca.ServerCertOptions{DNSNames: []string{"localhost"}, IPAddresses: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}

	s := New(q, Config{
		ListenAddr: "127.0.0.1:0",
		Timers:     wire.ConfigUpdates{HeartbeatIntervalSeconds: 300},
		TLS:        TLSMaterial{CAPEM: caPEM, CertPEM: srvPEM, KeyPEM: srvKeyPEM},
	})
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.Run(runCtx)

	// Donor client built from ONE issued cert (stable fingerprint for both
	// register and heartbeat).
	id := uuid.New()
	cliPEM, cliKeyPEM, err := ca.IssueClientCert(caPEM, caKeyPEM, id, "donor")
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := transport.ClientTLSConfig(caPEM, cliPEM, cliKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg.ServerName = "localhost"
	client := agent.NewHTTPClient("https://"+s.Addr(), tlsCfg)

	// Run the real agent briefly: it registers once then heartbeats.
	stateDir := t.TempDir()
	ag := agent.New(&nodeconfig.Config{BandwidthBudgetBytesPerDay: 1}, state.NewFileRegistrationStore(stateDir), state.NewFileStore(stateDir), state.NewFileAssignmentStore(stateDir), client, 20*time.Millisecond, time.Hour)
	agCtx, agCancel := context.WithTimeout(ctx, 120*time.Millisecond)
	defer agCancel()
	_ = ag.Run(agCtx)

	node, err := q.GetNodeByID(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		t.Fatalf("node not registered: %v", err)
	}
	if node.TrustState != "probationary" {
		t.Fatalf("trust_state = %q, want probationary", node.TrustState)
	}
	if node.Status != gen.NodeStatusActive {
		t.Fatalf("status = %v, want active", node.Status)
	}
	if !node.LastSeenAt.Valid {
		t.Fatal("last_seen_at not set (no heartbeat recorded)")
	}

	// Revoke → the next heartbeat (same client/cert) must fail.
	if _, err := q.RevokeNode(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Heartbeat(ctx, wire.HeartbeatRequest{}); err == nil {
		t.Fatal("expected heartbeat to fail after revoke")
	}
}
