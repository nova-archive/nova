# P2-M2 Identity, Registration, Capability Negotiation — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up Nova's live federation control channel — a coordinator mTLS listener serving `/fed/v1/register` + `/fed/v1/heartbeat`, donor identity from the verified federation cert, fail-closed capability negotiation, `trust_state` assignment, and the `novactl node` CA/cert/revocation tooling — with no pins, transfers, healing, or audits.

**Architecture:** A standalone `internal/federation/coordinator` mTLS server (constructed + run by `cmd/coordinator` as a second listener, never folded into the public `pkg/coordinator`), a shared stdlib-only `internal/federation/transport` (TLS config + cert→identity), a pure-Go `internal/federation/ca` X.509 CA used by `novactl node`, a `nodes`-scoped migration `0011`, and a real donor register→heartbeat agent over an injected client with atomic-JSON local state. Nebula stays an external sidecar (templates + docs only).

**Tech Stack:** Go (stdlib `crypto/tls`, `crypto/x509`, `crypto/ed25519`), pgx/sqlc, goose migrations, `github.com/google/uuid`, `gopkg.in/yaml.v3`.

**Design:** [`../../specs/phase2/2026-06-16-phase2-m2-identity-registration-design.md`](../../specs/phase2/2026-06-16-phase2-m2-identity-registration-design.md)

---

## File structure

**Shared (both binaries):**
- `internal/federation/wire/messages.go` — *modify*: extend `RegisterRequest`, `HeartbeatResponse`; add `ConfigUpdates`.
- `internal/federation/transport/identity.go` — *create*: `Identity`, `FingerprintDER`, `IdentityFromCert`. Stdlib only, **no uuid**.
- `internal/federation/transport/tls.go` — *create*: `ServerTLSConfig`, `ClientTLSConfig`.

**Coordinator / operator only:**
- `internal/federation/ca/ca.go` — *create*: Ed25519 X.509 CA + server/client cert issuance.
- `internal/federation/coordinator/server.go` — *create*: `Server`, `Listen`, `Run`, mux.
- `internal/federation/coordinator/handlers.go` — *create*: register + heartbeat handlers + authorization.
- `internal/db/migrations/0011_node_registration.sql` — *create*.
- `internal/db/queries/federation.sql` — *create*; regenerates `internal/db/gen/federation.sql.go`.
- `internal/config/types.go` — *modify*: extend `Federation`.
- `internal/config/federation.go` — *create*: `Federation` defaults + `nebula_interface` validation.
- `cmd/coordinator/main.go` — *modify*: build + run the federation server (dual listener); resolve federation config.
- `cmd/novactl/node.go` — *create*: `node ca-init|issue|revoke|rotate-cert|list|nebula-template`.
- `cmd/novactl/templates/` — *create*: Nebula `config.yml` + README + donor `node.yaml` + compose templates.

**Donor only:**
- `internal/node/state/registration.go` — *create*: `RegistrationStore` + atomic-JSON `FileRegistrationStore`.
- `internal/node/agent/agent.go` — *modify*: real register→heartbeat loop + `Client` interface.
- `internal/node/agent/client.go` — *create*: mTLS `HTTPClient` implementing `Client`.
- `cmd/node/main.go` — *modify*: build the mTLS client + wire the real agent.

**Boundary / deploy:**
- `scripts/check_node_deps.sh` — *modify*: add `internal/federation/transport` to the allowlist.
- `deploy/donor/compose.yaml`, `deploy/operator/` — *modify/create*: Nebula sidecar wiring.

---

## Task 1: Extend the shared wire message types

**Files:**
- Modify: `internal/federation/wire/messages.go`
- Test: `internal/federation/wire/messages_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `internal/federation/wire/messages_test.go`:

```go
package wire

import (
	"encoding/json"
	"testing"
)

