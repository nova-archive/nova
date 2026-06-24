package source

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/federation/replay"
	"github.com/nova-archive/nova/internal/federation/transport"
	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/node/state"
)

const (
	donorID = "11111111-1111-1111-1111-111111111111"
	testCID = "bafyTESTcid"
)

// --- fakes ------------------------------------------------------------------

type fakePinner struct {
	has  map[string]bool
	body []byte
	// getCalled records that Get was reached (used to assert budget-gating
	// happens BEFORE the body is opened).
	getCalled bool
}

func (p *fakePinner) Has(_ context.Context, cid string) (bool, error) { return p.has[cid], nil }
func (p *fakePinner) Get(_ context.Context, cid string) (io.ReadCloser, error) {
	p.getCalled = true
	return io.NopCloser(bytes.NewReader(p.body)), nil
}

type fakeBudget struct {
	allow bool
	took  int64
}

func (b *fakeBudget) Take(n int64, _ time.Time) bool {
	if b.allow {
		b.took += n
		return true
	}
	return false
}

type fakePubkey struct {
	pub ed25519.PublicKey
	ok  bool
}

func (k *fakePubkey) Current() (ed25519.PublicKey, bool) { return k.pub, k.ok }

type fakeProgress struct {
	m map[string]state.Progress
}

func (p *fakeProgress) Get(cid string) (state.Progress, bool) {
	v, ok := p.m[cid]
	return v, ok
}

// --- token + cert helpers ---------------------------------------------------

func mustKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// signToken mints a wire repair token directly with the coordinator's private
// key (no tokens-package import needed in the donor graph).
func signToken(t *testing.T, priv ed25519.PrivateKey, c wire.Claims) string {
	t.Helper()
	si, err := wire.SigningInput(c)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, []byte(si))
	return wire.AssembleToken(si, sig)
}

// validClaims returns a well-formed read-grant claim bound to this donor.
func validClaims(now time.Time) wire.Claims {
	return wire.Claims{
		JTI:             "jti-" + now.Format("150405.000000000"),
		AssignmentID:    "asg-1",
		Generation:      3,
		CID:             testCID,
		SourceNodeID:    donorID,
		DestNodeID:      wire.CoordinatorSourceID,
		NotBefore:       now.Add(-time.Second).Unix(),
		NotAfter:        now.Add(time.Hour).Unix(),
		MaxBytes:        1 << 20,
		ProtocolVersion: wire.ProtocolV1,
	}
}

// coordCert builds a leaf cert with the coordinator URI SAN so
// transport.IdentityFromCert returns RoleCoordinator. role="node" yields a
// donor identity (wrong role for the source server).
func makeCert(t *testing.T, role string) *x509.Certificate {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: role},
		URIs:         []*url.URL{{Scheme: "nova", Host: role, Path: "/" + donorID}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(nil, tmpl, tmpl, priv.Public(), priv)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// reqWith builds a request carrying a TLS peer leaf and a repair-token header.
func reqWith(cid, token string, leaf *x509.Certificate) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/fed/v1/blob/"+cid, nil)
	r.SetPathValue("cid", cid)
	if token != "" {
		r.Header.Set("X-Nova-Repair-Token", token)
	}
	if leaf != nil {
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	}
	return r
}

// fixture wires a server with sane happy-path defaults; tests mutate fakes.
type fixture struct {
	srv      *Server
	pinner   *fakePinner
	budget   *fakeBudget
	pubkey   *fakePubkey
	progress *fakeProgress
	priv     ed25519.PrivateKey
	boot     time.Time
	now      time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pub, priv := mustKey(t)
	now := time.Now()
	boot := now.Add(-time.Minute)
	f := &fixture{
		pinner:   &fakePinner{has: map[string]bool{testCID: true}, body: bytes.Repeat([]byte("z"), 2048)},
		budget:   &fakeBudget{allow: true},
		pubkey:   &fakePubkey{pub: pub, ok: true},
		progress: &fakeProgress{m: map[string]state.Progress{testCID: {AssignmentID: "asg-1", Generation: 3, ByteSize: 2048, State: state.ProgressAckDelivered}}},
		priv:     priv,
		boot:     boot,
		now:      now,
	}
	f.srv = NewServer(Deps{
		Pinner:      f.pinner,
		Budget:      f.budget,
		PubKey:      f.pubkey,
		Progress:    f.progress,
		NodeID:      donorID,
		BootTime:    boot,
		ReplayCache: replay.New(),
		Now:         func() time.Time { return f.now },
	})
	return f
}

func (f *fixture) do(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.srv.ServeHTTP(w, r)
	return w
}

