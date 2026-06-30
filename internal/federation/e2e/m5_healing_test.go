// Package e2e holds the P2-M5 functional chaos proof. This file exercises the
// milestone's novel transport — donor↔donor repair — end-to-end over a genuine
// mTLS handshake against the REAL donor source server (role + signed grant +
// dest-binding + egress-debit + exactly-size verify chain). The DB-driven liveness
// → projection → strict-Tier-1 scheduling chain is proven by the orchestrator
// integration tests; here we prove the wire/auth/egress path the source server
// gained in M5 (RoleNode repair callers).
package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/federation/ca"
	"github.com/nova-archive/nova/internal/federation/replay"
	"github.com/nova-archive/nova/internal/federation/tokens"
	"github.com/nova-archive/nova/internal/federation/transport"
	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/node/bandwidth"
	"github.com/nova-archive/nova/internal/node/source"
	"github.com/nova-archive/nova/internal/node/state"
	"github.com/stretchr/testify/require"
)

// --- minimal donor-side fakes (the source server's injected collaborators) -----

type memPinner struct {
	data map[string][]byte
}

func (p *memPinner) Has(_ context.Context, cid string) (bool, error) {
	_, ok := p.data[cid]
	return ok, nil
}
func (p *memPinner) Get(_ context.Context, cid string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(p.data[cid])), nil
}

type oneProgress struct {
	cid string
	p   state.Progress
}

func (o oneProgress) Get(cid string) (state.Progress, bool) {
	if cid == o.cid {
		return o.p, true
	}
	return state.Progress{}, false
}

// srcDonor is a running real-mTLS donor source server.
type srcDonor struct {
	nodeID uuid.UUID
	addr   string
	budget *bandwidth.Bucket
	ln     net.Listener
	srv    *http.Server
}

func (d *srcDonor) close() {
	_ = d.srv.Close()
	_ = d.ln.Close()
}

// startSourceDonor stands up the REAL source server holding `env` for `cid` with an
// acked progress record for the SOURCE's assignment, over loopback mTLS.
func startSourceDonor(t *testing.T, caPEM, caKeyPEM []byte, pub ed25519.PublicKey, cid string, env []byte, srcAssignID uuid.UUID, srcGen, byteSize, dailyBudget int64) *srcDonor {
	t.Helper()
	nodeID := uuid.New()
	srvPEM, srvKeyPEM, err := ca.IssueServerCert(caPEM, caKeyPEM, ca.ServerCertOptions{
		DNSNames: []string{"localhost"}, IPAddresses: []string{"127.0.0.1"},
	})
	require.NoError(t, err)
	tlsCfg, err := transport.ServerTLSConfig(caPEM, srvPEM, srvKeyPEM)
	require.NoError(t, err)

	budget := bandwidth.NewDailyBucket(dailyBudget, time.Now())
	handler := source.NewServer(source.Deps{
		Pinner:      &memPinner{data: map[string][]byte{cid: env}},
		Budget:      budget,
		PubKey:      staticPub{pub: pub},
		Progress:    oneProgress{cid: cid, p: state.Progress{AssignmentID: srcAssignID.String(), Generation: srcGen, ByteSize: byteSize, State: state.ProgressAckDelivered}},
		NodeID:      nodeID.String(),
		BootTime:    time.Now().Add(-time.Minute),
		ReplayCache: replay.New(),
		Now:         time.Now,
	})
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ln := transport.NewTLSListener(inner, tlsCfg)
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return &srcDonor{nodeID: nodeID, addr: inner.Addr().String(), budget: budget, ln: ln, srv: srv}
}

type staticPub struct{ pub ed25519.PublicKey }

func (s staticPub) Current() (ed25519.PublicKey, bool) { return s.pub, true }

// destClient builds a real mTLS client presenting a nova://node/<destNodeID> cert,
// and fetches GET /fed/v1/blob/{cid} with the grant.
func fetchRepair(t *testing.T, caPEM, caKeyPEM []byte, destNodeID uuid.UUID, srcAddr, cid, token string) (*http.Response, []byte) {
	t.Helper()
	cliPEM, cliKeyPEM, err := ca.IssueClientCert(caPEM, caKeyPEM, destNodeID, "donor")
	require.NoError(t, err)
	cliTLS, err := transport.ClientTLSConfig(caPEM, cliPEM, cliKeyPEM)
	require.NoError(t, err)
	cliTLS.ServerName = "localhost"
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: cliTLS}, Timeout: 5 * time.Second}

	req, err := http.NewRequest(http.MethodGet, "https://"+srcAddr+"/fed/v1/blob/"+cid, nil)
	require.NoError(t, err)
	req.Header.Set("X-Nova-Repair-Token", token)
	resp, err := hc.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, body
}