func TestRegisterRequestRoundTrip(t *testing.T) {
	in := RegisterRequest{
		SupportedProtocols:        []string{ProtocolV1},
		Capabilities:              []string{},
		ClientVersion:             "0.2.0",
		NebulaCertFingerprint:     "sha256:nebula",
		FederationCertFingerprint: "sha256:fed",
		DisplayName:               "donor-a",
		GeoDeclared:               "DE",
		CapacityBytes:             1 << 40,
		BandwidthBudgetBytesPerDay: 1 << 35,
		PolicyFilters:             map[string]any{"max_blob_bytes": float64(1048576)},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out RegisterRequest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.DisplayName != "donor-a" || out.CapacityBytes != 1<<40 || out.FederationCertFingerprint != "sha256:fed" {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}

func TestHeartbeatResponseShape(t *testing.T) {
	r := HeartbeatResponse{
		ConfigUpdates: &ConfigUpdates{HeartbeatIntervalSeconds: 300},
		CurrentEpoch:  0,
	}
	b, _ := json.Marshal(r)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"config_updates", "current_epoch"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing key %q in %s", k, b)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/federation/wire/ -run 'TestRegisterRequestRoundTrip|TestHeartbeatResponseShape' -v`
Expected: FAIL — compile error (`ConfigUpdates` undefined; new fields missing).

- [ ] **Step 3: Edit `internal/federation/wire/messages.go`**

Replace the `RegisterRequest` struct with the extended form and the `HeartbeatResponse` struct, and add `ConfigUpdates`:

```go
// RegisterRequest is sent by a donor; identity is derived from the verified
// mTLS cert, NOT these fields (D-cap). The fingerprints are echoed for
// cross-check against the verified peer cert; the rest are self-declared
// registration attributes plus the negotiation inputs.
type RegisterRequest struct {
	SupportedProtocols         []string       `json:"supported_protocols"`
	Capabilities               []string       `json:"capabilities"`
	ClientVersion              string         `json:"client_version,omitempty"`
	NebulaCertFingerprint      string         `json:"nebula_cert_fingerprint,omitempty"`
	FederationCertFingerprint  string         `json:"federation_cert_fingerprint,omitempty"`
	DisplayName                string         `json:"display_name,omitempty"`
	GeoDeclared                string         `json:"geo_declared,omitempty"`
	CapacityBytes              int64          `json:"capacity_bytes,omitempty"`
	BandwidthBudgetBytesPerDay int64          `json:"bandwidth_budget_bytes_per_day,omitempty"`
	PolicyFilters              map[string]any `json:"policy_filters,omitempty"`
}
```

```go
// ConfigUpdates carries operator-tunable federation timers back to a donor on
// each heartbeat so it can be retuned without redeploy.
type ConfigUpdates struct {
	HeartbeatIntervalSeconds int `json:"heartbeat_interval_seconds,omitempty"`
	PinsPollIntervalSeconds  int `json:"pins_poll_interval_seconds,omitempty"`
	MaxPinConcurrency        int `json:"max_pin_concurrency,omitempty"`
}

type HeartbeatResponse struct {
	ConfigUpdates        *ConfigUpdates `json:"config_updates"`
	CurrentEpoch         int64          `json:"current_epoch"`
	RepairTokenPublicKey string         `json:"repair_token_public_key,omitempty"` // empty until M4 (D1)
}
```

Leave `RegisterResponse`, `HeartbeatRequest`, `ChangesRequest/Response`, `PinChange`, `Ack`, `Fail` unchanged.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/federation/wire/ -v`
Expected: PASS (existing `capability_test.go` + `token_test.go` still green).

- [ ] **Step 5: Commit**

```bash
git add internal/federation/wire/messages.go internal/federation/wire/messages_test.go
git commit -m "feat(p2-m2): extend fed/v1 wire types for register/heartbeat (P2-M2)"
```

---

## Task 2: Transport — identity from the verified cert

**Files:**
- Create: `internal/federation/transport/identity.go`
- Test: `internal/federation/transport/identity_test.go`

- [ ] **Step 1: Write the failing test**

```go
package transport

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/url"
	"testing"
	"time"
)

// testLeaf builds a self-signed leaf carrying the given URI SAN (or none).
func testLeaf(t *testing.T, uri string) *x509.Certificate {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	if uri != "" {
		u, err := url.Parse(uri)
		if err != nil {
			t.Fatal(err)
		}
		tmpl.URIs = []*url.URL{u}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c
}

func TestIdentityFromCert(t *testing.T) {
	c := testLeaf(t, "nova://node/550e8400-e29b-41d4-a716-446655440000")
	id, err := IdentityFromCert(c)
	if err != nil {
		t.Fatal(err)
	}
	if id.NodeID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Fatalf("node id = %q", id.NodeID)
	}
	if len(id.Fingerprint) != len("sha256:")+64 || id.Fingerprint[:7] != "sha256:" {
		t.Fatalf("fingerprint = %q", id.Fingerprint)
	}
	// Fingerprint is over the DER and is stable.
	if FingerprintDER(c) != id.Fingerprint {
		t.Fatal("fingerprint mismatch")
	}
}

func TestIdentityFromCertNoSAN(t *testing.T) {
	if _, err := IdentityFromCert(testLeaf(t, "")); err == nil {
		t.Fatal("expected error for cert without nova://node SAN")
	}
}

func TestIdentityFromCertWrongScheme(t *testing.T) {
	if _, err := IdentityFromCert(testLeaf(t, "https://example.com/x")); err == nil {
		t.Fatal("expected error for non-nova URI SAN")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/federation/transport/ -v`
Expected: FAIL — `IdentityFromCert` / `FingerprintDER` undefined.

- [ ] **Step 3: Create `internal/federation/transport/identity.go`**

```go
// Package transport holds the shared mTLS-over-Nebula helpers: building the
// client/server *tls.Config against the federation CA, and extracting a donor's
// stable identity from a verified federation certificate. It is pure stdlib
// (crypto/tls, crypto/x509) so it stays inside the donor dependency boundary;
// it imports NO uuid library — node_id is carried as an opaque validated string.
package transport

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// uriScheme + uriHost define the URI SAN that carries a donor's node_id:
// nova://node/<uuid>. The UUID is minted by `novactl node issue` at cert-issue
// time and is the stable identity; the coordinator parses it as a real UUID.
const (
	uriScheme = "nova"
	uriHost   = "node"
)

// Identity is the verified federation identity of a peer.
type Identity struct {
	NodeID      string // the URI-SAN UUID string (validated structurally, not parsed)
	Fingerprint string // "sha256:" + hex(sha256(leaf DER))
}

// FingerprintDER returns the canonical fingerprint of a certificate: the SHA-256
// of its DER encoding, hex-encoded, prefixed "sha256:". DER (not SPKI) so the
// fingerprint tracks the certificate — correct for rotation/revocation
// bookkeeping.
func FingerprintDER(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// IdentityFromCert extracts the node_id (from the nova://node/<uuid> URI SAN)
// and the DER fingerprint from a verified leaf. It does minimal structural
// validation only — no uuid parsing (that is the coordinator's job, off the
// donor graph).
func IdentityFromCert(c *x509.Certificate) (Identity, error) {
	for _, u := range c.URIs {
		if u.Scheme != uriScheme || u.Host != uriHost {
			continue
		}
		id := strings.TrimPrefix(u.Path, "/")
		if id == "" {
			return Identity{}, errors.New("transport: empty node_id in URI SAN")
		}
		return Identity{NodeID: id, Fingerprint: FingerprintDER(c)}, nil
	}
	return Identity{}, fmt.Errorf("transport: cert has no %s://%s/<id> URI SAN", uriScheme, uriHost)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/federation/transport/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/federation/transport/identity.go internal/federation/transport/identity_test.go
git commit -m "feat(p2-m2): federation transport identity (DER fp + URI-SAN node_id) (P2-M2)"
```

---

## Task 3: Transport — mTLS config builders

**Files:**
- Create: `internal/federation/transport/tls.go`
- Test: `internal/federation/transport/tls_test.go`

- [ ] **Step 1: Write the failing test**

This test does a real loopback mTLS handshake using certs from the CA package, so it depends on Task 4 conceptually — but to keep tasks independent it generates its own throwaway CA + certs inline.

```go
package transport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// pemPair returns (certPEM, keyPEM) for a cert signed by (caCert, caKey), or a
// self-signed CA when parent is nil. uri is an optional URI SAN.
func pemPair(t *testing.T, parent *x509.Certificate, parentKey ed25519.PrivateKey, isCA bool, uri string, dns []string) ([]byte, []byte, *x509.Certificate, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "t"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	if isCA {
		tmpl.IsCA = true
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	}
	tmpl.DNSNames = dns
	if uri != "" {
		u, _ := url.Parse(uri)
		tmpl.URIs = []*url.URL{u}
	}
	signer, signerKey := tmpl, priv
	if parent != nil {
		signer, signerKey = parent, parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, pub, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	keyDER, _ := x509.MarshalPKCS8PrivateKey(priv)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, cert, priv
}

func TestMutualTLSHandshake(t *testing.T) {
	caPEM, caKeyPEM, caCert, caKey := pemPair(t, nil, nil, true, "", nil)
	_ = caKeyPEM
	srvPEM, srvKeyPEM, _, _ := pemPair(t, caCert, caKey, false, "", []string{"localhost"})
	cliPEM, cliKeyPEM, _, _ := pemPair(t, caCert, caKey, false, "nova://node/abc", nil)

	srvTLS, err := ServerTLSConfig(caPEM, srvPEM, srvKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cliTLS, err := ClientTLSConfig(caPEM, cliPEM, cliKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	cliTLS.ServerName = "localhost"

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(r.TLS.PeerCertificates) == 0 {
				w.WriteHeader(401)
				return
			}
			io.WriteString(w, "ok")
		}),
		TLSConfig: srvTLS,
	}
	tlsLn := newTLSListener(ln, srvTLS)
	go srv.Serve(tlsLn)
	defer srv.Close()

	c := &http.Client{Transport: &http.Transport{TLSClientConfig: cliTLS}, Timeout: 3 * time.Second}
	resp, err := c.Get("https://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("handshake/get failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_ = context.Background()
}
```

Note: `newTLSListener` is a tiny helper added in Step 3 (`tls.NewListener`) so the test and `Run` share one wrapper.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/federation/transport/ -run TestMutualTLSHandshake -v`
Expected: FAIL — `ServerTLSConfig` / `ClientTLSConfig` / `newTLSListener` undefined.

- [ ] **Step 3: Create `internal/federation/transport/tls.go`**

```go
package transport

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
)

// caPool parses a PEM CA bundle into a CertPool.
func caPool(caPEM []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("transport: no certificates parsed from CA PEM")
	}
	return pool, nil
}

// ServerTLSConfig builds the coordinator federation listener's TLS config:
// present the server cert, and REQUIRE + VERIFY client certs against the
// federation CA. TLS 1.2 floor (Ed25519 leaves require 1.2+; 1.3 preferred).
func ServerTLSConfig(caPEM, certPEM, keyPEM []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("transport: server keypair: %w", err)
	}
	pool, err := caPool(caPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ClientTLSConfig builds the donor's mTLS client config: present the client
// cert, verify the coordinator's server cert against the federation CA.
func ClientTLSConfig(caPEM, certPEM, keyPEM []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("transport: client keypair: %w", err)
	}
	pool, err := caPool(caPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// newTLSListener wraps a net.Listener in a TLS listener using cfg.
func newTLSListener(inner net.Listener, cfg *tls.Config) net.Listener {
	return tls.NewListener(inner, cfg)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/federation/transport/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/federation/transport/tls.go internal/federation/transport/tls_test.go
git commit -m "feat(p2-m2): federation transport mTLS config builders (P2-M2)"
```

---

## Task 4: Federation X.509 CA (`novactl node` crypto core)

**Files:**
- Create: `internal/federation/ca/ca.go`
- Test: `internal/federation/ca/ca_test.go`

This is operator-side (used by `novactl`, never by `cmd/node`). It may use `github.com/google/uuid`.

- [ ] **Step 1: Write the failing test**

```go
package ca

import (
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/federation/transport"
)

func parseLeaf(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	blk, _ := pem.Decode(certPEM)
	if blk == nil {
		t.Fatal("no PEM")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGenerateCAAndIssue(t *testing.T) {
	caCertPEM, caKeyPEM, err := GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	// Server cert verifies against the CA.
	srvPEM, _, err := IssueServerCert(caCertPEM, caKeyPEM, ServerCertOptions{DNSNames: []string{"coordinator.local"}})
	if err != nil {
		t.Fatal(err)
	}
	// Client cert carries the node_id URI SAN and verifies.
	id := uuid.New()
	cliPEM, _, err := IssueClientCert(caCertPEM, caKeyPEM, id, "donor-a")
	if err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCertPEM) {
		t.Fatal("ca pool")
	}
	for _, leafPEM := range [][]byte{srvPEM, cliPEM} {
		leaf := parseLeaf(t, leafPEM)
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
			t.Fatalf("verify: %v", err)
		}
	}

	gotID, err := transport.IdentityFromCert(parseLeaf(t, cliPEM))
	if err != nil {
		t.Fatal(err)
	}
	if gotID.NodeID != id.String() {
		t.Fatalf("node id = %q want %q", gotID.NodeID, id.String())
	}
}

func TestIssueClientCertRejectsBadCA(t *testing.T) {
	if _, _, err := IssueClientCert([]byte("not pem"), []byte("not pem"), uuid.New(), "x"); err == nil {
		t.Fatal("expected error on bad CA material")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/federation/ca/ -v`
Expected: FAIL — package/functions undefined.

- [ ] **Step 3: Create `internal/federation/ca/ca.go`**

```go
// Package ca is the Nova federation X.509 certificate authority used by
// `novactl node` to issue the coordinator's federation server cert and donor
// federation client certs. Ed25519 keys, pure crypto/x509 — no external binary
// and no Nebula involvement (Nebula has its own separate cert system). This
// package is OPERATOR-SIDE and must never enter the cmd/node build graph.
package ca

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"time"

	"github.com/google/uuid"
)

const (
	caValidity     = 10 * 365 * 24 * time.Hour
	leafValidity   = 2 * 365 * 24 * time.Hour
	uriSANTemplate = "nova://node/%s"
)

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func encodePair(certDER []byte, key ed25519.PrivateKey) (certPEM, keyPEM []byte, err error) {
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// GenerateCA creates a self-signed Ed25519 federation CA.
func GenerateCA() (certPEM, keyPEM []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: "Nova Federation CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(caValidity),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, nil, err
	}
	return encodePair(der, priv)
}

// parseCA decodes a CA cert + key PEM pair.
func parseCA(caCertPEM, caKeyPEM []byte) (*x509.Certificate, ed25519.PrivateKey, error) {
	cblk, _ := pem.Decode(caCertPEM)
	kblk, _ := pem.Decode(caKeyPEM)
	if cblk == nil || kblk == nil {
		return nil, nil, fmt.Errorf("ca: malformed CA PEM")
	}
	caCert, err := x509.ParseCertificate(cblk.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("ca: parse CA cert: %w", err)
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(kblk.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("ca: parse CA key: %w", err)
	}
	key, ok := keyAny.(ed25519.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("ca: CA key is not Ed25519")
	}
	return caCert, key, nil
}

// ServerCertOptions configures the coordinator's federation server cert.
type ServerCertOptions struct {
	DNSNames []string
	// IPAddresses may include the coordinator's Nebula overlay IP, as strings.
	IPAddresses []string
}

func issueLeaf(caCertPEM, caKeyPEM []byte, tmpl *x509.Certificate) (certPEM, keyPEM []byte, err error) {
	caCert, caKey, err := parseCA(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, nil, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, err
	}
	tmpl.SerialNumber = sn
	tmpl.NotBefore = time.Now().Add(-time.Hour)
	tmpl.NotAfter = time.Now().Add(leafValidity)
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	tmpl.BasicConstraintsValid = true
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, pub, caKey)
	if err != nil {
		return nil, nil, err
	}
	return encodePair(der, priv)
}

// IssueServerCert issues the coordinator's federation server cert.
func IssueServerCert(caCertPEM, caKeyPEM []byte, opts ServerCertOptions) (certPEM, keyPEM []byte, err error) {
	tmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: "nova-coordinator-federation"},
		DNSNames:    opts.DNSNames,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, ip := range opts.IPAddresses {
		if parsed := parseIP(ip); parsed != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, parsed)
		}
	}
	return issueLeaf(caCertPEM, caKeyPEM, tmpl)
}

// IssueClientCert issues a donor federation client cert binding node_id in the
// URI SAN (nova://node/<uuid>).
func IssueClientCert(caCertPEM, caKeyPEM []byte, nodeID uuid.UUID, displayName string) (certPEM, keyPEM []byte, err error) {
	u, err := url.Parse(fmt.Sprintf(uriSANTemplate, nodeID.String()))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: displayName},
		URIs:        []*url.URL{u},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	return issueLeaf(caCertPEM, caKeyPEM, tmpl)
}
```

Add the small IP helper at the bottom of the file:

```go
import "net" // add to the import block

func parseIP(s string) net.IP { return net.ParseIP(s) }
```

(Merge the `net` import into the existing import block rather than a second block.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/federation/ca/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/federation/ca/
git commit -m "feat(p2-m2): federation X.509 CA + server/client cert issuance (P2-M2)"
```

---

## Task 5: Migration `0011` + node-registration queries + sqlc regen

**Files:**
- Create: `internal/db/migrations/0011_node_registration.sql`
- Create: `internal/db/queries/federation.sql`
- Modify: `internal/db/migrations/MANIFEST.sha256` (append)
- Regenerate: `internal/db/gen/federation.sql.go` (+ `models.go`)

- [ ] **Step 1: Create the migration**

`internal/db/migrations/0011_node_registration.sql`:

```sql
-- +goose Up
-- +goose StatementBegin
-- P2-M2: node-registration columns. trust_state is text+CHECK (not an enum):
-- the classification is young and a text domain is cheaper to amend than
-- ALTER TYPE. D8 failure-domain / placement_weight columns are deferred to P2-M5
-- (nothing places pins in M2); pin_changes/assignment generation are P2-M3.
ALTER TABLE nodes
    ADD COLUMN trust_state text NOT NULL DEFAULT 'probationary'
        CHECK (trust_state IN ('probationary', 'trusted', 'suspended')),
    ADD COLUMN selected_protocol       text,
    ADD COLUMN advertised_capabilities text[] NOT NULL DEFAULT '{}',
    ADD COLUMN required_capabilities   text[] NOT NULL DEFAULT '{}',
    ADD COLUMN client_version          text,
    ADD COLUMN cert_revoked_at          timestamptz,
    ADD COLUMN cert_rotation_started_at timestamptz,
    ADD COLUMN cert_rotated_at          timestamptz,
    ADD COLUMN last_free_bytes   bigint CHECK (last_free_bytes   IS NULL OR last_free_bytes   >= 0),
    ADD COLUMN last_stored_bytes bigint CHECK (last_stored_bytes IS NULL OR last_stored_bytes >= 0);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE nodes
    DROP COLUMN trust_state,
    DROP COLUMN selected_protocol,
    DROP COLUMN advertised_capabilities,
    DROP COLUMN required_capabilities,
    DROP COLUMN client_version,
    DROP COLUMN cert_revoked_at,
    DROP COLUMN cert_rotation_started_at,
    DROP COLUMN cert_rotated_at,
    DROP COLUMN last_free_bytes,
    DROP COLUMN last_stored_bytes;
-- +goose StatementEnd
```

- [ ] **Step 2: Create the queries**

`internal/db/queries/federation.sql`:

```sql
-- name: GetNodeByID :one
SELECT * FROM nodes WHERE id = $1;

-- name: RegisterNode :one
INSERT INTO nodes (
    id, nebula_cert_fingerprint, federation_cert_fingerprint, display_name,
    geo_declared, capacity_bytes, bandwidth_budget_bytes_per_day, policy_filters,
    status, trust_state, selected_protocol, advertised_capabilities,
    required_capabilities, client_version
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8,
    'active', 'probationary', $9, $10, $11, $12
)
ON CONFLICT (id) DO UPDATE SET
    nebula_cert_fingerprint        = EXCLUDED.nebula_cert_fingerprint,
    display_name                   = EXCLUDED.display_name,
    geo_declared                   = EXCLUDED.geo_declared,
    capacity_bytes                 = EXCLUDED.capacity_bytes,
    bandwidth_budget_bytes_per_day = EXCLUDED.bandwidth_budget_bytes_per_day,
    policy_filters                 = EXCLUDED.policy_filters,
    selected_protocol              = EXCLUDED.selected_protocol,
    advertised_capabilities        = EXCLUDED.advertised_capabilities,
    required_capabilities          = EXCLUDED.required_capabilities,
    client_version                 = EXCLUDED.client_version
RETURNING *;

-- name: UpdateNodeHeartbeat :one
UPDATE nodes
SET last_seen_at = now(), last_free_bytes = $2, last_stored_bytes = $3
WHERE id = $1
RETURNING *;

-- name: RevokeNode :execrows
UPDATE nodes
SET status = 'revoked', cert_revoked_at = now(), last_status_change_at = now()
WHERE id = $1 AND status <> 'revoked';

-- name: RotateNodeCert :execrows
UPDATE nodes
SET federation_cert_fingerprint = $2,
    cert_rotation_started_at = now(),
    cert_rotated_at = now()
WHERE id = $1;

-- name: ListNodes :many
SELECT id, display_name, status, trust_state, selected_protocol, last_seen_at
FROM nodes
ORDER BY joined_at DESC;
```

Note: `RegisterNode`'s upsert deliberately does **not** update `federation_cert_fingerprint` — first contact sets it; rotation is operator-driven via `RotateNodeCert`.

- [ ] **Step 3: Regenerate sqlc + append manifest**

Run:
```bash
make sqlc-generate
(cd internal/db/migrations && sha256sum 0011_node_registration.sql >> MANIFEST.sha256)
```

- [ ] **Step 4: Verify codegen + frozen gate + migration applies**

Run:
```bash
go build ./internal/db/...
make migrations-frozen
go test ./internal/db/migrations/ -v
```
Expected: build OK; `migrations-frozen` prints success; migration tests PASS. (If a Postgres-backed migration test is gated on `DATABASE_URL`/`TEST_DATABASE_URL`, run it where one is available.)

- [ ] **Step 5: Commit**

```bash
git add internal/db/migrations/0011_node_registration.sql internal/db/migrations/MANIFEST.sha256 internal/db/queries/federation.sql internal/db/gen/
git commit -m "feat(p2-m2): nodes registration migration 0011 + federation queries (P2-M2)"
```

---

## Task 6: Coordinator federation server — register handler

**Files:**
- Create: `internal/federation/coordinator/server.go` (skeleton + helpers)
- Create: `internal/federation/coordinator/handlers.go` (register)
- Test: `internal/federation/coordinator/register_test.go`

- [ ] **Step 1: Write the failing test**

This is a DB-backed test using `internal/dbtest`. It drives the handler directly with a synthetic `*http.Request` carrying a verified peer cert in `r.TLS`.

```go
package coordinator

import (
	"bytes"
	"context"
	"crypto/x509"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/federation/ca"
	"github.com/nova-archive/nova/internal/federation/transport"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// issuedClient returns a parsed client leaf for nodeID signed by caPEM/caKeyPEM.
func issuedClient(t *testing.T, caPEM, caKeyPEM []byte, nodeID uuid.UUID) *x509.Certificate {
	t.Helper()
	cliPEM, _, err := ca.IssueClientCert(caPEM, caKeyPEM, nodeID, "donor")
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pemDecode(t, cliPEM)
	c, err := x509.ParseCertificate(blk)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// reqWithCert builds a request whose TLS state presents leaf as the peer cert.
func reqWithCert(method, path string, body []byte, leaf *x509.Certificate) *http.Request {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	return r
}

func newTestServer(t *testing.T) (*Server, []byte, []byte) {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	caPEM, caKeyPEM, err := ca.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	s := New(gen.New(pool), Config{
		RequiredCapabilities: nil,
		Timers:               wire.ConfigUpdates{HeartbeatIntervalSeconds: 300, PinsPollIntervalSeconds: 600, MaxPinConcurrency: 16},
	})
	return s, caPEM, caKeyPEM
}

func TestRegisterFirstAndIdempotent(t *testing.T) {
	s, caPEM, caKeyPEM := newTestServer(t)
	id := uuid.New()
	leaf := issuedClient(t, caPEM, caKeyPEM, id)
	body, _ := json.Marshal(wire.RegisterRequest{
		SupportedProtocols:        []string{wire.ProtocolV1},
		Capabilities:              []string{},
		FederationCertFingerprint: transport.FingerprintDER(leaf),
		DisplayName:               "donor-a",
		CapacityBytes:             1 << 40,
	})

	// First register → 201, stable node_id.
	w := httptest.NewRecorder()
	s.handleRegister(w, reqWithCert(http.MethodPost, "/fed/v1/register", body, leaf))
	if w.Code != http.StatusCreated {
		t.Fatalf("first register = %d (%s)", w.Code, w.Body)
	}
	var resp wire.RegisterResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.NodeID != id.String() || resp.SelectedProtocol != wire.ProtocolV1 {
		t.Fatalf("resp = %+v", resp)
	}

	// Re-register with the same cert → 200, same node_id.
	w2 := httptest.NewRecorder()
	s.handleRegister(w2, reqWithCert(http.MethodPost, "/fed/v1/register", body, leaf))
	if w2.Code != http.StatusOK {
		t.Fatalf("re-register = %d", w2.Code)
	}
}

func TestRegisterIncompatibleProtocol(t *testing.T) {
	s, caPEM, caKeyPEM := newTestServer(t)
	leaf := issuedClient(t, caPEM, caKeyPEM, uuid.New())
	body, _ := json.Marshal(wire.RegisterRequest{SupportedProtocols: []string{"fed/v2"}})
	w := httptest.NewRecorder()
	s.handleRegister(w, reqWithCert(http.MethodPost, "/fed/v1/register", body, leaf))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", w.Code)
	}
	var e wire.ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &e)
	if e.Code != "incompatible_protocol" {
		t.Fatalf("code = %q", e.Code)
	}
}

func TestRegisterMissingCapabilityFailsClosed(t *testing.T) {
	s, caPEM, caKeyPEM := newTestServer(t)
	s.cfg.RequiredCapabilities = []string{wire.CapPinChangeLog}
	leaf := issuedClient(t, caPEM, caKeyPEM, uuid.New())
	body, _ := json.Marshal(wire.RegisterRequest{SupportedProtocols: []string{wire.ProtocolV1}, Capabilities: []string{}})
	w := httptest.NewRecorder()
	s.handleRegister(w, reqWithCert(http.MethodPost, "/fed/v1/register", body, leaf))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", w.Code)
	}
}
```

Add a tiny `pemDecode` test helper in this file:

```go
func pemDecode(t *testing.T, p []byte) ([]byte, []byte) {
	t.Helper()
	blk, rest := pemBlock(p)
	if blk == nil {
		t.Fatal("no PEM block")
	}
	return blk, rest
}
```

…where `pemBlock` wraps `encoding/pem`.Decode — add `import "encoding/pem"` and:

```go
func pemBlock(p []byte) ([]byte, []byte) {
	b, rest := pem.Decode(p)
	if b == nil {
		return nil, rest
	}
	return b.Bytes, rest
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/federation/coordinator/ -v`
Expected: FAIL — `New`, `Server`, `Config`, `handleRegister` undefined.

- [ ] **Step 3: Create `internal/federation/coordinator/server.go`**

```go
// Package coordinator is the operator-side federation server: the mTLS listener
// (separate from the public/admin mux) serving /fed/v1/register + heartbeat.
// Identity derives from the verified federation client cert. Operator-only —
// never in the cmd/node build graph.
package coordinator

import (
	"encoding/json"
	"net/http"

	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// Config is the federation server's static configuration.
type Config struct {
	ListenAddr           string
	RequiredCapabilities []string         // [] in M2 — see design § capability negotiation
	Timers               wire.ConfigUpdates // delivered to donors via heartbeat config_updates
	TLS                  TLSMaterial
}

// TLSMaterial holds the PEM bytes for the federation listener.
type TLSMaterial struct {
	CAPEM   []byte
	CertPEM []byte
	KeyPEM  []byte
}

// Server serves the federation control endpoints.
type Server struct {
	q   *gen.Queries
	cfg Config
}

// New constructs a federation Server. The listener is built later by Listen.
func New(q *gen.Queries, cfg Config) *Server { return &Server{q: q, cfg: cfg} }

// mux returns the federation HTTP routes.
func (s *Server) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("/fed/v1/register", s.handleRegister)
	m.HandleFunc("/fed/v1/heartbeat", s.handleHeartbeat)
	return m
}

// writeError emits a normalized fed/v1 error with the given status.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(wire.ErrorResponse{Code: code, Message: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
```

- [ ] **Step 4: Create `internal/federation/coordinator/handlers.go` (register)**

```go
package coordinator

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/transport"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// authenticate extracts the verified federation identity from the request's
// peer certificate. The mTLS listener guarantees a verified cert is present;
// this also guards direct handler tests / misconfiguration.
func (s *Server) authenticate(r *http.Request) (transport.Identity, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return transport.Identity{}, errors.New("no peer certificate")
	}
	return transport.IdentityFromCert(r.TLS.PeerCertificates[0])
}

func pgText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	id, err := s.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", err.Error())
		return
	}
	nodeUUID, err := uuid.Parse(id.NodeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_node_id", "node_id URI SAN is not a UUID")
		return
	}
	var req wire.RegisterRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "malformed register body")
		return
	}

	// Protocol negotiation.
	if !contains(req.SupportedProtocols, wire.ProtocolV1) {
		writeError(w, http.StatusBadRequest, "incompatible_protocol", "no common fed/v1")
		return
	}
	// Capability negotiation (fail-closed). M2 required set is empty.
	required := s.cfg.RequiredCapabilities
	if missing, ok := wire.NegotiateCapabilities(req.Capabilities, required); !ok {
		writeError(w, http.StatusBadRequest, "missing_capability", join(missing))
		return
	}
	// Fingerprint cross-check (reported must match verified leaf).
	if req.FederationCertFingerprint != "" && req.FederationCertFingerprint != id.Fingerprint {
		writeError(w, http.StatusBadRequest, "fingerprint_mismatch", "reported fingerprint != verified cert")
		return
	}

	ctx := r.Context()
	pgID := pgtype.UUID{Bytes: nodeUUID, Valid: true}
	existing, err := s.q.GetNodeByID(ctx, pgID)
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "internal", "lookup failed")
		return
	}
	if found {
		if existing.Status == gen.NodeStatusRevoked {
			writeError(w, http.StatusForbidden, "node_revoked", "")
			return
		}
		if existing.FederationCertFingerprint != id.Fingerprint {
			writeError(w, http.StatusForbidden, "fingerprint_mismatch", "presented cert is not the active cert")
			return
		}
	}

	policy, _ := json.Marshal(req.PolicyFilters)
	if req.PolicyFilters == nil {
		policy = []byte("{}")
	}
	if required == nil {
		required = []string{}
	}
	caps := req.Capabilities
	if caps == nil {
		caps = []string{}
	}
	if _, err := s.q.RegisterNode(ctx, gen.RegisterNodeParams{
		ID:                         pgID,
		NebulaCertFingerprint:      req.NebulaCertFingerprint,
		FederationCertFingerprint:  id.Fingerprint,
		DisplayName:                pgText(req.DisplayName),
		GeoDeclared:                pgText(req.GeoDeclared),
		CapacityBytes:              req.CapacityBytes,
		BandwidthBudgetBytesPerDay: req.BandwidthBudgetBytesPerDay,
		PolicyFilters:              policy,
		SelectedProtocol:           pgText(wire.ProtocolV1),
		AdvertisedCapabilities:     caps,
		RequiredCapabilities:       required,
		ClientVersion:              pgText(req.ClientVersion),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "register failed")
		return
	}

	status := http.StatusCreated
	if found {
		status = http.StatusOK
	}
	writeJSON(w, status, wire.RegisterResponse{
		SelectedProtocol:     wire.ProtocolV1,
		RequiredCapabilities: required,
		NodeID:               id.NodeID,
	})
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

func join(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}
```

> The exact `gen.RegisterNodeParams` field names + `gen.NodeStatusRevoked` constant come from Task 5's sqlc output. If a field name differs (e.g. `gen.NodeStatusRevoked` vs `gen.NodeStatusRevoked`), match the generated identifiers — do not invent.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/federation/coordinator/ -run TestRegister -v` (with a test Postgres available)
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/federation/coordinator/server.go internal/federation/coordinator/handlers.go internal/federation/coordinator/register_test.go
git commit -m "feat(p2-m2): coordinator federation register handler + negotiation (P2-M2)"
```

---

## Task 7: Heartbeat handler + identity/revocation enforcement

**Files:**
- Modify: `internal/federation/coordinator/handlers.go` (add `handleHeartbeat`)
- Test: `internal/federation/coordinator/heartbeat_test.go`

- [ ] **Step 1: Write the failing test**

```go
package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/federation/wire"
)

func registerOK(t *testing.T, s *Server, caPEM, caKeyPEM []byte, id uuid.UUID) {
	t.Helper()
	leaf := issuedClient(t, caPEM, caKeyPEM, id)
	body, _ := json.Marshal(wire.RegisterRequest{SupportedProtocols: []string{wire.ProtocolV1}})
	w := httptest.NewRecorder()
	s.handleRegister(w, reqWithCert(http.MethodPost, "/fed/v1/register", body, leaf))
	if w.Code != http.StatusCreated {
		t.Fatalf("setup register = %d (%s)", w.Code, w.Body)
	}
}

func TestHeartbeatUpdatesAndConfig(t *testing.T) {
	s, caPEM, caKeyPEM := newTestServer(t)
	id := uuid.New()
	registerOK(t, s, caPEM, caKeyPEM, id)
	leaf := issuedClient(t, caPEM, caKeyPEM, id)

	body, _ := json.Marshal(wire.HeartbeatRequest{FreeBytes: 100, StoredBytes: 200})
	w := httptest.NewRecorder()
	s.handleHeartbeat(w, reqWithCert(http.MethodPost, "/fed/v1/heartbeat", body, leaf))
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d (%s)", w.Code, w.Body)
	}
	var resp wire.HeartbeatResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ConfigUpdates == nil || resp.ConfigUpdates.HeartbeatIntervalSeconds != 300 {
		t.Fatalf("config_updates = %+v", resp.ConfigUpdates)
	}
	if resp.CurrentEpoch != 0 || resp.RepairTokenPublicKey != "" {
		t.Fatalf("epoch/repair-key = %d/%q", resp.CurrentEpoch, resp.RepairTokenPublicKey)
	}
}