func decodeErr(t *testing.T, w *httptest.ResponseRecorder) wire.ErrorResponse {
	t.Helper()
	var e wire.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode error body %q: %v", w.Body.String(), err)
	}
	return e
}

// --- happy path -------------------------------------------------------------

func TestServeHappyPath(t *testing.T) {
	f := newFixture(t)
	tok := signToken(t, f.priv, validClaims(f.now))
	w := f.do(reqWith(testCID, tok, makeCert(t, "coordinator")))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), f.pinner.body) {
		t.Fatalf("served %d bytes, want %d", w.Body.Len(), len(f.pinner.body))
	}
	if f.budget.took != 2048 {
		t.Fatalf("budget debited %d, want 2048", f.budget.took)
	}
}

// --- refusal table ----------------------------------------------------------

func TestRefuseWrongRole(t *testing.T) {
	f := newFixture(t)
	tok := signToken(t, f.priv, validClaims(f.now))
	w := f.do(reqWith(testCID, tok, makeCert(t, "node")))
	if w.Code != http.StatusForbidden {
		t.Fatalf("code=%d", w.Code)
	}
	if decodeErr(t, w).Code != "source_unauthorized" {
		t.Fatalf("code=%s", decodeErr(t, w).Code)
	}
}

func TestRefuseNoPeerCert(t *testing.T) {
	f := newFixture(t)
	tok := signToken(t, f.priv, validClaims(f.now))
	w := f.do(reqWith(testCID, tok, nil))
	if w.Code != http.StatusForbidden || decodeErr(t, w).Code != "source_unauthorized" {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRefuseNoPubkey503(t *testing.T) {
	f := newFixture(t)
	f.pubkey.ok = false
	tok := signToken(t, f.priv, validClaims(f.now))
	w := f.do(reqWith(testCID, tok, makeCert(t, "coordinator")))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d", w.Code)
	}
	if decodeErr(t, w).Code != "source_unavailable" {
		t.Fatalf("code=%s", decodeErr(t, w).Code)
	}
}

func TestRefuseForgedToken(t *testing.T) {
	f := newFixture(t)
	_, wrongPriv := mustKey(t)
	tok := signToken(t, wrongPriv, validClaims(f.now))
	w := f.do(reqWith(testCID, tok, makeCert(t, "coordinator")))
	if w.Code != http.StatusForbidden || decodeErr(t, w).Code != "source_unauthorized" {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRefuseMissingToken(t *testing.T) {
	f := newFixture(t)
	w := f.do(reqWith(testCID, "", makeCert(t, "coordinator")))
	if w.Code != http.StatusForbidden || decodeErr(t, w).Code != "source_unauthorized" {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRefuseSourceMismatch(t *testing.T) {
	f := newFixture(t)
	c := validClaims(f.now)
	c.SourceNodeID = "someone-else"
	tok := signToken(t, f.priv, c)
	w := f.do(reqWith(testCID, tok, makeCert(t, "coordinator")))
	if w.Code != http.StatusForbidden || decodeErr(t, w).Code != "source_unauthorized" {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRefuseDestMismatch(t *testing.T) {
	f := newFixture(t)
	c := validClaims(f.now)
	c.DestNodeID = "not-the-coordinator"
	tok := signToken(t, f.priv, c)
	w := f.do(reqWith(testCID, tok, makeCert(t, "coordinator")))
	if w.Code != http.StatusForbidden || decodeErr(t, w).Code != "source_unauthorized" {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRefuseCIDMismatch(t *testing.T) {
	f := newFixture(t)
	c := validClaims(f.now)
	c.CID = "bafyOTHER"
	tok := signToken(t, f.priv, c) // token says bafyOTHER but path says testCID
	w := f.do(reqWith(testCID, tok, makeCert(t, "coordinator")))
	if w.Code != http.StatusForbidden || decodeErr(t, w).Code != "source_unauthorized" {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRefuseMissingProgress404(t *testing.T) {
	f := newFixture(t)
	delete(f.progress.m, testCID)
	tok := signToken(t, f.priv, validClaims(f.now))
	w := f.do(reqWith(testCID, tok, makeCert(t, "coordinator")))
	if w.Code != http.StatusNotFound || decodeErr(t, w).Code != "blob_unavailable" {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRefuseStaleProgressState404(t *testing.T) {
	f := newFixture(t)
	p := f.progress.m[testCID]
	p.State = state.ProgressVerifiedPending // not yet acked-delivered
	f.progress.m[testCID] = p
	tok := signToken(t, f.priv, validClaims(f.now))
	w := f.do(reqWith(testCID, tok, makeCert(t, "coordinator")))
	if w.Code != http.StatusNotFound || decodeErr(t, w).Code != "blob_unavailable" {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRefuseWrongGeneration404(t *testing.T) {
	f := newFixture(t)
	p := f.progress.m[testCID]
	p.Generation = 99 // progress at a different generation than the token
	f.progress.m[testCID] = p
	tok := signToken(t, f.priv, validClaims(f.now))
	w := f.do(reqWith(testCID, tok, makeCert(t, "coordinator")))
	if w.Code != http.StatusNotFound || decodeErr(t, w).Code != "blob_unavailable" {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRefuseWrongAssignment404(t *testing.T) {
	f := newFixture(t)
	p := f.progress.m[testCID]
	p.AssignmentID = "asg-other"
	f.progress.m[testCID] = p
	tok := signToken(t, f.priv, validClaims(f.now))
	w := f.do(reqWith(testCID, tok, makeCert(t, "coordinator")))
	if w.Code != http.StatusNotFound || decodeErr(t, w).Code != "blob_unavailable" {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRefusePreBootFloor(t *testing.T) {
	f := newFixture(t)
	c := validClaims(f.now)
	c.NotBefore = f.boot.Add(-time.Minute).Unix() // minted before this server booted
	tok := signToken(t, f.priv, c)
	w := f.do(reqWith(testCID, tok, makeCert(t, "coordinator")))
	if w.Code != http.StatusForbidden || decodeErr(t, w).Code != "source_unauthorized" {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRefuseReplay(t *testing.T) {
	f := newFixture(t)
	tok := signToken(t, f.priv, validClaims(f.now))
	leaf := makeCert(t, "coordinator")
	if w := f.do(reqWith(testCID, tok, leaf)); w.Code != http.StatusOK {
		t.Fatalf("first use code=%d body=%s", w.Code, w.Body.String())
	}
	// Second use of the same JTI must be refused as a replay.
	w := f.do(reqWith(testCID, tok, leaf))
	if w.Code != http.StatusForbidden || decodeErr(t, w).Code != "source_unauthorized" {
		t.Fatalf("replay code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRefuseNotPinned404(t *testing.T) {
	f := newFixture(t)
	f.pinner.has[testCID] = false
	tok := signToken(t, f.priv, validClaims(f.now))
	w := f.do(reqWith(testCID, tok, makeCert(t, "coordinator")))
	if w.Code != http.StatusNotFound || decodeErr(t, w).Code != "blob_unavailable" {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestRefuseTooLarge413(t *testing.T) {
	f := newFixture(t)
	c := validClaims(f.now)
	c.MaxBytes = 100 // progress ByteSize is 2048 > 100
	tok := signToken(t, f.priv, c)
	w := f.do(reqWith(testCID, tok, makeCert(t, "coordinator")))
	if w.Code != http.StatusRequestEntityTooLarge || decodeErr(t, w).Code != "blob_too_large" {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if f.pinner.getCalled {
		t.Fatal("must not open body when size exceeds max_bytes")
	}
}

func TestRefuseBudgetExhausted429(t *testing.T) {
	f := newFixture(t)
	f.budget.allow = false
	tok := signToken(t, f.priv, validClaims(f.now))
	w := f.do(reqWith(testCID, tok, makeCert(t, "coordinator")))
	if w.Code != http.StatusTooManyRequests || decodeErr(t, w).Code != "budget_exceeded" {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if f.pinner.getCalled {
		t.Fatal("must not open body when budget is exhausted")
	}
}

func TestRefuseZeroSize404(t *testing.T) {
	f := newFixture(t)
	p := f.progress.m[testCID]
	p.ByteSize = 0 // no recorded envelope size
	f.progress.m[testCID] = p
	tok := signToken(t, f.priv, validClaims(f.now))
	w := f.do(reqWith(testCID, tok, makeCert(t, "coordinator")))
	if w.Code != http.StatusNotFound || decodeErr(t, w).Code != "blob_unavailable" {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

// --- pubkey provider --------------------------------------------------------

func TestKeyProviderSetCurrent(t *testing.T) {
	pub, _ := mustKey(t)
	kp := &KeyProvider{}
	if _, ok := kp.Current(); ok {
		t.Fatal("expected no key before Set")
	}
	kp.Set(pub)
	got, ok := kp.Current()
	if !ok || !got.Equal(pub) {
		t.Fatalf("Current after Set: ok=%v equal=%v", ok, got.Equal(pub))
	}
}

// ensure the cert helper produces something IdentityFromCert reads as expected.
func TestMakeCertRole(t *testing.T) {
	id, err := transport.IdentityFromCert(makeCert(t, "coordinator"))
	if err != nil {
		t.Fatal(err)
	}
	if id.Role != transport.RoleCoordinator {
		t.Fatalf("role=%v", id.Role)
	}
}