// mintRepairGrant builds a donor↔donor grant: SourceNodeID names the source's acked
// assignment (AssignmentID/Generation), Dest* bind the destination's pending one.
func mintRepairGrant(t *testing.T, signer *tokens.Signer, src, dest uuid.UUID, cid string, srcAssignID uuid.UUID, srcGen int64, destAssignID uuid.UUID, destGen, maxBytes int64) string {
	t.Helper()
	now := time.Now()
	tok, err := signer.Mint(wire.Claims{
		JTI: uuid.New().String(), AssignmentID: srcAssignID.String(), Generation: srcGen, CID: cid,
		SourceNodeID: src.String(), DestNodeID: dest.String(),
		NotBefore: now.Add(-5 * time.Second).Unix(), NotAfter: now.Add(time.Hour).Unix(),
		MaxBytes: maxBytes, ProtocolVersion: wire.ProtocolV1,
		DestAssignmentID: destAssignID.String(), DestGeneration: destGen,
	})
	require.NoError(t, err)
	return tok
}

func TestM5HealingDonorToDonorRepairOverMTLS(t *testing.T) {
	caPEM, caKeyPEM, err := ca.GenerateCA()
	require.NoError(t, err)
	signer, err := tokens.NewSignerFromSeed(make([]byte, 32))
	require.NoError(t, err)
	pub, err := wire.DecodePublicKey(signer.PublicKeyWire())
	require.NoError(t, err)

	const cid = "bafyREPAIRe2e"
	env := bytes.Repeat([]byte("E"), 4096)
	srcAssign, destAssign := uuid.New(), uuid.New()
	dest := uuid.New() // the destination donor's node id (presented via its client cert)

	// Source donor with ample budget; one repair fits.
	srcD := startSourceDonor(t, caPEM, caKeyPEM, pub, cid, env, srcAssign, 3, int64(len(env)), 1<<20)
	defer srcD.close()

	t.Run("happy donor-to-donor repair fetch debits egress", func(t *testing.T) {
		before := srcD.budget.Remaining(time.Now())
		tok := mintRepairGrant(t, signer, srcD.nodeID, dest, cid, srcAssign, 3, destAssign, 2, int64(len(env)))
		resp, body := fetchRepair(t, caPEM, caKeyPEM, dest, srcD.addr, cid, tok)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, env, body, "source streams exactly the ciphertext envelope")
		after := srcD.budget.Remaining(time.Now())
		require.LessOrEqual(t, after, before-int64(len(env)), "the source donor's egress bucket is debited")
	})

	t.Run("grant minted for another dest is refused", func(t *testing.T) {
		other := uuid.New()
		tok := mintRepairGrant(t, signer, srcD.nodeID, other, cid, srcAssign, 3, destAssign, 2, int64(len(env)))
		// `dest` presents its own cert but the grant names `other` as dest.
		resp, _ := fetchRepair(t, caPEM, caKeyPEM, dest, srcD.addr, cid, tok)
		require.Equal(t, http.StatusForbidden, resp.StatusCode, "a grant for donor B can't be replayed by donor C")
	})

	t.Run("over-budget serve is refused and serves no body", func(t *testing.T) {
		// A source donor whose daily budget is smaller than the blob refuses.
		poor := startSourceDonor(t, caPEM, caKeyPEM, pub, cid, env, srcAssign, 3, int64(len(env)), 100)
		defer poor.close()
		tok := mintRepairGrant(t, signer, poor.nodeID, dest, cid, srcAssign, 3, destAssign, 2, int64(len(env)))
		resp, body := fetchRepair(t, caPEM, caKeyPEM, dest, poor.addr, cid, tok)
		require.Equal(t, http.StatusTooManyRequests, resp.StatusCode, "budget_exceeded")
		require.NotEqual(t, env, body, "no ciphertext served when the egress budget is exhausted")
		require.Contains(t, string(body), "budget_exceeded")
	})
}