func TestHeartbeatUnregistered403(t *testing.T) {
	s, caPEM, caKeyPEM := newTestServer(t)
	leaf := issuedClient(t, caPEM, caKeyPEM, uuid.New()) // never registered
	w := httptest.NewRecorder()
	s.handleHeartbeat(w, reqWithCert(http.MethodPost, "/fed/v1/heartbeat", []byte(`{}`), leaf))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestRevokedCertRejected(t *testing.T) {
	ctx := context.Background()
	s, caPEM, caKeyPEM := newTestServer(t)
	id := uuid.New()
	registerOK(t, s, caPEM, caKeyPEM, id)
	if _, err := s.q.RevokeNode(ctx, pgUUID(id)); err != nil {
		t.Fatal(err)
	}
	leaf := issuedClient(t, caPEM, caKeyPEM, id)
	w := httptest.NewRecorder()
	s.handleHeartbeat(w, reqWithCert(http.MethodPost, "/fed/v1/heartbeat", []byte(`{}`), leaf))
	if w.Code != http.StatusForbidden {
		t.Fatalf("revoked heartbeat = %d", w.Code)
	}
	_ = bytes.TrimSpace
}
```

Add a `pgUUID` helper to the test (or to `handlers.go` as exported-for-test); simplest is a test helper in this file:

```go
func pgUUID(id uuid.UUID) pgtypeUUID { return toPG(id) }
```

To avoid importing pgtype in two test files, add the conversion to `handlers.go` as an unexported helper and a tiny test shim. Concretely, add to `handlers.go`:

```go
// pgUUIDFrom converts a uuid.UUID to pgtype.UUID.
func pgUUIDFrom(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }
```

…and in the test reference `s.q.RevokeNode(ctx, pgUUIDFrom(id))` directly (drop the `pgUUID`/`pgtypeUUID`/`toPG` shim above). Update the register handler to use `pgUUIDFrom(nodeUUID)` too for consistency.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/federation/coordinator/ -run 'Heartbeat|Revoked' -v`
Expected: FAIL — `handleHeartbeat` undefined.

- [ ] **Step 3: Add `handleHeartbeat` to `handlers.go`**

```go
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	id, err := s.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", err.Error())
		return
	}
	nodeUUID, err := uuid.Parse(id.NodeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_node_id", "")
		return
	}
	ctx := r.Context()
	node, err := s.q.GetNodeByID(ctx, pgUUIDFrom(nodeUUID))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusForbidden, "registration_required", "node must register first")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "lookup failed")
		return
	}
	if node.Status == gen.NodeStatusRevoked {
		writeError(w, http.StatusForbidden, "node_revoked", "")
		return
	}
	if node.FederationCertFingerprint != id.Fingerprint {
		writeError(w, http.StatusForbidden, "fingerprint_mismatch", "presented cert is not the active cert")
		return
	}

	var req wire.HeartbeatRequest
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req) // tolerant: empty body ok

	if _, err := s.q.UpdateNodeHeartbeat(ctx, gen.UpdateNodeHeartbeatParams{
		ID:              pgUUIDFrom(nodeUUID),
		LastFreeBytes:   pgInt8(req.FreeBytes),
		LastStoredBytes: pgInt8(req.StoredBytes),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "heartbeat failed")
		return
	}

	timers := s.cfg.Timers
	writeJSON(w, http.StatusOK, wire.HeartbeatResponse{
		ConfigUpdates:        &timers,
		CurrentEpoch:         0,  // M3 gives this meaning
		RepairTokenPublicKey: "", // M4 populates
	})
}

// pgInt8 wraps a non-negative byte count into a nullable bigint.
func pgInt8(v int64) pgtype.Int8 { return pgtype.Int8{Int64: v, Valid: true} }
```

> Match `gen.UpdateNodeHeartbeatParams` field names + the `LastFreeBytes`/`LastStoredBytes` pg types (`pgtype.Int8`) to the sqlc output from Task 5.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/federation/coordinator/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/federation/coordinator/handlers.go internal/federation/coordinator/heartbeat_test.go
git commit -m "feat(p2-m2): coordinator heartbeat + revocation/identity enforcement (P2-M2)"
```

---

## Task 8: Federation server — Listen + Run (mTLS listener)

**Files:**
- Modify: `internal/federation/coordinator/server.go` (add `Listen`, `Run`, `Addr`)
- Test: `internal/federation/coordinator/server_test.go`

- [ ] **Step 1: Write the failing test**

```go
package coordinator

import (
	"context"
	"crypto/tls"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/federation/ca"
	"github.com/nova-archive/nova/internal/federation/transport"
	"github.com/nova-archive/nova/internal/federation/wire"
)

func TestServerListenServesOverMTLS(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	caPEM, caKeyPEM, _ := ca.GenerateCA()
	srvPEM, srvKeyPEM, _ := ca.IssueServerCert(caPEM, caKeyPEM, ca.ServerCertOptions{DNSNames: []string{"localhost"}, IPAddresses: []string{"127.0.0.1"}})

	s := New(gen.New(pool), Config{
		ListenAddr: "127.0.0.1:0",
		Timers:     wire.ConfigUpdates{HeartbeatIntervalSeconds: 300},
		TLS:        TLSMaterial{CAPEM: caPEM, CertPEM: srvPEM, KeyPEM: srvKeyPEM},
	})
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	go s.Run(runCtx)
	defer cancel()

	// Build a donor client.
	id := uuid.New()
	cliPEM, cliKeyPEM, _ := ca.IssueClientCert(caPEM, caKeyPEM, id, "donor")
	cliTLS, _ := transport.ClientTLSConfig(caPEM, cliPEM, cliKeyPEM)
	cliTLS.ServerName = "localhost"
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: cliTLS}, Timeout: 3 * time.Second}

	// register over the real listener.
	resp, err := hc.Post("https://"+s.Addr()+"/fed/v1/register", "application/json",
		mustBody(wire.RegisterRequest{SupportedProtocols: []string{wire.ProtocolV1}}))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// A client with NO cert cannot handshake.
	noCert := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{ServerName: "localhost", RootCAs: cliTLS.RootCAs}}, Timeout: 2 * time.Second}
	if _, err := noCert.Get("https://" + s.Addr() + "/fed/v1/register"); err == nil {
		t.Fatal("expected handshake failure without client cert")
	}
}
```

Add `mustBody` helper (json-marshal to an `io.Reader`) in this test file.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/federation/coordinator/ -run TestServerListen -v`
Expected: FAIL — `Listen`/`Run`/`Addr` undefined.

