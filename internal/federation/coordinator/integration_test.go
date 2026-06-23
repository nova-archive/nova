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

// waitFor polls cond until it returns true or the deadline elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func TestEndToEndAssignmentSync(t *testing.T) {
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
	s := New(q, Config{ListenAddr: "127.0.0.1:0", Timers: wireTimers(), TLS: TLSMaterial{CAPEM: caPEM, CertPEM: srvPEM, KeyPEM: srvKeyPEM}})
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.Run(runCtx)

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

	dir := t.TempDir()
	asg := state.NewFileAssignmentStore(dir)
	ag := agent.New(&nodeconfig.Config{BandwidthBudgetBytesPerDay: 1},
		state.NewFileRegistrationStore(dir), state.NewFileStore(dir), asg, client,
		time.Hour, 15*time.Millisecond)
	agCtx, agCancel := context.WithCancel(ctx)
	defer agCancel()
	go ag.Run(agCtx)

	// donor registers
	waitFor(t, 3*time.Second, func() bool {
		_, err := q.GetNodeByID(ctx, pgtype.UUID{Bytes: id, Valid: true})
		return err == nil
	})

	// coordinator assigns two CIDs; donor converges via the poll loop
	for _, c := range []string{"bafa", "bafb"} {
		seedBlob(t, ctx, pool, c, 7)
		assignViaSeam(t, ctx, pool, c, id)
	}
	waitFor(t, 3*time.Second, func() bool {
		l, _ := asg.List()
		return len(l) == 2
	})

	// no acked rows — the donor never acks in M3
	var acked int
	pool.QueryRow(ctx, `SELECT count(*) FROM pin_assignments WHERE state='acked'`).Scan(&acked)
	if acked != 0 {
		t.Fatalf("acked rows = %d, want 0 (no auto-ack in M3)", acked)
	}

	// unpin one ⇒ donor removes it
	txu, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnpinPin(ctx, txu, "bafa", id); err != nil {
		t.Fatal(err)
	}
	if err := txu.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		l, _ := asg.List()
		return len(l) == 1 && l[0].CID == "bafb"
	})
}

func TestEndToEndSnapshotRecovery(t *testing.T) {
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
	s := New(q, Config{ListenAddr: "127.0.0.1:0", Timers: wireTimers(), TLS: TLSMaterial{CAPEM: caPEM, CertPEM: srvPEM, KeyPEM: srvKeyPEM}})
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.Run(runCtx)

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

	// Register the node directly (no donor cursor involved yet).
	if _, err := client.Register(ctx, wire.RegisterRequest{
		SupportedProtocols: []string{wire.ProtocolV1},
		Capabilities:       []string{wire.CapPinChangeLog, wire.CapSnapshot},
	}); err != nil {
		t.Fatal(err)
	}

	// Assign two CIDs, then prune the whole change log so the watermark passes head.
	for _, c := range []string{"bafa", "bafb"} {
		seedBlob(t, ctx, pool, c, 7)
		assignViaSeam(t, ctx, pool, c, id)
	}
	if _, err := pool.Exec(ctx, `UPDATE pin_changes SET created_at = now() - interval '30 days'`); err != nil {
		t.Fatal(err)
	}
	if err := s.pruneOnce(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}

	// A fresh-cursor donor (cursor 0 < watermark) must recover via snapshot.
	// Pre-seed registration so the agent skips re-register and goes straight to sync.
	dir := t.TempDir()
	reg := state.NewFileRegistrationStore(dir)
	if err := reg.SaveRegistration(ctx, state.Registration{NodeID: id.String(), SelectedProtocol: wire.ProtocolV1}); err != nil {
		t.Fatal(err)
	}
	asg := state.NewFileAssignmentStore(dir)
	ag := agent.New(&nodeconfig.Config{BandwidthBudgetBytesPerDay: 1}, reg, state.NewFileStore(dir), asg, client, time.Hour, 15*time.Millisecond)
	agCtx, agCancel := context.WithCancel(ctx)
	defer agCancel()
	go ag.Run(agCtx)

	// snapshot recovery rebuilds the full desired set
	waitFor(t, 3*time.Second, func() bool {
		l, _ := asg.List()
		return len(l) == 2
	})
	var acked int
	pool.QueryRow(ctx, `SELECT count(*) FROM pin_assignments WHERE state='acked'`).Scan(&acked)
	if acked != 0 {
		t.Fatalf("acked rows = %d, want 0", acked)
	}
}
