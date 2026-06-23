package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/federation/ca"
	"github.com/nova-archive/nova/internal/federation/tokens"
	"github.com/nova-archive/nova/internal/federation/transport"
	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/node/agent"
	nodeconfig "github.com/nova-archive/nova/internal/node/config"
	"github.com/nova-archive/nova/internal/node/state"
)

// echoPinner is a blockstore test double whose AddDeterministic returns the CID
// it was configured with (happy path) or a mismatch CID (fail path).
// Has always returns the configured value; Unpin and RepoStoredBytes are no-ops.
type echoPinner struct {
	cid string // returned by AddDeterministic
	has bool   // returned by Has
}

func (e *echoPinner) AddDeterministic(_ context.Context, _ []byte) (string, error) {
	return e.cid, nil
}

func (e *echoPinner) Has(_ context.Context, _ string) (bool, error) {
	return e.has, nil
}

func (e *echoPinner) Unpin(_ context.Context, _ string) error { return nil }

func (e *echoPinner) RepoStoredBytes(_ context.Context) (int64, error) { return 0, nil }

// buildM4Server constructs a live mTLS coordinator server with M4 source deps
// wired (signer, fakeBackend, RepairTokenTTL:time.Hour). Returns the server,
// pool, caPEM/caKeyPEM, and a bound-address (after Listen+Run).
func buildM4Server(t *testing.T) (*Server, *pgxpool.Pool, []byte, []byte, *tokens.Signer) {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	q := gen.New(pool)
	caPEM, caKeyPEM, err := ca.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	srvPEM, srvKeyPEM, err := ca.IssueServerCert(caPEM, caKeyPEM, ca.ServerCertOptions{
		DNSNames:    []string{"localhost"},
		IPAddresses: []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	signer, err := tokens.NewSignerFromSeed(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	// The agent only advertises CapPinChangeLog+CapSnapshot; do NOT require
	// CapBlobTransfer in RequiredCapabilities so registration succeeds without
	// modifying the agent's registerReq.
	s := New(q, Config{
		ListenAddr:     "127.0.0.1:0",
		Timers:         wireTimers(),
		TLS:            TLSMaterial{CAPEM: caPEM, CertPEM: srvPEM, KeyPEM: srvKeyPEM},
		RepairTokenTTL: time.Hour,
	})
	return s, pool, caPEM, caKeyPEM, signer
}

// pollPinState queries pin_assignments.state for (cid, nodeID) with a deadline.
// Returns the state string when it matches wantState, or fatals on timeout.
func pollPinState(t *testing.T, pool *pgxpool.Pool, cidStr string, nodeID uuid.UUID, wantState gen.PinState, timeout time.Duration) {
	t.Helper()
	ctx := context.Background()
	q := gen.New(pool)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		row, err := q.GetPinAssignment(ctx, gen.GetPinAssignmentParams{
			Cid:    cidStr,
			NodeID: pgtype.UUID{Bytes: nodeID, Valid: true},
		})
		if err == nil && row.State == wantState {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	// Report current state on timeout.
	row, err := q.GetPinAssignment(ctx, gen.GetPinAssignmentParams{
		Cid:    cidStr,
		NodeID: pgtype.UUID{Bytes: nodeID, Valid: true},
	})
	if err != nil {
		t.Fatalf("pollPinState: timeout waiting for %q, GetPinAssignment error: %v", wantState, err)
	}
	t.Fatalf("pollPinState: timeout; want state=%q, got state=%q", wantState, row.State)
}

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

// ---- M4 replication e2e tests -----------------------------------------------

// TestM4ReplicationVerticalSlice exercises the full happy path over loopback mTLS:
// register → AssignPin → /pins/changes mints a Source token → donor fetches
// ciphertext from coordinator-as-source /fed/v1/blob/{cid} → echoPinner returns
// the same CID string → Verify passes → persist verified-ack-pending → Ack →
// coordinator records the donor as a VERIFIED HOLDER (pin_assignments.state='acked').
func TestM4ReplicationVerticalSlice(t *testing.T) {
	ctx := context.Background()

	s, pool, caPEM, caKeyPEM, signer := buildM4Server(t)

	// Wire the canned blob so the source endpoint can serve it.
	ciphertext := []byte("opaque-v1-ciphertext")
	cidStr := mkCID(t, ciphertext)
	s.SetSourceDeps(signer, fakeBackendFor(cidStr, ciphertext), time.Now().Add(-time.Minute))

	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.Run(runCtx)

	// Build a donor client.
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

	// Insert the blob row (the node is assigned AFTER registration succeeds).
	insertBlobRow(t, pool, cidStr, int64(len(ciphertext)))

	// Build the agent with an echoPinner that returns the same CID (happy path).
	dir := t.TempDir()
	progressDir := dir + "/progress"
	prog, err := state.NewFileProgressStore(progressDir)
	if err != nil {
		t.Fatal(err)
	}
	asg := state.NewFileAssignmentStore(dir)
	ag := agent.New(
		&nodeconfig.Config{BandwidthBudgetBytesPerDay: 1},
		state.NewFileRegistrationStore(dir),
		state.NewFileStore(dir),
		asg,
		client,
		time.Hour,           // hbInterval: long so heartbeat never fires in the test
		15*time.Millisecond, // pollInterval: short so sync happens quickly
	)
	ag = agent.WithSource(ag, client, &echoPinner{cid: cidStr, has: true}, prog, 0)

	agCtx, agCancel := context.WithCancel(ctx)
	defer agCancel()
	go ag.Run(agCtx)

	// Wait for the agent to register, then assign the blob to the donor node.
	waitFor(t, 3*time.Second, func() bool {
		_, err := gen.New(pool).GetNodeByID(ctx, pgtype.UUID{Bytes: id, Valid: true})
		return err == nil
	})
	assignViaSeam(t, ctx, pool, cidStr, id)

	// Poll until the coordinator sees the donor as a VERIFIED HOLDER.
	pollPinState(t, pool, cidStr, id, gen.PinStateAcked, 5*time.Second)
}

// TestM4CrashBeforeAckRecovers simulates a donor crash after verify but before ack.
// It pre-seeds the DB assignment and the donor's on-disk progress as
// verified-ack-pending, then builds a fresh agent (WithSource) and calls
// ReconcilePendingAcks directly to verify the coordinator transitions to 'acked'.
func TestM4CrashBeforeAckRecovers(t *testing.T) {
	ctx := context.Background()

	s, pool, caPEM, caKeyPEM, signer := buildM4Server(t)

	ciphertext := []byte("opaque-v1-ciphertext")
	cidStr := mkCID(t, ciphertext)
	s.SetSourceDeps(signer, fakeBackendFor(cidStr, ciphertext), time.Now().Add(-time.Minute))

	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.Run(runCtx)

	// Build the donor client and register the node.
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

	if _, err := client.Register(ctx, wire.RegisterRequest{
		SupportedProtocols: []string{wire.ProtocolV1},
		Capabilities:       []string{wire.CapPinChangeLog, wire.CapSnapshot},
	}); err != nil {
		t.Fatal(err)
	}

	// Assign the blob in the coordinator DB.
	insertBlobRow(t, pool, cidStr, int64(len(ciphertext)))
	assignViaSeam(t, ctx, pool, cidStr, id)

	// Read back the real assignment_id + generation so the progress record matches.
	q := gen.New(pool)
	cur, err := q.GetPinAssignment(ctx, gen.GetPinAssignmentParams{
		Cid:    cidStr,
		NodeID: pgtype.UUID{Bytes: id, Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	asgID := uuid.UUID(cur.AssignmentID.Bytes).String()

	// Pre-seed the donor's on-disk progress as verified-ack-pending (simulating a
	// crash that happened after verify but before ack delivery).
	dir := t.TempDir()
	progressDir := dir + "/progress"
	prog, err := state.NewFileProgressStore(progressDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := prog.Set(cidStr, state.Progress{
		AssignmentID: asgID,
		Generation:   cur.Generation,
		ByteSize:     int64(len(ciphertext)),
		State:        state.ProgressVerifiedPending,
	}); err != nil {
		t.Fatal(err)
	}

	// Pre-seed the local assignment store so ReconcilePendingAcks can look it up.
	asgStore := state.NewFileAssignmentStore(dir)
	if err := asgStore.ApplyChanges([]state.ChangeInput{{
		AssignmentID: asgID,
		Generation:   cur.Generation,
		Kind:         "assign",
		CID:          cidStr,
		ByteSize:     int64(len(ciphertext)),
	}}); err != nil {
		t.Fatal(err)
	}

	// Build a fresh agent; Has=true (pin survived the crash).
	ag := agent.New(
		&nodeconfig.Config{BandwidthBudgetBytesPerDay: 1},
		state.NewFileRegistrationStore(dir),
		state.NewFileStore(dir),
		asgStore,
		client,
		time.Hour,
		time.Hour,
	)
	ag = agent.WithSource(ag, client, &echoPinner{cid: cidStr, has: true}, prog, 0)

	// ReconcilePendingAcks retries the ack without re-fetching.
	ag.ReconcilePendingAcks(ctx)

	// Coordinator must see state=acked.
	row, err := q.GetPinAssignment(ctx, gen.GetPinAssignmentParams{
		Cid:    cidStr,
		NodeID: pgtype.UUID{Bytes: id, Valid: true},
	})
	if err != nil {
		t.Fatalf("GetPinAssignment after reconcile: %v", err)
	}
	if row.State != gen.PinStateAcked {
		t.Fatalf("expected acked after crash-recovery, got %q", row.State)
	}
}

// TestM4CIDMismatchFails wires an echoPinner that returns a DIFFERENT CID so
// Verify fails with cid_mismatch; the donor posts /pins/{cid}/fail and the
// coordinator transitions the pin to 'failed'.
func TestM4CIDMismatchFails(t *testing.T) {
	ctx := context.Background()

	s, pool, caPEM, caKeyPEM, signer := buildM4Server(t)

	ciphertext := []byte("opaque-v1-ciphertext")
	cidStr := mkCID(t, ciphertext)
	wrongCID := mkCID(t, []byte("wrong"))
	s.SetSourceDeps(signer, fakeBackendFor(cidStr, ciphertext), time.Now().Add(-time.Minute))

	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.Run(runCtx)

	// Build a donor client.
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

	// Insert the blob row (the node is assigned AFTER registration succeeds).
	insertBlobRow(t, pool, cidStr, int64(len(ciphertext)))

	// echoPinner returns the wrong CID, so Verify sees root != cid.
	dir := t.TempDir()
	progressDir := dir + "/progress"
	prog, err := state.NewFileProgressStore(progressDir)
	if err != nil {
		t.Fatal(err)
	}
	asg := state.NewFileAssignmentStore(dir)
	ag := agent.New(
		&nodeconfig.Config{BandwidthBudgetBytesPerDay: 1},
		state.NewFileRegistrationStore(dir),
		state.NewFileStore(dir),
		asg,
		client,
		time.Hour,
		15*time.Millisecond,
	)
	ag = agent.WithSource(ag, client, &echoPinner{cid: wrongCID, has: false}, prog, 0)

	agCtx, agCancel := context.WithCancel(ctx)
	defer agCancel()
	go ag.Run(agCtx)

	// Wait for the agent to register, then assign the blob to the donor node.
	waitFor(t, 3*time.Second, func() bool {
		_, err := gen.New(pool).GetNodeByID(ctx, pgtype.UUID{Bytes: id, Valid: true})
		return err == nil
	})
	assignViaSeam(t, ctx, pool, cidStr, id)

	// Poll until the coordinator records the pin as 'failed'.
	pollPinState(t, pool, cidStr, id, gen.PinStateFailed, 5*time.Second)
}