- [ ] **Step 3: Add to `server.go`**

```go
import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/nova-archive/nova/internal/federation/transport"
)

// Server gains a bound listener + server.
type … // (extend the existing struct)
//   ln   net.Listener
//   srv  *http.Server

// Listen binds the mTLS listener. Call before Run so a bad bind fails fast
// (the coordinator must not declare startup success with a dead federation
// listener — cmd/coordinator binds this BEFORE serving the public mux).
func (s *Server) Listen() error {
	tlsCfg, err := transport.ServerTLSConfig(s.cfg.TLS.CAPEM, s.cfg.TLS.CertPEM, s.cfg.TLS.KeyPEM)
	if err != nil {
		return err
	}
	raw, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return err
	}
	s.ln = transport.NewTLSListener(raw, tlsCfg)
	s.srv = &http.Server{Handler: s.mux(), ReadHeaderTimeout: 10 * time.Second}
	return nil
}

// Addr returns the bound address (useful with :0). Empty until Listen.
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Run serves until ctx is cancelled, then drains. Listen must be called first.
func (s *Server) Run(ctx context.Context) error {
	if s.ln == nil {
		return errors.New("coordinator: federation Listen() not called")
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(sctx)
	}()
	if err := s.srv.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
```

Add the `ln net.Listener` and `srv *http.Server` fields to the `Server` struct. Export the listener wrapper from transport by renaming `newTLSListener` → `NewTLSListener` in `transport/tls.go` (update Task 3's test reference accordingly), since the coordinator needs it too.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/federation/coordinator/ -v && go test ./internal/federation/transport/ -v`
Expected: PASS (both packages).

- [ ] **Step 5: Commit**

```bash
git add internal/federation/coordinator/server.go internal/federation/coordinator/server_test.go internal/federation/transport/tls.go internal/federation/transport/tls_test.go
git commit -m "feat(p2-m2): federation server Listen/Run mTLS listener (P2-M2)"
```

---

## Task 9: Operator `federation` config block + nebula_interface guard

**Files:**
- Modify: `internal/config/types.go` (`Federation` struct)
- Create: `internal/config/federation.go` (defaults + validation)
- Test: `internal/config/federation_test.go`

- [ ] **Step 1: Write the failing test**

```go
package config

import "testing"

func TestFederationValidateLoopbackSkipsInterfaceGuard(t *testing.T) {
	f := Federation{
		ListenAddr:         "127.0.0.1:9443",
		NebulaInterface:    "nebula1",
		FederationCAPath:   "x", FederationCertPath: "y", FederationKeyPath: "z",
	}
	if err := f.Validate(true /* dev */); err != nil {
		t.Fatalf("loopback dev should skip interface guard: %v", err)
	}
}

func TestFederationValidateRequiresMaterialWhenEnabled(t *testing.T) {
	f := Federation{ListenAddr: "10.42.0.1:9443"}
	if err := f.Validate(false); err == nil {
		t.Fatal("expected error: missing cert paths")
	}
}

func TestFederationValidateDisabledWhenNoListen(t *testing.T) {
	if err := (Federation{}).Validate(false); err != nil {
		t.Fatalf("empty federation (disabled) must validate: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestFederation -v`
Expected: FAIL — new fields + `Validate` undefined.

- [ ] **Step 3: Extend `internal/config/types.go`**

Replace the `Federation` struct with:

```go
type Federation struct {
	// M2 additions: listener + mTLS material.
	ListenAddr         string `yaml:"listen_addr"`
	NebulaInterface    string `yaml:"nebula_interface"`
	FederationCAPath   string `yaml:"federation_ca_path"`
	FederationCertPath string `yaml:"federation_cert_path"`
	FederationKeyPath  string `yaml:"federation_key_path"`

	// Existing timers.
	HeartbeatIntervalSeconds     int `yaml:"heartbeat_interval_seconds"`
	PinsPollIntervalSeconds      int `yaml:"pins_poll_interval_seconds"`
	MaxPinConcurrency            int `yaml:"max_pin_concurrency"`
	SuspectAfterMissedHeartbeats int `yaml:"suspect_after_missed_heartbeats"`
	UnreachableAfterSeconds      int `yaml:"unreachable_after_seconds"`
	EvictedAfterSeconds          int `yaml:"evicted_after_seconds"`
}

// Enabled reports whether the federation listener should run (operator set a
// listen_addr).
func (f Federation) Enabled() bool { return f.ListenAddr != "" }
```

- [ ] **Step 4: Create `internal/config/federation.go`**

```go
package config

import (
	"fmt"
	"net"
)

// Default federation timers (mirrors FEDERATION_PROTOCOL.md).
const (
	DefaultHeartbeatIntervalSeconds = 300
	DefaultPinsPollIntervalSeconds  = 600
	DefaultMaxPinConcurrency        = 16
)

// Validate checks the federation block. dev=true (loopback/test) skips the
// interface-membership guard. A disabled block (no listen_addr) is always valid.
func (f Federation) Validate(dev bool) error {
	if !f.Enabled() {
		return nil
	}
	if _, _, err := net.SplitHostPort(f.ListenAddr); err != nil {
		return fmt.Errorf("federation.listen_addr %q is not host:port: %w", f.ListenAddr, err)
	}
	for name, p := range map[string]string{
		"federation_ca_path":   f.FederationCAPath,
		"federation_cert_path": f.FederationCertPath,
		"federation_key_path":  f.FederationKeyPath,
	} {
		if p == "" {
			return fmt.Errorf("federation.%s is required when listen_addr is set", name)
		}
	}
	if f.NebulaInterface != "" && !dev {
		if err := f.checkListenOnInterface(); err != nil {
			return err
		}
	}
	return nil
}

// checkListenOnInterface verifies the listen IP belongs to nebula_interface —
// catching the accidental 0.0.0.0/public-interface foot-gun at boot.
func (f Federation) checkListenOnInterface() error {
	host, _, _ := net.SplitHostPort(f.ListenAddr)
	ifi, err := net.InterfaceByName(f.NebulaInterface)
	if err != nil {
		return fmt.Errorf("federation.nebula_interface %q: %w", f.NebulaInterface, err)
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return err
	}
	for _, a := range addrs {
		ip, _, _ := net.ParseCIDR(a.String())
		if ip != nil && ip.String() == host {
			return nil
		}
	}
	return fmt.Errorf("federation.listen_addr host %q is not an address of interface %q", host, f.NebulaInterface)
}

// FederationTimers fills defaults and returns the timer triple delivered to
// donors via heartbeat config_updates.
func (f Federation) FederationTimers() (heartbeat, poll, concurrency int) {
	heartbeat, poll, concurrency = f.HeartbeatIntervalSeconds, f.PinsPollIntervalSeconds, f.MaxPinConcurrency
	if heartbeat == 0 {
		heartbeat = DefaultHeartbeatIntervalSeconds
	}
	if poll == 0 {
		poll = DefaultPinsPollIntervalSeconds
	}
	if concurrency == 0 {
		concurrency = DefaultMaxPinConcurrency
	}
	return heartbeat, poll, concurrency
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: PASS (existing config tests still green).

- [ ] **Step 6: Commit**

```bash
git add internal/config/types.go internal/config/federation.go internal/config/federation_test.go
git commit -m "feat(p2-m2): operator federation config block + interface guard (P2-M2)"
```

---

## Task 10: Wire the federation server into `cmd/coordinator` (dual listener)

**Files:**
- Modify: `cmd/coordinator/main.go`
- Test: `cmd/coordinator/main_test.go` (add a run-loop unit test)

- [ ] **Step 1: Write the failing test**

Add to `cmd/coordinator/main_test.go`:

```go
func TestRunBothStopsOnFirstError(t *testing.T) {
	ctx := context.Background()
	good := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	bad := func(ctx context.Context) error { return errors.New("boom") }
	err := runBoth(ctx, bad, good)
	if err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v, want boom (and good must be cancelled)", err)
	}
}
```

(Imports `context`, `errors`, `testing` — add any missing.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/coordinator/ -run TestRunBoth -v`
Expected: FAIL — `runBoth` undefined.

- [ ] **Step 3: Add `runBoth` + federation wiring to `cmd/coordinator/main.go`**

Add the helper:

```go
// runBoth runs each function concurrently under a derived context; the first
// non-nil error cancels the rest and is returned. A clean (nil) return from one
// runner does NOT cancel the others — only an error does.
func runBoth(ctx context.Context, runs ...func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, len(runs))
	for _, run := range runs {
		run := run
		go func() { errc <- run(ctx) }()
	}
	var first error
	for range runs {
		if e := <-errc; e != nil && first == nil {
			first = e
			cancel()
		}
	}
	return first
}
```

In `run()`, after `c, err := coordinator.New(...)` and product registration, just before `return c.Run(ctx)`, build + bind the federation server when enabled, then run both:

```go
	// Federation control channel (P2-M2). Enabled when operator.yaml sets
	// federation.listen_addr. Bound BEFORE serving so a dead federation listener
	// fails startup rather than leaving the public coordinator silently up.
	if opCfg != nil && opCfg.Federation.Enabled() {
		fed := opCfg.Federation
		dev := isLoopback(fed.ListenAddr)
		if err := fed.Validate(dev); err != nil {
			return fmt.Errorf("federation config: %w", err)
		}
		caPEM, err := os.ReadFile(fed.FederationCAPath)
		if err != nil {
			return fmt.Errorf("federation ca: %w", err)
		}
		certPEM, err := os.ReadFile(fed.FederationCertPath)
		if err != nil {
			return fmt.Errorf("federation cert: %w", err)
		}
		keyPEM, err := os.ReadFile(fed.FederationKeyPath)
		if err != nil {
			return fmt.Errorf("federation key: %w", err)
		}
		hb, poll, conc := fed.FederationTimers()
		fedSrv := fedcoord.New(gen.New(pool), fedcoord.Config{
			ListenAddr: fed.ListenAddr,
			Timers:     wire.ConfigUpdates{HeartbeatIntervalSeconds: hb, PinsPollIntervalSeconds: poll, MaxPinConcurrency: conc},
			TLS:        fedcoord.TLSMaterial{CAPEM: caPEM, CertPEM: certPEM, KeyPEM: keyPEM},
		})
		if err := fedSrv.Listen(); err != nil {
			return fmt.Errorf("federation listen %s: %w", fed.ListenAddr, err)
		}
		slog.Info("federation listener bound", "listen", fedSrv.Addr())
		return runBoth(ctx, c.Run, fedSrv.Run)
	}
	return c.Run(ctx)
```

Add imports: `fedcoord "github.com/nova-archive/nova/internal/federation/coordinator"`, `"github.com/nova-archive/nova/internal/federation/wire"`. Add a small helper:

```go
// isLoopback reports whether addr's host is a loopback address (dev/test mode
// where the nebula_interface guard is skipped).
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
```

Add `"net"` to the import block if absent.

- [ ] **Step 4: Run tests + build**

Run: `go test ./cmd/coordinator/ -v && go build ./cmd/coordinator/`
Expected: PASS + build OK.

- [ ] **Step 5: Commit**

```bash
git add cmd/coordinator/main.go cmd/coordinator/main_test.go
git commit -m "feat(p2-m2): run federation listener alongside coordinator (P2-M2)"
```

---

## Task 11: Donor `RegistrationStore` (atomic JSON)

**Files:**
- Create: `internal/node/state/registration.go`
- Test: `internal/node/state/registration_test.go`

- [ ] **Step 1: Write the failing test**

```go
package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileRegistrationRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewFileRegistrationStore(dir)
	ctx := context.Background()

	if _, ok, err := s.LoadRegistration(ctx); err != nil || ok {
		t.Fatalf("empty load: ok=%v err=%v", ok, err)
	}

	reg := Registration{NodeID: "n1", Fingerprint: "sha256:fp", SelectedProtocol: "fed/v1", RegisteredAt: time.Now().UTC().Truncate(time.Second)}
	if err := s.SaveRegistration(ctx, reg); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LoadRegistration(ctx)
	if err != nil || !ok {
		t.Fatalf("load after save: ok=%v err=%v", ok, err)
	}
	if got.NodeID != "n1" || got.Fingerprint != "sha256:fp" {
		t.Fatalf("got %+v", got)
	}

	// File is 0600 and no leftover temp files.
	info, err := os.Stat(filepath.Join(dir, "state", "registration.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "state"))
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/node/state/ -run TestFileRegistration -v`
Expected: FAIL — undefined symbols.

- [ ] **Step 3: Create `internal/node/state/registration.go`**

```go
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Registration is the donor's durable proof of a completed registration. It is
// the minimal state the agent needs to resume without re-registering.
type Registration struct {
	NodeID           string    `json:"node_id"`
	Fingerprint      string    `json:"fingerprint"`
	SelectedProtocol string    `json:"selected_protocol"`
	RegisteredAt     time.Time `json:"registered_at"`
}

// RegistrationStore persists the donor's registration. It is intentionally
// separate from the cursor/jti Store seam (M3/M4): different lifecycle, different
// durability needs.
type RegistrationStore interface {
	LoadRegistration(ctx context.Context) (Registration, bool, error)
	SaveRegistration(ctx context.Context, reg Registration) error
}

// FileRegistrationStore writes <storageDir>/state/registration.json atomically.
type FileRegistrationStore struct{ dir string }

// NewFileRegistrationStore roots the store under storageDir/state.
func NewFileRegistrationStore(storageDir string) *FileRegistrationStore {
	return &FileRegistrationStore{dir: filepath.Join(storageDir, "state")}
}

func (f *FileRegistrationStore) path() string { return filepath.Join(f.dir, "registration.json") }

// LoadRegistration returns the stored registration, ok=false when none exists.
func (f *FileRegistrationStore) LoadRegistration(_ context.Context) (Registration, bool, error) {
	data, err := os.ReadFile(f.path())
	if errors.Is(err, os.ErrNotExist) {
		return Registration{}, false, nil
	}
	if err != nil {
		return Registration{}, false, err
	}
	var reg Registration
	if err := json.Unmarshal(data, &reg); err != nil {
		return Registration{}, false, fmt.Errorf("state: corrupt registration.json: %w", err)
	}
	return reg, true, nil
}

// SaveRegistration writes atomically: temp file → fsync → rename → dir fsync.
func (f *FileRegistrationStore) SaveRegistration(_ context.Context, reg Registration) error {
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(f.dir, "registration-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, f.path()); err != nil {
		return err
	}
	return fsyncDir(f.dir)
}

// fsyncDir flushes a directory entry so the rename survives a crash.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		// Some filesystems reject directory fsync; tolerate ENOTSUP/EINVAL.
		return nil
	}
	return nil
}

var _ RegistrationStore = (*FileRegistrationStore)(nil)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/node/state/ -v`
Expected: PASS (existing `store_test.go` still green).

- [ ] **Step 5: Commit**

```bash
git add internal/node/state/registration.go internal/node/state/registration_test.go
git commit -m "feat(p2-m2): donor atomic-JSON RegistrationStore (P2-M2)"
```

---

## Task 12: Donor agent — real register→heartbeat loop

**Files:**
- Modify: `internal/node/agent/agent.go`
- Test: `internal/node/agent/agent_test.go` (replace the M1 no-op test)

- [ ] **Step 1: Write the failing test**

```go
package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	nodeconfig "github.com/nova-archive/nova/internal/node/config"
	"github.com/nova-archive/nova/internal/node/state"
	"github.com/nova-archive/nova/internal/federation/wire"
)

type fakeClient struct {
	registers atomic.Int32
	heartbeats atomic.Int32
	regResp   wire.RegisterResponse
	hbErr     error
}

func (f *fakeClient) Register(context.Context, wire.RegisterRequest) (wire.RegisterResponse, error) {
	f.registers.Add(1)
	return f.regResp, nil
}
func (f *fakeClient) Heartbeat(context.Context, wire.HeartbeatRequest) (wire.HeartbeatResponse, error) {
	f.heartbeats.Add(1)
	return wire.HeartbeatResponse{ConfigUpdates: &wire.ConfigUpdates{HeartbeatIntervalSeconds: 1}}, f.hbErr
}

func TestAgentRegistersOnceThenHeartbeats(t *testing.T) {
	cfg := &nodeconfig.Config{BandwidthBudgetBytesPerDay: 1}
	store := state.NewFileRegistrationStore(t.TempDir())
	fc := &fakeClient{regResp: wire.RegisterResponse{NodeID: "n1", SelectedProtocol: wire.ProtocolV1}}
	a := New(cfg, store, fc, 20*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	_ = a.Run(ctx)

	if got := fc.registers.Load(); got != 1 {
		t.Fatalf("registers = %d, want 1", got)
	}
	if got := fc.heartbeats.Load(); got < 2 {
		t.Fatalf("heartbeats = %d, want >= 2", got)
	}
	// Registration persisted.
	if reg, ok, _ := store.LoadRegistration(ctx); !ok || reg.NodeID != "n1" {
		t.Fatalf("registration not persisted: %+v ok=%v", reg, ok)
	}
}

func TestAgentSkipsRegisterWhenAlreadyRegistered(t *testing.T) {
	store := state.NewFileRegistrationStore(t.TempDir())
	_ = store.SaveRegistration(context.Background(), state.Registration{NodeID: "n9"})
	fc := &fakeClient{}
	a := New(&nodeconfig.Config{BandwidthBudgetBytesPerDay: 1}, store, fc, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_ = a.Run(ctx)
	if fc.registers.Load() != 0 {
		t.Fatalf("should not re-register, got %d", fc.registers.Load())
	}
	_ = errors.New
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/node/agent/ -v`
Expected: FAIL — `New` signature changed; `Client` undefined.

- [ ] **Step 3: Rewrite `internal/node/agent/agent.go`**

```go
// Package agent runs the donor's register→heartbeat loop over the federation
// mTLS client. M2: register once (persisted), then heartbeat on the negotiated
// interval, honoring config_updates and backing off on transport errors. The
// pins/changes poll is M3.
package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/nova-archive/nova/internal/federation/wire"
	nodeconfig "github.com/nova-archive/nova/internal/node/config"
	"github.com/nova-archive/nova/internal/node/state"
)

// Client is the donor's view of the coordinator federation API. The real impl
// is agent.HTTPClient (mTLS); tests inject a fake.
type Client interface {
	Register(ctx context.Context, req wire.RegisterRequest) (wire.RegisterResponse, error)
	Heartbeat(ctx context.Context, req wire.HeartbeatRequest) (wire.HeartbeatResponse, error)
}

// Agent owns the donor control loop.
type Agent struct {
	cfg      *nodeconfig.Config
	store    state.RegistrationStore
	client   Client
	interval time.Duration
}

// New constructs an Agent. interval is the initial heartbeat cadence (overridden
// by config_updates).
func New(cfg *nodeconfig.Config, store state.RegistrationStore, client Client, interval time.Duration) *Agent {
	return &Agent{cfg: cfg, store: store, client: client, interval: interval}
}

func (a *Agent) registerReq() wire.RegisterRequest {
	return wire.RegisterRequest{
		SupportedProtocols:         []string{wire.ProtocolV1},
		Capabilities:               []string{}, // M2: advertise nothing beyond base protocol
		BandwidthBudgetBytesPerDay: a.cfg.BandwidthBudgetBytesPerDay,
	}
}

// Run blocks until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if _, ok, err := a.store.LoadRegistration(ctx); err != nil {
		return err
	} else if !ok {
		resp, err := a.client.Register(ctx, a.registerReq())
		if err != nil {
			return err
		}
		if err := a.store.SaveRegistration(ctx, state.Registration{
			NodeID:           resp.NodeID,
			SelectedProtocol: resp.SelectedProtocol,
			RegisteredAt:     time.Now().UTC(),
		}); err != nil {
			return err
		}
		slog.Info("nova-node registered", "node_id", resp.NodeID, "protocol", resp.SelectedProtocol)
	}

	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			resp, err := a.client.Heartbeat(ctx, wire.HeartbeatRequest{})
			if err != nil {
				slog.Warn("nova-node heartbeat failed", "err", err)
				continue
			}
			if resp.ConfigUpdates != nil && resp.ConfigUpdates.HeartbeatIntervalSeconds > 0 {
				next := time.Duration(resp.ConfigUpdates.HeartbeatIntervalSeconds) * time.Second
				if next != a.interval {
					a.interval = next
					ticker.Reset(next)
				}
			}
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/node/agent/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/node/agent/agent.go internal/node/agent/agent_test.go
git commit -m "feat(p2-m2): donor register->heartbeat agent loop (P2-M2)"
```

---

## Task 13: Donor mTLS HTTP client + `cmd/node` wiring

**Files:**
- Create: `internal/node/agent/client.go`
- Modify: `cmd/node/main.go`
- Test: `cmd/node/main_test.go` (validate exit code unchanged; agent constructed)

- [ ] **Step 1: Write the failing test (client unit)**

Create `internal/node/agent/client_test.go`:

```go
package agent

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nova-archive/nova/internal/federation/wire"
)

func TestHTTPClientRegisterPostsToFedEndpoint(t *testing.T) {
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"selected_protocol":"fed/v1","node_id":"n1"}`))
	}))
	defer ts.Close()

	c := NewHTTPClient(ts.URL, &tls.Config{}) // plain TLS config; httptest is plain HTTP, override transport
	c.hc = ts.Client()                        // use the test server's client (see note)

	resp, err := c.Register(context.Background(), wire.RegisterRequest{SupportedProtocols: []string{wire.ProtocolV1}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.NodeID != "n1" || gotPath != "/fed/v1/register" {
		t.Fatalf("resp=%+v path=%s", resp, gotPath)
	}
}
```

> Note: this unit test swaps in `ts.Client()` to exercise the request/JSON plumbing over plain HTTP; the real mTLS path is covered end-to-end in Task 17. Make the `hc` field package-visible (lowercase, same package) so the test can override it.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/node/agent/ -run TestHTTPClient -v`
Expected: FAIL — `NewHTTPClient` undefined.

- [ ] **Step 3: Create `internal/node/agent/client.go`**

```go
package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/nova-archive/nova/internal/federation/wire"
)

// HTTPClient is the donor's mTLS federation client.
type HTTPClient struct {
	base string
	hc   *http.Client
}

// NewHTTPClient builds an mTLS client targeting coordinatorURL with tlsCfg.
func NewHTTPClient(coordinatorURL string, tlsCfg *tls.Config) *HTTPClient {
	return &HTTPClient{
		base: coordinatorURL,
		hc: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}
}

func (c *HTTPClient) post(ctx context.Context, path string, in, out any, okStatus int) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
	if resp.StatusCode != okStatus && !(path == "/fed/v1/register" && resp.StatusCode == http.StatusOK) {
		var e wire.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s: status %d (%s)", path, resp.StatusCode, e.Code)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *HTTPClient) Register(ctx context.Context, req wire.RegisterRequest) (wire.RegisterResponse, error) {
	var out wire.RegisterResponse
	err := c.post(ctx, "/fed/v1/register", req, &out, http.StatusCreated)
	return out, err
}

func (c *HTTPClient) Heartbeat(ctx context.Context, req wire.HeartbeatRequest) (wire.HeartbeatResponse, error) {
	var out wire.HeartbeatResponse
	err := c.post(ctx, "/fed/v1/heartbeat", req, &out, http.StatusOK)
	return out, err
}

var _ Client = (*HTTPClient)(nil)
```

- [ ] **Step 4: Wire `cmd/node/main.go`**

In `serve(...)`, replace the M1 no-op agent construction with the real client + agent. Read the federation PEM at the configured paths, build the client TLS config via `transport.ClientTLSConfig`, and start the agent:

```go
// imports to add:
//   "github.com/nova-archive/nova/internal/federation/transport"
//   "github.com/nova-archive/nova/internal/node/state"   (already imported)

// replace:
//   ag := agent.New(cfg, state.NewMemStore())
//   go func() { _ = ag.Run(ctx) }()
// with:
	caPEM, err := os.ReadFile(cfg.FederationCAPath)   // see note below on FederationCAPath
	if err != nil {
		return fmt.Errorf("read federation ca: %w", err)
	}
	certPEM, err := os.ReadFile(cfg.FederationCertPath)
	if err != nil {
		return fmt.Errorf("read federation cert: %w", err)
	}
	keyPEM, err := os.ReadFile(cfg.FederationKeyPath)
	if err != nil {
		return fmt.Errorf("read federation key: %w", err)
	}
	tlsCfg, err := transport.ClientTLSConfig(caPEM, certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("federation client tls: %w", err)
	}
	client := agent.NewHTTPClient(cfg.CoordinatorURL, tlsCfg)
	regStore := state.NewFileRegistrationStore(cfg.StorageDir)
	ag := agent.New(cfg, regStore, client, time.Duration(cfg.HeartbeatIntervalSeconds())*time.Second)
	go func() {
		if e := ag.Run(ctx); e != nil && ctx.Err() == nil {
			slog.Error("nova-node agent stopped", "err", e)
		}
	}()
```

This requires a `FederationCAPath` field on the node config. Add it in Step 5 below (Task 13b) — `internal/node/config` currently has cert/key but no CA path. Also add a `HeartbeatIntervalSeconds()` helper to node config returning a sane default (300) for M2 (the donor's initial cadence before config_updates).

- [ ] **Step 4b: Add `federation_ca_path` + interval default to node config**

In `internal/node/config/config.go`, add to `Config`:

```go
	FederationCAPath string `yaml:"federation_ca_path"`
```

…include it in the `files` validation map in `validate()`:

```go
		"federation_ca_path": c.FederationCAPath,
```

…and add:

```go
// HeartbeatIntervalSeconds is the donor's initial heartbeat cadence before the
// coordinator overrides it via config_updates. M2 default 300.
func (c *Config) HeartbeatIntervalSeconds() int { return 300 }
```

Update `internal/node/config/config_test.go`'s minimal-valid fixture to include a readable `federation_ca_path` file so existing tests stay green.

- [ ] **Step 5: Run tests + build**

Run: `go test ./internal/node/... ./cmd/node/ -v && go build ./cmd/node/`
Expected: PASS + build OK. (`cmd/node/main_test.go` `--validate` cases pass with the added `federation_ca_path` fixture.)

- [ ] **Step 6: Commit**

```bash
git add internal/node/agent/client.go internal/node/agent/client_test.go cmd/node/main.go internal/node/config/config.go internal/node/config/config_test.go
git commit -m "feat(p2-m2): donor mTLS client + wire real agent into cmd/node (P2-M2)"
```

---

## Task 14: `novactl node` CA / issue / nebula-template (local file ops)

**Files:**
- Create: `cmd/novactl/node.go`
- Create: `cmd/novactl/templates/{nebula-config.yml.tmpl,node.yaml.tmpl,donor-compose.yaml.tmpl,operator-README.txt.tmpl}`
- Modify: `cmd/novactl/main.go` (route `node` subcommand + usage)
- Test: `cmd/novactl/node_test.go`

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestNodeCAInitAndIssue(t *testing.T) {
	dir := t.TempDir()
	if err := cmdNode([]string{"ca-init", "--dir", dir}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"federation-ca.crt", "federation-ca.key", "coordinator-federation.crt", "coordinator-federation.key"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
	}
	out := filepath.Join(dir, "donor-a")
	if err := cmdNode([]string{"issue", "--dir", dir, "--name", "donor-a", "--out", out}); err != nil {
		t.Fatal(err)
	}
	certPEM, err := os.ReadFile(filepath.Join(out, "federation.crt"))
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(certPEM)
	c, _ := x509.ParseCertificate(blk.Bytes)
	if len(c.URIs) == 0 || c.URIs[0].Scheme != "nova" {
		t.Fatalf("client cert missing nova URI SAN: %+v", c.URIs)
	}
}

func TestNodeNebulaTemplate(t *testing.T) {
	dir := t.TempDir()
	if err := cmdNode([]string{"nebula-template", "--name", "donor-a", "--nebula-ip", "10.42.0.23/24", "--out", dir}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"nebula-config.yml", "node.yaml", "compose.yaml", "README.operator.txt"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
		if len(b) == 0 {
			t.Fatalf("%s empty", f)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/novactl/ -run TestNode -v`
Expected: FAIL — `cmdNode` undefined.

- [ ] **Step 3: Create the templates**

`cmd/novactl/templates/nebula-config.yml.tmpl` (abbreviated but complete + valid YAML; placeholders explicit):

```yaml
# Nova donor Nebula sidecar config for {{.Name}} (overlay IP {{.NebulaIP}}).
# Provision the Nebula cert/key with the upstream nebula-cert tool (NOT novactl):
#   nebula-cert sign -name "{{.Name}}" -ip "{{.NebulaIP}}" -ca-crt nebula-ca.crt -ca-key nebula-ca.key
pki:
  ca: /etc/nebula/nebula-ca.crt
  cert: /etc/nebula/nebula.crt
  key: /etc/nebula/nebula.key
static_host_map:
  "{{.LighthouseOverlayIP}}": ["{{.LighthousePublicIP}}:4242"]
lighthouse:
  am_lighthouse: false
  hosts:
    - "{{.LighthouseOverlayIP}}"
listen:
  host: 0.0.0.0
  port: 4242
tun:
  dev: nebula1
firewall:
  outbound:
    - port: any
      proto: any
      host: any
  inbound:
    - port: any
      proto: any
      host: any
```

`cmd/novactl/templates/node.yaml.tmpl`:

```yaml
# nova-node config for {{.Name}}. Federation cert/key issued by `novactl node issue`;
# Nebula cert/key issued by `nebula-cert`. Two distinct trust roots — do not mix.
coordinator_url: "https://{{.CoordinatorOverlayIP}}:9443"
federation_ca_path:   /etc/nova/federation/federation-ca.crt
federation_cert_path: /etc/nova/federation/federation.crt
federation_key_path:  /etc/nova/federation/federation.key
nebula_cert_path: /etc/nebula/nebula.crt
nebula_key_path:  /etc/nebula/nebula.key
swarm_key_path:   /etc/nova/swarm.key
storage_dir: /var/lib/nova-node
bandwidth_budget_bytes_per_day: 53687091200
health_listen_addr: "127.0.0.1:9100"
failure_domain:
  provider: ""
  asn: ""
  region: ""
```

`cmd/novactl/templates/donor-compose.yaml.tmpl` and `operator-README.txt.tmpl`: include the Nebula sidecar (NET_ADMIN, /dev/net/tun) + nova-node (no NET_ADMIN, shares netns, no published ports, read-only rootfs, cap_drop ALL), and the README lists the exact `nebula-cert ca` / `nebula-cert sign` commands and the explicit two-trust-root file layout (`nebula-ca.crt`/`nebula.crt`/`nebula.key` vs `federation-ca.crt`/`federation.crt`/`federation.key`). (Mirror `deploy/donor/compose.yaml` from M1.)

- [ ] **Step 4: Create `cmd/novactl/node.go`**

```go
package main

import (
	"embed"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"text/template"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/federation/ca"
	"github.com/nova-archive/nova/internal/federation/transport"
)

//go:embed templates/*.tmpl
var nodeTemplates embed.FS

// cmdNode dispatches `novactl node <subcommand>`.
func cmdNode(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: novactl node <ca-init|issue|revoke|rotate-cert|list|nebula-template>")
	}
	switch args[0] {
	case "ca-init":
		return cmdNodeCAInit(args[1:])
	case "issue":
		return cmdNodeIssue(args[1:])
	case "nebula-template":
		return cmdNodeNebulaTemplate(args[1:])
	case "revoke":
		return cmdNodeRevoke(args[1:]) // Task 15
	case "rotate-cert":
		return cmdNodeRotateCert(args[1:]) // Task 15
	case "list":
		return cmdNodeList(args[1:]) // Task 15
	default:
		return fmt.Errorf("novactl node: unknown subcommand %q", args[0])
	}
}

func writeFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, perm)
}

func cmdNodeCAInit(args []string) error {
	fs := flag.NewFlagSet("node ca-init", flag.ContinueOnError)
	dir := fs.String("dir", ".", "output directory for CA + coordinator server cert")
	coordIP := fs.String("coordinator-ip", "127.0.0.1", "coordinator Nebula overlay IP for the server cert SAN")
	coordDNS := fs.String("coordinator-dns", "localhost", "coordinator DNS SAN")
	if err := fs.Parse(args); err != nil {
		return err
	}
	caCertPEM, caKeyPEM, err := ca.GenerateCA()
	if err != nil {
		return err
	}
	srvPEM, srvKeyPEM, err := ca.IssueServerCert(caCertPEM, caKeyPEM, ca.ServerCertOptions{
		DNSNames:    []string{*coordDNS},
		IPAddresses: []string{*coordIP},
	})
	if err != nil {
		return err
	}
	for name, data := range map[string][]byte{
		"federation-ca.crt":            caCertPEM,
		"federation-ca.key":            caKeyPEM,
		"coordinator-federation.crt":   srvPEM,
		"coordinator-federation.key":   srvKeyPEM,
		"federation-ca.manifest.json":  caManifest(caCertPEM),
	} {
		perm := os.FileMode(0o644)
		if filepath.Ext(name) == ".key" {
			perm = 0o600
		}
		if err := writeFile(filepath.Join(*dir, name), data, perm); err != nil {
			return err
		}
	}
	fmt.Printf("federation CA + coordinator server cert written to %s\n", *dir)
	return nil
}

func caManifest(caCertPEM []byte) []byte {
	// minimal manifest: the CA fingerprint operators can pin.
	blk := []byte(fmt.Sprintf("{\n  \"ca_fingerprint\": %q\n}\n", caFingerprint(caCertPEM)))
	return blk
}

func cmdNodeIssue(args []string) error {
	fs := flag.NewFlagSet("node issue", flag.ContinueOnError)
	dir := fs.String("dir", ".", "directory holding federation-ca.crt + federation-ca.key")
	name := fs.String("name", "", "donor display name (required)")
	out := fs.String("out", "", "output dir for the donor bundle (default ./<name>)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("node issue: --name is required")
	}
	outDir := *out
	if outDir == "" {
		outDir = filepath.Join(".", *name)
	}
	caCertPEM, err := os.ReadFile(filepath.Join(*dir, "federation-ca.crt"))
	if err != nil {
		return err
	}
	caKeyPEM, err := os.ReadFile(filepath.Join(*dir, "federation-ca.key"))
	if err != nil {
		return err
	}
	nodeID := uuid.New()
	certPEM, keyPEM, err := ca.IssueClientCert(caCertPEM, caKeyPEM, nodeID, *name)
	if err != nil {
		return err
	}
	for name, data := range map[string][]byte{
		"federation.crt":    certPEM,
		"federation.key":    keyPEM,
		"federation-ca.crt": caCertPEM,
		"node-manifest.json": []byte(fmt.Sprintf("{\n  \"node_id\": %q,\n  \"display_name\": %q,\n  \"fingerprint\": %q\n}\n",
			nodeID.String(), *name, leafFingerprint(certPEM))),
	} {
		perm := os.FileMode(0o644)
		if filepath.Ext(name) == ".key" {
			perm = 0o600
		}
		if err := writeFile(filepath.Join(outDir, name), data, perm); err != nil {
			return err
		}
	}
	fmt.Printf("issued donor cert for %s\n  node_id:     %s\n  fingerprint: %s\n  bundle:      %s\n",
		*name, nodeID.String(), leafFingerprint(certPEM), outDir)
	return nil
}

func cmdNodeNebulaTemplate(args []string) error {
	fs := flag.NewFlagSet("node nebula-template", flag.ContinueOnError)
	name := fs.String("name", "donor", "donor name")
	nebulaIP := fs.String("nebula-ip", "10.42.0.10/24", "donor Nebula overlay IP/CIDR")
	out := fs.String("out", ".", "output dir")
	lhOverlay := fs.String("lighthouse-overlay-ip", "10.42.0.1", "lighthouse overlay IP")
	lhPublic := fs.String("lighthouse-public-ip", "REPLACE_ME", "lighthouse public IP")
	coordOverlay := fs.String("coordinator-overlay-ip", "10.42.0.1", "coordinator overlay IP")
	if err := fs.Parse(args); err != nil {
		return err
	}
	data := map[string]string{
		"Name": *name, "NebulaIP": *nebulaIP,
		"LighthouseOverlayIP": *lhOverlay, "LighthousePublicIP": *lhPublic,
		"CoordinatorOverlayIP": *coordOverlay,
	}
	files := map[string]string{
		"nebula-config.yml":   "templates/nebula-config.yml.tmpl",
		"node.yaml":           "templates/node.yaml.tmpl",
		"compose.yaml":        "templates/donor-compose.yaml.tmpl",
		"README.operator.txt": "templates/operator-README.txt.tmpl",
	}
	for outName, tmplPath := range files {
		tmpl, err := template.ParseFS(nodeTemplates, tmplPath)
		if err != nil {
			return err
		}
		f, err := os.Create(filepath.Join(*out, outName))
		if err != nil {
			return err
		}
		if err := tmpl.Execute(f, data); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
	fmt.Printf("Nebula + donor templates written to %s (run nebula-cert yourself — see README.operator.txt)\n", *out)
	return nil
}

// caFingerprint / leafFingerprint compute "sha256:<hex of DER>" from PEM.
func caFingerprint(pemBytes []byte) string  { return leafFingerprint(pemBytes) }
func leafFingerprint(pemBytes []byte) string {
	c, err := parseFirstCert(pemBytes)
	if err != nil {
		return ""
	}
	return transport.FingerprintDER(c)
}
```

Add `parseFirstCert` (decode the first PEM CERTIFICATE block) in this file:

```go
import (
	"crypto/x509"
	"encoding/pem"
)

func parseFirstCert(pemBytes []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		return nil, fmt.Errorf("no PEM certificate")
	}
	return x509.ParseCertificate(blk.Bytes)
}
```

- [ ] **Step 5: Route `node` in `cmd/novactl/main.go`**

Add a `case "node": return cmdNode(args[1:])` to the top-level subcommand switch, and a `node` line to the usage block.

- [ ] **Step 6: Run tests + build**

Run: `go test ./cmd/novactl/ -run TestNode -v && go build ./cmd/novactl/`
Expected: PASS + build OK.

- [ ] **Step 7: Commit**

```bash
git add cmd/novactl/node.go cmd/novactl/templates/ cmd/novactl/node_test.go cmd/novactl/main.go
git commit -m "feat(p2-m2): novactl node ca-init/issue/nebula-template (P2-M2)"
```

---

## Task 15: `novactl node` revoke / rotate-cert / list (DB-direct)

**Files:**
- Modify: `cmd/novactl/node.go` (add `cmdNodeRevoke`, `cmdNodeRotateCert`, `cmdNodeList`)
- Test: `cmd/novactl/node_db_test.go`

- [ ] **Step 1: Write the failing test**

`dbtest.New` spins up an ephemeral Postgres **testcontainer** and returns only a `*pgxpool.Pool` — it never exposes a DSN, so a command that reads `DATABASE_URL` internally cannot reach the test DB. Therefore each registry command is split into a thin flag-parsing/`DATABASE_URL` wrapper (`cmdNodeRevoke`) and a **testable core that takes an injected `*gen.Queries`** (`revokeNode`). Tests drive the core with the `dbtest` pool.

```go
package main

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
)

func seedNode(t *testing.T, ctx context.Context, q *gen.Queries, id uuid.UUID, fp string) {
	t.Helper()
	_, err := q.RegisterNode(ctx, gen.RegisterNodeParams{
		ID:                        pgtype.UUID{Bytes: id, Valid: true},
		NebulaCertFingerprint:     "sha256:n",
		FederationCertFingerprint: fp,
		PolicyFilters:             []byte("{}"),
		AdvertisedCapabilities:    []string{},
		RequiredCapabilities:      []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRevokeNodeCore(t *testing.T) {
	ctx := context.Background()
	q := gen.New(dbtest.New(t, ctx))
	id := uuid.New()
	seedNode(t, ctx, q, id, "sha256:f")
	if err := revokeNode(ctx, q, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		t.Fatal(err)
	}
	got, _ := q.GetNodeByID(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if got.Status != gen.NodeStatusRevoked {
		t.Fatalf("status = %v", got.Status)
	}
}

func TestRotateNodeCertCore(t *testing.T) {
	ctx := context.Background()
	q := gen.New(dbtest.New(t, ctx))
	id := uuid.New()
	seedNode(t, ctx, q, id, "sha256:old")
	if err := rotateNodeCert(ctx, q, pgtype.UUID{Bytes: id, Valid: true}, "sha256:new"); err != nil {
		t.Fatal(err)
	}
	got, _ := q.GetNodeByID(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if got.FederationCertFingerprint != "sha256:new" || !got.CertRotatedAt.Valid {
		t.Fatalf("rotate not applied: fp=%s rotated=%v", got.FederationCertFingerprint, got.CertRotatedAt.Valid)
	}
}
```

> Note: confirm the seed params + `gen.NodeStatusRevoked` + `got.CertRotatedAt` (pgtype.Timestamptz) names against Task 5's sqlc output.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/novactl/ -run TestNodeRevoke -v`
Expected: FAIL — `cmdNodeRevoke` undefined.

- [ ] **Step 3: Add the DB-direct commands to `cmd/novactl/node.go`**

```go
import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db"
	"github.com/nova-archive/nova/internal/db/gen"
)

func withNodeDB(fn func(ctx context.Context, q *gen.Queries) error) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL must be set for node registry commands")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	return fn(ctx, gen.New(pool))
}

func parsePGUUID(s string) (pgtype.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("invalid --id: %w", err)
	}
	return pgtype.UUID{Bytes: id, Valid: true}, nil
}

func cmdNodeRevoke(args []string) error {
	fs := flag.NewFlagSet("node revoke", flag.ContinueOnError)
	idStr := fs.String("id", "", "node id (uuid)")
	noConfirm := fs.Bool("no-confirm", false, "skip confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pgID, err := parsePGUUID(*idStr)
	if err != nil {
		return err
	}
	if !*noConfirm {
		fmt.Printf("Revoke node %s? Its certificate will be refused at the next request. [y/N]: ", *idStr)
		var ans string
		fmt.Scanln(&ans)
		if ans != "y" && ans != "Y" {
			return errors.New("aborted")
		}
	}
	return withNodeDB(func(ctx context.Context, q *gen.Queries) error {
		if err := revokeNode(ctx, q, pgID); err != nil {
			return err
		}
		fmt.Printf("revoked node %s\n", *idStr)
		return nil
	})
}

// revokeNode is the testable core: it flips status to revoked.
func revokeNode(ctx context.Context, q *gen.Queries, pgID pgtype.UUID) error {
	n, err := q.RevokeNode(ctx, pgID)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("node not found or already revoked")
	}
	return nil
}

func cmdNodeRotateCert(args []string) error {
	fs := flag.NewFlagSet("node rotate-cert", flag.ContinueOnError)
	idStr := fs.String("id", "", "node id (uuid)")
	dir := fs.String("dir", ".", "directory holding federation-ca.crt + federation-ca.key")
	name := fs.String("name", "donor", "donor display name for the new cert")
	out := fs.String("out", "", "output dir for the replacement bundle (default ./<id>-rotated)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := uuid.Parse(*idStr)
	if err != nil {
		return fmt.Errorf("invalid --id: %w", err)
	}
	caCertPEM, err := os.ReadFile(filepath.Join(*dir, "federation-ca.crt"))
	if err != nil {
		return err
	}
	caKeyPEM, err := os.ReadFile(filepath.Join(*dir, "federation-ca.key"))
	if err != nil {
		return err
	}
	certPEM, keyPEM, err := ca.IssueClientCert(caCertPEM, caKeyPEM, id, *name) // SAME node_id
	if err != nil {
		return err
	}
	outDir := *out
	if outDir == "" {
		outDir = filepath.Join(".", *idStr+"-rotated")
	}
	for fn, data := range map[string][]byte{"federation.crt": certPEM, "federation.key": keyPEM, "federation-ca.crt": caCertPEM} {
		perm := os.FileMode(0o644)
		if filepath.Ext(fn) == ".key" {
			perm = 0o600
		}
		if err := writeFile(filepath.Join(outDir, fn), data, perm); err != nil {
			return err
		}
	}
	newFP := leafFingerprint(certPEM)
	if err := withNodeDB(func(ctx context.Context, q *gen.Queries) error {
		return rotateNodeCert(ctx, q, pgtype.UUID{Bytes: id, Valid: true}, newFP)
	}); err != nil {
		return err
	}
	fmt.Printf("rotated node %s — new fingerprint %s active (downtime cutover until donor restarts with %s)\n", *idStr, newFP, outDir)
	return nil
}

// rotateNodeCert is the testable core: it swaps the stored fingerprint to newFP.
func rotateNodeCert(ctx context.Context, q *gen.Queries, pgID pgtype.UUID, newFP string) error {
	n, err := q.RotateNodeCert(ctx, gen.RotateNodeCertParams{ID: pgID, FederationCertFingerprint: newFP})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("node not found")
	}
	return nil
}

func cmdNodeList(args []string) error {
	return withNodeDB(func(ctx context.Context, q *gen.Queries) error {
		rows, err := q.ListNodes(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("%-38s %-16s %-12s %-12s %s\n", "NODE_ID", "DISPLAY", "STATUS", "TRUST", "LAST_SEEN")
		for _, r := range rows {
			fmt.Printf("%-38s %-16s %-12s %-12s %v\n", uuidString(r.ID), r.DisplayName.String, r.Status, r.TrustState, r.LastSeenAt.Time)
		}
		return nil
	})
}

// uuidString renders a pgtype.UUID.
func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}
```

> Match `gen.RotateNodeCertParams`, `gen.ListNodesRow` field names, and `gen.NodeStatusRevoked` to Task 5's sqlc output. `r.TrustState` is a `string` (text+CHECK column).

- [ ] **Step 4: Run tests + build**

Run: `go test ./cmd/novactl/ -run TestNode -v && go build ./cmd/novactl/`
Expected: PASS + build OK.

- [ ] **Step 5: Commit**

```bash
git add cmd/novactl/node.go cmd/novactl/node_db_test.go
git commit -m "feat(p2-m2): novactl node revoke/rotate-cert/list (DB-direct) (P2-M2)"
```

---

## Task 16: Donor dependency-boundary allowlist + deploy wiring

**Files:**
- Modify: `scripts/check_node_deps.sh`
- Modify: `deploy/donor/compose.yaml`, `deploy/donor/node.yaml.example`
- Create: `deploy/operator/README.md` (federation listener + Nebula sidecar notes)

- [ ] **Step 1: Add `internal/federation/transport` to the allowlist**

In `scripts/check_node_deps.sh`, add to the `ALLOWED` array (after the `wire` line):

```sh
  "$MOD/internal/federation/transport"
```

- [ ] **Step 2: Verify the boundary is green, then red against a violation**

Run: `./scripts/check_node_deps.sh`
Expected: `OK: cmd/node dependency boundary clean`.

Then temporarily add `import _ "github.com/nova-archive/nova/internal/db"` to `cmd/node/main.go`, run the script, confirm it FAILS listing `internal/db`, then **revert the import**. Re-run: OK again.

- [ ] **Step 3: Update donor deploy + operator notes**

Update `deploy/donor/node.yaml.example` to include `federation_ca_path` (M1 added cert/key but the CA path is new — Task 13b). Update `deploy/donor/compose.yaml` if needed so the federation CA file is mounted. Create `deploy/operator/README.md` documenting: the coordinator `operator.yaml` `federation:` block, that `novactl node ca-init` produces the CA + coordinator server cert, and that the operator runs a Nebula sidecar and binds `federation.listen_addr` to the overlay IP.

- [ ] **Step 4: Run the full build + vet + boundary**

Run: `go build ./... && go vet ./... && ./scripts/check_node_deps.sh`
Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add scripts/check_node_deps.sh deploy/
git commit -m "feat(p2-m2): allowlist federation/transport + donor/operator deploy wiring (P2-M2)"
```

---

## Task 17: End-to-end loopback mTLS integration test + public-mux exclusion

**Files:**
- Create: `internal/federation/coordinator/integration_test.go`
- Create: `internal/api/federation_exclusion_test.go` (public-mux 404)

- [ ] **Step 1: Write the integration test**

```go
package coordinator

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/federation/ca"
	"github.com/nova-archive/nova/internal/node/agent"
	"github.com/nova-archive/nova/internal/node/config"
	"github.com/nova-archive/nova/internal/node/state"
	"github.com/nova-archive/nova/internal/federation/transport"
)

func TestEndToEndRegisterHeartbeatRevokeRotate(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	q := gen.New(pool)
	caPEM, caKeyPEM, _ := ca.GenerateCA()
	srvPEM, srvKeyPEM, _ := ca.IssueServerCert(caPEM, caKeyPEM, ca.ServerCertOptions{DNSNames: []string{"localhost"}, IPAddresses: []string{"127.0.0.1"}})

	s := New(q, Config{ListenAddr: "127.0.0.1:0", Timers: wireTimers(), TLS: TLSMaterial{CAPEM: caPEM, CertPEM: srvPEM, KeyPEM: srvKeyPEM}})
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.Run(runCtx)

	// donor cert + client
	id := uuid.New()
	cliPEM, cliKeyPEM, _ := ca.IssueClientCert(caPEM, caKeyPEM, id, "donor")
	tlsCfg, _ := transport.ClientTLSConfig(caPEM, cliPEM, cliKeyPEM)
	tlsCfg.ServerName = "localhost"
	client := agent.NewHTTPClient("https://"+s.Addr(), tlsCfg)

	// agent registers + heartbeats
	ag := agent.New(&config.Config{BandwidthBudgetBytesPerDay: 1}, state.NewFileRegistrationStore(t.TempDir()), client, 20*time.Millisecond)
	agCtx, agCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer agCancel()
	_ = ag.Run(agCtx)

	node, err := q.GetNodeByID(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		t.Fatalf("node not registered: %v", err)
	}
	if string(node.TrustState) != "probationary" || node.Status != gen.NodeStatusActive {
		t.Fatalf("trust=%v status=%v", node.TrustState, node.Status)
	}
	if !node.LastSeenAt.Valid {
		t.Fatal("last_seen_at not set (no heartbeat recorded)")
	}

	// revoke → next heartbeat 403
	if _, err := q.RevokeNode(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Heartbeat(ctx, hbReq()); err == nil {
		t.Fatal("expected heartbeat to fail after revoke")
	}
}
```

Add small helpers `wireTimers()` and `hbReq()` in the test file. `node.TrustState` is a string; compare accordingly.

- [ ] **Step 2: Write the public-mux exclusion test**

In `internal/api/federation_exclusion_test.go`, build the public coordinator handler (mirror an existing `internal/api/server_test.go` setup) and assert:

```go
func TestPublicMuxDoesNotServeFederation(t *testing.T) {
	h := newTestPublicHandler(t) // reuse the existing test harness pattern in internal/api
	req := httptest.NewRequest(http.MethodPost, "/fed/v1/register", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("/fed/v1/register on public mux = %d, want 404", w.Code)
	}
}
```

> Use whatever handler-construction helper `internal/api/server_test.go` already exposes; if none is exported, construct via `api.NewServer(...)` as that test does.

- [ ] **Step 3: Run the tests**

Run: `go test ./internal/federation/... ./internal/api/ -run 'EndToEnd|PublicMux' -v` (with a test Postgres available)
Expected: PASS — registration persists `probationary`/`active`, heartbeat sets `last_seen_at`, revoke blocks the next heartbeat, and `/fed/v1/*` is 404 on the public mux.

- [ ] **Step 4: Full suite + boundary + frozen + vet**

Run:
```bash
go build ./... && go vet ./...
go test ./... 
./scripts/check_node_deps.sh
make migrations-frozen
```
Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add internal/federation/coordinator/integration_test.go internal/api/federation_exclusion_test.go
git commit -m "test(p2-m2): e2e loopback mTLS register/heartbeat/revoke + public-mux 404 (P2-M2)"
```

---

## Self-review checklist (completed during authoring)

- **Spec coverage:** every design section maps to a task — wire types (T1), transport identity+TLS (T2–T3), CA (T4), migration/queries (T5), register handler + negotiation + non-2xx refusal (T6), heartbeat + revocation/identity enforcement + empty repair-key/epoch-0 (T7), mTLS listener (T8), operator federation config + interface guard (T9), dual-listener bind-before-serve (T10), donor atomic-JSON state (T11), donor agent loop (T12), donor mTLS client + cmd/node wiring + node CA path (T13), novactl CA/issue/templates (T14), novactl revoke/rotate/list DB-direct (T15), boundary allowlist + deploy (T16), e2e + public-mux exclusion (T17).
- **DER fingerprint, URI-SAN node_id, honest empty capability set, downtime-cutover rotation, omit `last_heartbeat_error`** — all present.
- **Type consistency:** `wire.ConfigUpdates`, `transport.Identity`/`FingerprintDER`/`IdentityFromCert`/`ClientTLSConfig`/`ServerTLSConfig`/`NewTLSListener`, `ca.GenerateCA`/`IssueServerCert`/`IssueClientCert`/`ServerCertOptions`, `coordinator.New`/`Config`/`TLSMaterial`/`Listen`/`Run`/`Addr`, `state.RegistrationStore`/`Registration`/`NewFileRegistrationStore`, `agent.Client`/`New`/`NewHTTPClient` are used consistently across tasks.
- **sqlc identifiers:** generated `gen.RegisterNodeParams` / `gen.UpdateNodeHeartbeatParams` / `gen.RotateNodeCertParams` / `gen.ListNodesRow` / `gen.NodeStatusRevoked` / `gen.NodeStatusActive` field + constant names are produced in T5; T6/T7/T15/T17 must match the actual generated identifiers (flagged inline).
- **Boundary:** the only donor-graph additions are `internal/federation/transport` (allowlisted in T16) + already-allowlisted `wire`; `ca`/`coordinator`/`google/uuid` stay operator-side.

---

## Execution

Per the milestone workflow (subagent-driven, feature branch per milestone — already on `p2-m2-identity-registration`), execute task-by-task with a fresh subagent per task and review between tasks (superpowers:subagent-driven-development). Tasks 6/7/15/17 require a test Postgres (`dbtest`); run them where one is available.
