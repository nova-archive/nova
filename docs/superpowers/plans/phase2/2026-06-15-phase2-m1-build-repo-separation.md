# P2-M1 Build / Repo Separation — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `nova-node` — a separately built, minimal, signed donor artifact whose dependency graph *provably* excludes operator-only code — that loads + validates a path-based `node.yaml`, serves a loopback health endpoint, and does nothing else. **No live federation.**

**Architecture:** Extract the generic secret resolver into a stdlib leaf `internal/secret` (shared, coordinator re-points). Add donor-only `internal/node/{config,agent,state,bandwidth,transfer,audit}` (only `bandwidth` has real logic; the rest are interfaces + stubs) and the shared pure-types `internal/federation/wire` (messages, capability negotiation, a canonical Ed25519 repair-token claim + `Verify` — no mint, no replay). A deny-by-default `go list -deps` boundary gate, a split `node.Dockerfile`, a `deploy/donor/` tree, and a CI SBOM + keyless-signing pipeline complete the separation. No schema, no migration, no operator behavior change beyond the mechanical `internal/secret` re-point.

**Tech Stack:** Go (`go 1.26.2` per `go.mod`), stdlib `crypto/ed25519` + `encoding/base64` + `net/http`, `gopkg.in/yaml.v3`, `testify/require`; Docker (distroless static, CGO-off), syft + cosign keyless (CI), GitHub Actions.

**Design spec:** `docs/superpowers/specs/phase2/2026-06-15-phase2-m1-build-repo-separation-design.md`

**Commit convention:** every commit uses `git commit -s` and ends with the `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>` trailer (repo convention). Work happens on branch `p2-m1-build-repo-separation`; local fast-forward merge + annotated tag at the end, no remote push.

---

## File structure

**Created (donor + shared):**
- `internal/secret/secret.go` — `ResolveSecret` + `Source` (+`Source*`), stdlib leaf. **Moved** from `internal/config/secrets.go`.
- `internal/secret/secret_test.go` — moved from `internal/config/secrets_test.go`.
- `internal/federation/wire/messages.go` — register/heartbeat/changes/snapshot/ack/fail structs + capability ids + error codes.
- `internal/federation/wire/capability.go` — `NegotiateCapabilities`.
- `internal/federation/wire/token.go` — `Claims`, `SignToken`, `Verify` (canonical format).
- `internal/federation/wire/{capability,token}_test.go` — tables.
- `internal/node/config/config.go` — donor `node.yaml` schema + shallow validation.
- `internal/node/config/config_test.go` — validation table.
- `internal/node/bandwidth/bucket.go` — authoritative daily token-bucket (real arithmetic).
- `internal/node/bandwidth/bucket_test.go` — arithmetic table.
- `internal/node/state/store.go` — cursor/cert/replay `Store` interface + in-memory stub.
- `internal/node/agent/agent.go` — no-op register→heartbeat→sync loop skeleton.
- `internal/node/transfer/transfer.go` — `Verifier` interface + not-implemented stub.
- `internal/node/audit/responder.go` — `Responder` interface + not-implemented stub.
- `cmd/node/main.go` — `--config` / `--validate` / `--healthcheck`.
- `cmd/node/main_test.go` — validate/healthcheck exit-code table.
- `scripts/check_node_deps.sh` — deny-by-default boundary gate.
- `scripts/check_node_image.sh` — forbidden-inventory scan over the exported image rootfs.
- `docker/node.Dockerfile` — distroless-static, CGO-off donor image.
- `deploy/donor/node.yaml.example` — annotated donor config.
- `deploy/donor/compose.yaml` — Nebula sidecar + nova-node, no ports, hardening floors.
- `docs/quickstart/donor.md` — release-trust note stub (full walkthrough is P2-M7).

**Modified:**
- `cmd/coordinator/main.go:416` + `internal/envelope/keystore.go:16,101` — re-point `config.ResolveSecret` → `secret.ResolveSecret`.
- `docker/Dockerfile` → **renamed** `docker/coordinator.Dockerfile` (contents unchanged).
- `docker/docker-compose.yml:77` — `dockerfile: docker/coordinator.Dockerfile`.
- `Makefile` — `node-build`, `node-validate`, `node-deps-check`, `node-image`, `node-sbom`, `node-image-inventory` targets; `docker-build` Dockerfile path.
- `.github/workflows/ci.yml` — `donor-deps-boundary`, `donor-build`, `donor-sbom-sign` jobs.

**Deleted:** `internal/config/secrets.go`, `internal/config/secrets_test.go` (moved).

**No migration** — Phase-2 schema lands in P2-M3; the `migrations-frozen` gate stays green. Run `go test` with `testify`; build with `CGO_ENABLED=0` for the donor.

---

## Task 1: Extract `internal/secret` leaf (behavior-preserving refactor)

**Files:**
- Create: `internal/secret/secret.go`, `internal/secret/secret_test.go`
- Delete: `internal/config/secrets.go`, `internal/config/secrets_test.go`
- Modify: `cmd/coordinator/main.go:416`, `internal/envelope/keystore.go:16,101`

- [ ] **Step 1: Create `internal/secret/secret.go`** (the existing resolver, repackaged; `SecretSource` → `Source` to avoid stutter)

```go
// Package secret implements Nova's secret-loading precedence. It is a
// stdlib-only leaf shared by the coordinator and the donor (nova-node); it
// imports no other Nova package so the donor dependency boundary stays clean.
package secret

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Source identifies which precedence step satisfied a ResolveSecret call. The
// string form is stable and intended for startup log lines.
type Source string

const (
	SourceEnv     Source = "env"      // inline env value
	SourceFileEnv Source = "file_env" // path from the *_FILE env var
	SourceMount   Source = "mount"    // defaultMountPath
)

// ResolveSecret applies the precedence: (1) env var envKey; (2) file at the
// path in envFileKey; (3) file at defaultMountPath. Returns the trimmed secret
// value plus the resolving Source, or an error if none resolves.
func ResolveSecret(envKey, envFileKey, defaultMountPath string) (string, Source, error) {
	if v := os.Getenv(envKey); v != "" {
		return strings.TrimSpace(v), SourceEnv, nil
	}
	if path := os.Getenv(envFileKey); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", "", fmt.Errorf("secret: read %s (from $%s): %w", path, envFileKey, err)
		}
		return strings.TrimSpace(string(data)), SourceFileEnv, nil
	}
	if defaultMountPath != "" {
		data, err := os.ReadFile(defaultMountPath)
		if err == nil {
			return strings.TrimSpace(string(data)), SourceMount, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("secret: read %s: %w", defaultMountPath, err)
		}
	}
	return "", "", fmt.Errorf("secret: none of $%s, $%s, or %s resolved", envKey, envFileKey, defaultMountPath)
}
```

- [ ] **Step 2: Move the test to `internal/secret/secret_test.go`** (port verbatim, swapping `config.` → `secret.` and `package config_test` → `package secret_test`)

```go
package secret_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nova-archive/nova/internal/secret"
	"github.com/stretchr/testify/require"
)

func TestResolverPrefersEnvVar(t *testing.T) {
	t.Setenv("FOO", "from-env")
	t.Setenv("FOO_FILE", filepath.Join(t.TempDir(), "ignored"))
	got, src, err := secret.ResolveSecret("FOO", "FOO_FILE", "/dev/null")
	require.NoError(t, err)
	require.Equal(t, "from-env", got)
	require.Equal(t, secret.SourceEnv, src)
}

func TestResolverFallsBackToFileEnv(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "secret.txt")
	require.NoError(t, os.WriteFile(file, []byte("from-file-env\n"), 0o600))
	t.Setenv("BAR", "")
	t.Setenv("BAR_FILE", file)
	got, src, err := secret.ResolveSecret("BAR", "BAR_FILE", "/dev/null")
	require.NoError(t, err)
	require.Equal(t, "from-file-env", got, "trailing newline trimmed")
	require.Equal(t, secret.SourceFileEnv, src)
}

func TestResolverFallsBackToDefaultMountPath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "secret.txt")
	require.NoError(t, os.WriteFile(file, []byte("from-mount"), 0o600))
	t.Setenv("BAZ", "")
	t.Setenv("BAZ_FILE", "")
	got, src, err := secret.ResolveSecret("BAZ", "BAZ_FILE", file)
	require.NoError(t, err)
	require.Equal(t, "from-mount", got)
	require.Equal(t, secret.SourceMount, src)
}

func TestResolverErrorsWhenNoneAvailable(t *testing.T) {
	t.Setenv("QUUX", "")
	t.Setenv("QUUX_FILE", "")
	_, _, err := secret.ResolveSecret("QUUX", "QUUX_FILE", "/nonexistent")
	require.Error(t, err)
}
```

- [ ] **Step 3: Delete the originals and run the new package test**

```bash
git rm internal/config/secrets.go internal/config/secrets_test.go
go test ./internal/secret/ -v
```
Expected: 4 tests PASS.

- [ ] **Step 4: Re-point `internal/envelope/keystore.go`** — swap the import (line 16) `"github.com/nova-archive/nova/internal/config"` → `"github.com/nova-archive/nova/internal/secret"`, and the call (line 101) `config.ResolveSecret(` → `secret.ResolveSecret(`. (keystore.go uses `config` for nothing else — confirm with `grep -n 'config\.' internal/envelope/keystore.go` returning only the resolver line.)

- [ ] **Step 5: Re-point `cmd/coordinator/main.go:416`** — `config.ResolveSecret(` → `secret.ResolveSecret(`; add `"github.com/nova-archive/nova/internal/secret"` to the import block. `string(signerSrc)` (line 421) is unaffected (`Source` is still a string type). Leave the other `config.` uses in main.go intact.

- [ ] **Step 6: Build, vet, and run the affected suites**

```bash
go build ./... && go vet ./...
go test ./internal/secret/ ./internal/config/ ./internal/envelope/ ./cmd/coordinator/
```
Expected: all green (the coordinator suite is the regression guard that behavior is unchanged).

- [ ] **Step 7: Commit**

```bash
git add internal/secret/ cmd/coordinator/main.go internal/envelope/keystore.go
git commit -s -m "refactor(secret): extract ResolveSecret into stdlib leaf internal/secret (P2-M1)" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: `internal/federation/wire` — messages + capability negotiation + token

**Files:**
- Create: `internal/federation/wire/messages.go`, `capability.go`, `token.go`
- Test: `internal/federation/wire/capability_test.go`, `token_test.go`

- [ ] **Step 1: Write `internal/federation/wire/messages.go`** (pure types; no behavior)

```go
// Package wire holds the federation protocol's shared, dependency-free types:
// the fed/v1 request/response messages, capability identifiers, normalized
// error codes, and the Ed25519 repair-token claim + verification. It is the
// only Nova package besides internal/secret that both the coordinator and the
// donor import. No operator-only dependencies may enter here.
package wire

// Protocol identifiers negotiated at register time.
const ProtocolV1 = "fed/v1"

// Capability identifiers (D-cap). A donor advertises the set it offers; the
// coordinator declares the set it requires.
const (
	CapPinChangeLog   = "pin-change-log/v1"
	CapSnapshot       = "snapshot/v1"
	CapRepairStream   = "repair-stream/v1"
	CapAuditBlockHash = "audit-block-hash/v1"
)

// Normalized machine-readable error codes carried in ErrorResponse.Code.
const (
	CodeSnapshotRequired   = "snapshot_required"   // since_seq predates retention (D7)
	CodeIncompatible       = "incompatible_capabilities"
	CodeUnknownChangeKind  = "unknown_change_kind" // fail-closed (D7)
)

// ErrorResponse is the normalized error envelope for fed/v1 responses.
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// RegisterRequest is sent by a donor; identity is derived from the verified
// mTLS cert, NOT these fields (D-cap). The JSON carries only negotiation inputs.
type RegisterRequest struct {
	SupportedProtocols []string `json:"supported_protocols"`
	Capabilities       []string `json:"capabilities"`
	ClientVersion      string   `json:"client_version,omitempty"`
}

// RegisterResponse confirms the selected protocol + required capabilities.
type RegisterResponse struct {
	SelectedProtocol     string   `json:"selected_protocol"`
	RequiredCapabilities []string `json:"required_capabilities"`
	NodeID               string   `json:"node_id"` // derived from the cert
}

// The remaining fed/v1 message shapes the M2–M4 handlers consume. Snapshot
// recovery (snapshot/epoch) gets its own types when M3 implements it.
type HeartbeatRequest struct {
	FreeBytes   int64 `json:"free_bytes"`
	StoredBytes int64 `json:"stored_bytes"`
}
type HeartbeatResponse struct {
	RepairTokenPublicKey string `json:"repair_token_public_key,omitempty"` // base64; delivered via config (D1)
}
type ChangesRequest struct {
	SinceSeq int64 `json:"since_seq"`
}
type ChangesResponse struct {
	Changes     []PinChange `json:"changes"`
	CurrentSeq  int64       `json:"current_seq"`
	CurrentEpoch int64      `json:"current_epoch"`
}
type PinChange struct {
	Sequence     int64  `json:"sequence"`
	AssignmentID string `json:"assignment_id"`
	Generation   int64  `json:"generation"`
	Kind         string `json:"kind"`
	CID          string `json:"cid"`
}
type Ack struct {
	AssignmentID string `json:"assignment_id"`
	Generation   int64  `json:"generation"`
	CID          string `json:"cid"`
}
type Fail struct {
	AssignmentID string `json:"assignment_id"`
	Generation   int64  `json:"generation"`
	CID          string `json:"cid"`
	Reason       string `json:"reason"`
}
```

- [ ] **Step 2: Write the failing capability test `capability_test.go`**

```go
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
```

- [ ] **Step 3: Run it to verify it fails**

Run: `go test ./internal/federation/wire/ -run TestNegotiate -v`
Expected: FAIL — `undefined: wire.NegotiateCapabilities`.

- [ ] **Step 4: Write `internal/federation/wire/capability.go`**

```go
package wire

// NegotiateCapabilities reports whether offered ⊇ required. When it is not, ok
// is false and missing lists the required capabilities the donor did not offer,
// in required's order. Negotiation FAILS CLOSED: an empty overlap is a clear
// failure, never a silent downgrade (D-cap, D7).
func NegotiateCapabilities(offered, required []string) (missing []string, ok bool) {
	have := make(map[string]struct{}, len(offered))
	for _, c := range offered {
		have[c] = struct{}{}
	}
	for _, r := range required {
		if _, found := have[r]; !found {
			missing = append(missing, r)
		}
	}
	return missing, len(missing) == 0
}
```

- [ ] **Step 5: Run it to verify it passes**

Run: `go test ./internal/federation/wire/ -run TestNegotiate -v`
Expected: PASS (both).

- [ ] **Step 6: Write the failing token test `token_test.go`** (defines the canonical format contract)

```go
package wire_test

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/stretchr/testify/require"
)

func sampleClaims(now time.Time) wire.Claims {
	return wire.Claims{
		JTI: "jti-1", AssignmentID: "a-1", Generation: 7, CID: "bafyTEST",
		SourceNodeID: "src", DestNodeID: "dst",
		NotBefore: now.Add(-time.Minute).Unix(), NotAfter: now.Add(time.Minute).Unix(),
		MaxBytes: 1 << 20, ProtocolVersion: wire.ProtocolV1,
	}
}

// signTestToken builds a valid token via the exported format primitives. There
// is intentionally NO wire.SignToken (minting with a private key is
// coordinator-only, M4); tests sign locally with a throwaway key.
func signTestToken(t *testing.T, priv ed25519.PrivateKey, c wire.Claims) string {
	t.Helper()
	si, err := wire.SigningInput(c)
	require.NoError(t, err)
	return wire.AssembleToken(si, ed25519.Sign(priv, []byte(si)))
}

func TestTokenRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_700_000_000, 0)
	tok := signTestToken(t, priv, sampleClaims(now))
	require.Contains(t, tok, ".")
	got, err := wire.Verify(pub, tok, now)
	require.NoError(t, err)
	require.Equal(t, "jti-1", got.JTI)
	require.Equal(t, int64(7), got.Generation)
}

func TestTokenExpired(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_700_000_000, 0)
	tok := signTestToken(t, priv, sampleClaims(now))
	_, err := wire.Verify(pub, tok, now.Add(2*time.Minute))
	require.ErrorIs(t, err, wire.ErrExpired)
}

func TestTokenNotYetValid(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_700_000_000, 0)
	tok := signTestToken(t, priv, sampleClaims(now))
	_, err := wire.Verify(pub, tok, now.Add(-2*time.Minute))
	require.ErrorIs(t, err, wire.ErrNotYetValid)
}

func TestTokenWrongKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	otherPub, _, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_700_000_000, 0)
	tok := signTestToken(t, priv, sampleClaims(now))
	_, err := wire.Verify(otherPub, tok, now)
	require.ErrorIs(t, err, wire.ErrBadSignature)
}

func TestTokenTamperedClaim(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_700_000_000, 0)
	tok := signTestToken(t, priv, sampleClaims(now))
	parts := strings.SplitN(tok, ".", 2)
	// Flip a byte in the claims segment; the signature no longer matches.
	tampered := parts[0][:len(parts[0])-1] + "Z" + "." + parts[1]
	_, err := wire.Verify(pub, tampered, now)
	require.Error(t, err)
}

func TestTokenMissingJTI(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_700_000_000, 0)
	c := sampleClaims(now)
	c.JTI = ""
	tok := signTestToken(t, priv, c)
	_, err := wire.Verify(pub, tok, now)
	require.ErrorIs(t, err, wire.ErrMalformedClaims)
}

func TestTokenRejectsWrongProtocol(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_700_000_000, 0)
	c := sampleClaims(now)
	c.ProtocolVersion = "fed/v99"
	tok := signTestToken(t, priv, c)
	_, err := wire.Verify(pub, tok, now)
	require.ErrorIs(t, err, wire.ErrMalformedClaims)
}

func TestTokenRejectsNonPositiveGenerationAndBytes(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_700_000_000, 0)
	for _, mut := range []func(c *wire.Claims){
		func(c *wire.Claims) { c.Generation = 0 },
		func(c *wire.Claims) { c.MaxBytes = 0 },
		func(c *wire.Claims) { c.NotBefore, c.NotAfter = c.NotAfter, c.NotBefore },
	} {
		c := sampleClaims(now)
		mut(&c)
		_, err := wire.Verify(pub, signTestToken(t, priv, c), now)
		require.ErrorIs(t, err, wire.ErrMalformedClaims)
	}
}

func TestTokenMalformedToken(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	_, err := wire.Verify(pub, "not-a-token", time.Now())
	require.ErrorIs(t, err, wire.ErrMalformedToken)
}
```

- [ ] **Step 7: Run it to verify it fails**

Run: `go test ./internal/federation/wire/ -run TestToken -v`
Expected: FAIL — `undefined: wire.Claims` / `wire.SigningInput` / `wire.AssembleToken` / `wire.Verify`.

- [ ] **Step 8: Write `internal/federation/wire/token.go`** (canonical `base64url(claims_json) "." base64url(sig)`; verify over the received claims segment, never re-marshal)

```go
package wire

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Repair-token verification errors (D1). M1 ships Verify; the coordinator-side
// MINT FLOW and the source-side single-use jti replay cache land in M4 — Verify
// itself does NOT enforce replay.
var (
	ErrMalformedToken  = errors.New("wire: malformed token")
	ErrMalformedClaims = errors.New("wire: malformed claims")
	ErrBadSignature    = errors.New("wire: bad signature")
	ErrNotYetValid     = errors.New("wire: token not yet valid")
	ErrExpired         = errors.New("wire: token expired")
)

// Claims is the Ed25519 repair-token payload (D1). In M1 the id/cid fields are
// opaque non-empty strings; deeper CID/UUID parsing lands when transfer/register
// code needs it (no go-cid/UUID dependency enters the donor graph yet).
type Claims struct {
	JTI             string `json:"jti"`
	AssignmentID    string `json:"assignment_id"`
	Generation      int64  `json:"generation"`
	CID             string `json:"cid"`
	SourceNodeID    string `json:"source_node_id"`
	DestNodeID      string `json:"dest_node_id"`
	NotBefore       int64  `json:"not_before"`
	NotAfter        int64  `json:"not_after"`
	MaxBytes        int64  `json:"max_bytes"`
	ProtocolVersion string `json:"protocol_version"`
}

var b64 = base64.RawURLEncoding

// SigningInput returns the canonical signing input for a token:
// base64url(claims_json). claims_json is deterministic because Claims marshals
// in fixed struct-field order. The coordinator signs []byte(SigningInput) with
// its Ed25519 PRIVATE key in the coordinator-only internal/federation/tokens
// package (M4). This shared package holds NO private-key / minting API — donors
// only ever Verify.
func SigningInput(c Claims) (string, error) {
	cj, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return b64.EncodeToString(cj), nil
}

// AssembleToken joins a signing input and its raw signature into the wire token
// "signingInput.base64url(sig)". It performs no signing (no private key).
func AssembleToken(signingInput string, sig []byte) string {
	return signingInput + "." + b64.EncodeToString(sig)
}

// Verify checks the Ed25519 signature over the RECEIVED claims segment (it does
// not re-marshal), then decodes the claims and validates structure + the
// not_before/not_after window against now. It does NOT check replay.
func Verify(pub ed25519.PublicKey, token string, now time.Time) (Claims, error) {
	seg, sigPart, found := strings.Cut(token, ".")
	if !found || seg == "" || sigPart == "" {
		return Claims{}, ErrMalformedToken
	}
	sig, err := b64.DecodeString(sigPart)
	if err != nil {
		return Claims{}, ErrMalformedToken
	}
	if !ed25519.Verify(pub, []byte(seg), sig) {
		return Claims{}, ErrBadSignature
	}
	cj, err := b64.DecodeString(seg)
	if err != nil {
		return Claims{}, ErrMalformedToken
	}
	var c Claims
	if err := json.Unmarshal(cj, &c); err != nil {
		return Claims{}, ErrMalformedClaims
	}
	if c.JTI == "" || c.AssignmentID == "" || c.CID == "" || c.SourceNodeID == "" || c.DestNodeID == "" {
		return Claims{}, ErrMalformedClaims
	}
	if c.ProtocolVersion != ProtocolV1 || c.Generation <= 0 || c.MaxBytes <= 0 || c.NotBefore >= c.NotAfter {
		return Claims{}, ErrMalformedClaims
	}
	ts := now.Unix()
	if ts < c.NotBefore {
		return Claims{}, ErrNotYetValid
	}
	if ts > c.NotAfter {
		return Claims{}, ErrExpired
	}
	return c, nil
}
```

- [ ] **Step 9: Run the full wire suite**

Run: `go test ./internal/federation/wire/ -v`
Expected: all PASS (capability ×2, token ×9).

- [ ] **Step 10: Commit**

```bash
git add internal/federation/wire/
git commit -s -m "feat(wire): fed/v1 message types + capability negotiation + canonical Ed25519 token Verify (P2-M1)" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: `internal/node/bandwidth` — authoritative daily token-bucket

**Files:**
- Create: `internal/node/bandwidth/bucket.go`
- Test: `internal/node/bandwidth/bucket_test.go`

- [ ] **Step 1: Write the failing test `bucket_test.go`**

```go
package bandwidth_test

import (
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/node/bandwidth"
	"github.com/stretchr/testify/require"
)

func TestBucketAllowsUpToBudgetThenRefuses(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	b := bandwidth.NewDailyBucket(1000, now)
	require.True(t, b.Take(600, now))
	require.True(t, b.Take(400, now)) // exactly the budget consumed
	require.False(t, b.Take(1, now))  // over budget — refused
}

func TestBucketRefillsOverTime(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	b := bandwidth.NewDailyBucket(86_400, now) // 1 byte/sec
	require.True(t, b.Take(86_400, now))       // drain
	require.False(t, b.Take(1, now))
	later := now.Add(10 * time.Second)
	require.True(t, b.Take(10, later)) // 10s × 1 B/s refilled
	require.False(t, b.Take(1, later))
}

func TestBucketRefusesSingleRequestExceedingCapacity(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	b := bandwidth.NewDailyBucket(1000, now)
	require.False(t, b.Take(1001, now)) // larger than a full day's budget
}

func TestBucketRefillCapsAtCapacity(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	b := bandwidth.NewDailyBucket(1000, now)
	require.True(t, b.Take(1000, now))
	veryLater := now.Add(48 * time.Hour) // would over-refill if uncapped
	require.True(t, b.Take(1000, veryLater))
	require.False(t, b.Take(1, veryLater)) // capacity, not 2×
}

func TestBucketRejectsNonPositiveTake(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	b := bandwidth.NewDailyBucket(1000, now)
	require.False(t, b.Take(0, now))
	require.False(t, b.Take(-100, now)) // must NOT credit tokens
	require.True(t, b.Take(1000, now))  // budget intact after the bad takes
}

func TestBucketWithNonPositiveBudgetRefusesAll(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	require.False(t, bandwidth.NewDailyBucket(0, now).Take(1, now))
	require.False(t, bandwidth.NewDailyBucket(-5, now).Take(1, now))
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/node/bandwidth/ -v`
Expected: FAIL — `undefined: bandwidth.NewDailyBucket`.

- [ ] **Step 3: Write `internal/node/bandwidth/bucket.go`**

```go
// Package bandwidth implements the donor's AUTHORITATIVE budget enforcer (D11):
// the coordinator only reserves best-effort, but the node that actually sends
// bytes refuses work exceeding its configured budget. A classic token bucket
// with capacity == one day's budget and a per-second refill of budget/86400.
package bandwidth

import (
	"sync"
	"time"
)

// Bucket is a thread-safe token bucket. Tokens are bytes.
type Bucket struct {
	mu           sync.Mutex
	capacity     float64
	tokens       float64
	refillPerSec float64
	last         time.Time
}

// NewDailyBucket returns a bucket that allows bytesPerDay bytes per rolling day,
// starting full at now.
func NewDailyBucket(bytesPerDay int64, now time.Time) *Bucket {
	cap := float64(bytesPerDay)
	if cap < 0 { // defensive: a non-positive budget refuses all work
		cap = 0
	}
	return &Bucket{
		capacity:     cap,
		tokens:       cap,
		refillPerSec: cap / 86_400.0,
		last:         now,
	}
}

// Take attempts to consume n bytes as of now. A non-positive n is rejected
// (never credits tokens). It refills based on elapsed time (capped at
// capacity), then succeeds and deducts iff enough tokens remain. A request
// larger than capacity can never succeed.
func (b *Bucket) Take(n int64, now time.Time) bool {
	if n <= 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * b.refillPerSec
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = now
	}
	if float64(n) > b.tokens {
		return false
	}
	b.tokens -= float64(n)
	return true
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./internal/node/bandwidth/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/node/bandwidth/
git commit -s -m "feat(node): authoritative daily bandwidth token-bucket (D11) (P2-M1)" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: `internal/node/config` — donor `node.yaml` (path-based, shallow validation)

**Files:**
- Create: `internal/node/config/config.go`
- Test: `internal/node/config/config_test.go`

- [ ] **Step 1: Write the failing test `config_test.go`** (uses a helper that writes real fixture files so path-readability checks are exercised)

```go
package config_test

import (
	"os"
	"path/filepath"
	"testing"

	nodeconfig "github.com/nova-archive/nova/internal/node/config"
	"github.com/stretchr/testify/require"
)

// writeValid creates a temp dir with readable fixture files for every *_path
// field and returns rendered YAML plus the temp dir.
func writeValid(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"fed.crt", "fed.key", "neb.crt", "neb.key", "swarm.key"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600))
	}
	storage := filepath.Join(dir, "data")
	require.NoError(t, os.MkdirAll(storage, 0o700))
	yaml := "coordinator_url: https://coord.example\n" +
		"federation_cert_path: " + filepath.Join(dir, "fed.crt") + "\n" +
		"federation_key_path: " + filepath.Join(dir, "fed.key") + "\n" +
		"nebula_cert_path: " + filepath.Join(dir, "neb.crt") + "\n" +
		"nebula_key_path: " + filepath.Join(dir, "neb.key") + "\n" +
		"swarm_key_path: " + filepath.Join(dir, "swarm.key") + "\n" +
		"storage_dir: " + storage + "\n" +
		"bandwidth_budget_bytes_per_day: 53687091200\n"
	return yaml, dir
}

func TestLoadMinimalValid(t *testing.T) {
	yaml, _ := writeValid(t)
	cfg, err := nodeconfig.LoadFromBytes([]byte(yaml))
	require.NoError(t, err)
	require.Equal(t, "https://coord.example", cfg.CoordinatorURL)
	require.Equal(t, int64(53687091200), cfg.BandwidthBudgetBytesPerDay)
	require.Equal(t, nodeconfig.DefaultHealthListenAddr, cfg.HealthListenAddr, "default applied")
}

func TestLoadRejectsMalformedYAML(t *testing.T) {
	_, err := nodeconfig.LoadFromBytes([]byte("coordinator_url: [unterminated"))
	require.Error(t, err)
}

func TestValidateRejectsMissingCoordinatorURL(t *testing.T) {
	yaml, _ := writeValid(t)
	yaml = "coordinator_url: \"\"\n" + yaml[len("coordinator_url: https://coord.example\n"):]
	_, err := nodeconfig.LoadFromBytes([]byte(yaml))
	require.ErrorContains(t, err, "coordinator_url")
}

func TestValidateRejectsMissingCertFile(t *testing.T) {
	yaml, dir := writeValid(t)
	require.NoError(t, os.Remove(filepath.Join(dir, "fed.crt")))
	_, err := nodeconfig.LoadFromBytes([]byte(yaml))
	require.ErrorContains(t, err, "federation_cert_path")
}

func TestValidateRejectsCertPathThatIsDirectory(t *testing.T) {
	yaml, dir := writeValid(t)
	require.NoError(t, os.Remove(filepath.Join(dir, "neb.key")))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "neb.key"), 0o700))
	_, err := nodeconfig.LoadFromBytes([]byte(yaml))
	require.ErrorContains(t, err, "nebula_key_path")
}

func TestValidateRejectsNonPositiveBudget(t *testing.T) {
	yaml, _ := writeValid(t)
	yaml = yaml[:len(yaml)-len("bandwidth_budget_bytes_per_day: 53687091200\n")] +
		"bandwidth_budget_bytes_per_day: 0\n"
	_, err := nodeconfig.LoadFromBytes([]byte(yaml))
	require.ErrorContains(t, err, "bandwidth_budget_bytes_per_day")
}

func TestValidateCreatesMissingStorageDir(t *testing.T) {
	yaml, dir := writeValid(t)
	storage := filepath.Join(dir, "data")
	require.NoError(t, os.RemoveAll(storage)) // absent but creatable under dir
	_, err := nodeconfig.LoadFromBytes([]byte(yaml))
	require.NoError(t, err)
	info, statErr := os.Stat(storage)
	require.NoError(t, statErr)
	require.True(t, info.IsDir())
}

func TestValidateRejectsBadCoordinatorURL(t *testing.T) {
	yaml, _ := writeValid(t)
	yaml = "coordinator_url: \"not a url\"\n" + yaml[len("coordinator_url: https://coord.example\n"):]
	_, err := nodeconfig.LoadFromBytes([]byte(yaml))
	require.ErrorContains(t, err, "coordinator_url")
}

func TestValidateRejectsBadHealthAddr(t *testing.T) {
	yaml, _ := writeValid(t)
	yaml += "health_listen_addr: not-a-host-port\n"
	_, err := nodeconfig.LoadFromBytes([]byte(yaml))
	require.ErrorContains(t, err, "health_listen_addr")
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/node/config/ -v`
Expected: FAIL — `undefined: nodeconfig.LoadFromBytes`.

- [ ] **Step 3: Write `internal/node/config/config.go`**

```go
// Package config defines and validates the donor (nova-node) configuration. It
// is DONOR-LOCAL and deliberately separate from internal/config (the operator
// config home) so cmd/node never imports operator code. All secret material is
// referenced by *_path fields: node.yaml carries filesystem paths, never inline
// secret bytes. Validation is intentionally SHALLOW — it checks references, not
// cert chains — so a build-boundary milestone does not become cert provisioning.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// DefaultHealthListenAddr binds the M1 health endpoint to loopback (there is no
// Nebula interface yet; the federation listener binds the overlay from M2).
const DefaultHealthListenAddr = "127.0.0.1:9100"

// FailureDomain holds operator-DECLARED anti-affinity hints. They are
// informational at the donor and become authoritative only when
// operator-verified at the coordinator (D8).
type FailureDomain struct {
	Provider string `yaml:"provider"`
	ASN      string `yaml:"asn"`
	Region   string `yaml:"region"`
}

// Config is the donor node.yaml schema.
type Config struct {
	CoordinatorURL             string        `yaml:"coordinator_url"`
	FederationCertPath         string        `yaml:"federation_cert_path"`
	FederationKeyPath          string        `yaml:"federation_key_path"`
	NebulaCertPath             string        `yaml:"nebula_cert_path"`
	NebulaKeyPath              string        `yaml:"nebula_key_path"`
	SwarmKeyPath               string        `yaml:"swarm_key_path"`
	StorageDir                 string        `yaml:"storage_dir"`
	BandwidthBudgetBytesPerDay int64         `yaml:"bandwidth_budget_bytes_per_day"`
	FailureDomain              FailureDomain `yaml:"failure_domain"`
	HealthListenAddr           string        `yaml:"health_listen_addr"`
}

// LoadFromFile reads, parses, defaults, and validates a node.yaml.
func LoadFromFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("node config: read %s: %w", path, err)
	}
	return LoadFromBytes(data)
}

// LoadFromBytes parses node.yaml, applies defaults, and validates.
func LoadFromBytes(data []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("node config: parse: %w", err)
	}
	if c.HealthListenAddr == "" {
		c.HealthListenAddr = DefaultHealthListenAddr
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.CoordinatorURL == "" {
		return fmt.Errorf("node config: coordinator_url is required")
	}
	if u, err := url.ParseRequestURI(c.CoordinatorURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("node config: coordinator_url %q is not a valid http(s) URL", c.CoordinatorURL)
	}
	files := map[string]string{
		"federation_cert_path": c.FederationCertPath,
		"federation_key_path":  c.FederationKeyPath,
		"nebula_cert_path":     c.NebulaCertPath,
		"nebula_key_path":      c.NebulaKeyPath,
		"swarm_key_path":       c.SwarmKeyPath,
	}
	for field, path := range files {
		if err := checkReadableFile(field, path); err != nil {
			return err
		}
	}
	if c.BandwidthBudgetBytesPerDay <= 0 {
		return fmt.Errorf("node config: bandwidth_budget_bytes_per_day must be positive")
	}
	if _, _, err := net.SplitHostPort(c.HealthListenAddr); err != nil {
		return fmt.Errorf("node config: health_listen_addr %q is not host:port: %w", c.HealthListenAddr, err)
	}
	if c.StorageDir == "" {
		return fmt.Errorf("node config: storage_dir is required")
	}
	if err := os.MkdirAll(c.StorageDir, 0o700); err != nil {
		return fmt.Errorf("node config: storage_dir %q not usable: %w", c.StorageDir, err)
	}
	if err := checkWritableDir(c.StorageDir); err != nil {
		return err
	}
	return nil
}

// checkWritableDir confirms storage_dir is actually writable by the current
// uid — important because the donor container runs distroless-nonroot against a
// mounted volume. It writes and removes a probe file.
func checkWritableDir(dir string) error {
	probe := filepath.Join(dir, ".nova-write-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		return fmt.Errorf("node config: storage_dir %q not writable: %w", dir, err)
	}
	_ = os.Remove(probe)
	return nil
}

// checkReadableFile verifies a *_path is set, exists, is a regular file (not a
// directory), and is readable. It does NOT parse the contents.
func checkReadableFile(field, path string) error {
	if path == "" {
		return fmt.Errorf("node config: %s is required", field)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("node config: %s %q: %w", field, path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("node config: %s %q is a directory, want a file", field, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("node config: %s %q not readable: %w", field, path, err)
	}
	_ = f.Close()
	return nil
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./internal/node/config/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/node/config/
git commit -s -m "feat(node): donor node.yaml schema + shallow path-based validation (P2-M1)" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: `internal/node/{state,agent,transfer,audit}` skeletons

**Files:**
- Create: `internal/node/state/store.go`, `internal/node/agent/agent.go`, `internal/node/transfer/transfer.go`, `internal/node/audit/responder.go`
- Test: `internal/node/state/store_test.go`, `internal/node/agent/agent_test.go`

- [ ] **Step 1: Write `internal/node/state/store.go`** (interface + in-memory stub; real persistent store is M3/M4)

```go
// Package state is the donor's LOCAL persistence seam — assignment cursor, cert
// material handles, and the single-use repair-token jti replay cache. It uses
// NO Postgres (donors hold no catalog). M1 ships the interface + an in-memory
// stub; the durable file/KV implementation lands in M3/M4.
package state

import "time"

// Store is the donor's local state. Cursor methods back the change-log sync
// (M3); the jti methods back single-use repair-token enforcement (M4).
type Store interface {
	Cursor() (int64, error)
	SetCursor(seq int64) error
	SeenJTI(jti string) (bool, error)
	RecordJTI(jti string, exp time.Time) error
}

// MemStore is an in-memory Store stub for M1/tests.
type MemStore struct {
	cursor int64
	jtis   map[string]time.Time
}

func NewMemStore() *MemStore { return &MemStore{jtis: map[string]time.Time{}} }

func (m *MemStore) Cursor() (int64, error)    { return m.cursor, nil }
func (m *MemStore) SetCursor(seq int64) error { m.cursor = seq; return nil }

// SeenJTI reports whether jti was recorded and has NOT expired. Expired entries
// are pruned lazily so the replay cache cannot report a stale hit or grow
// unbounded — the semantics M4's source-side single-use enforcement relies on.
func (m *MemStore) SeenJTI(jti string) (bool, error) {
	exp, ok := m.jtis[jti]
	if !ok {
		return false, nil
	}
	if !time.Now().Before(exp) {
		delete(m.jtis, jti)
		return false, nil
	}
	return true, nil
}

func (m *MemStore) RecordJTI(jti string, exp time.Time) error {
	m.jtis[jti] = exp
	return nil
}

var _ Store = (*MemStore)(nil)
```

- [ ] **Step 2: Write `internal/node/state/store_test.go`**

```go
package state_test

import (
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/node/state"
	"github.com/stretchr/testify/require"
)

func TestMemStoreCursor(t *testing.T) {
	s := state.NewMemStore()
	require.NoError(t, s.SetCursor(42))
	c, err := s.Cursor()
	require.NoError(t, err)
	require.Equal(t, int64(42), c)
}

func TestMemStoreJTI(t *testing.T) {
	s := state.NewMemStore()
	seen, err := s.SeenJTI("x")
	require.NoError(t, err)
	require.False(t, seen)
	require.NoError(t, s.RecordJTI("x", time.Now().Add(time.Hour)))
	seen, _ = s.SeenJTI("x")
	require.True(t, seen)
}

func TestMemStoreJTIExpiry(t *testing.T) {
	s := state.NewMemStore()
	require.NoError(t, s.RecordJTI("old", time.Now().Add(-time.Second)))
	seen, err := s.SeenJTI("old")
	require.NoError(t, err)
	require.False(t, seen, "expired jti must not count as seen")
}
```

- [ ] **Step 3: Write `internal/node/agent/agent.go`** (no-op loop; real transport is M2+)

```go
// Package agent runs the donor's register→heartbeat→sync loop. In M1 the loop
// is a NO-OP: there is no transport, no Nebula, and no coordinator contact. It
// exists so cmd/node has a lifecycle to start and stop; M2 fills in registration
// and heartbeats, M3 assignment sync.
package agent

import (
	"context"

	nodeconfig "github.com/nova-archive/nova/internal/node/config"
	"github.com/nova-archive/nova/internal/node/state"
)

// Agent owns the donor's control loop.
type Agent struct {
	cfg   *nodeconfig.Config
	store state.Store
}

// New constructs an Agent. The store is the donor's local state seam.
func New(cfg *nodeconfig.Config, store state.Store) *Agent {
	return &Agent{cfg: cfg, store: store}
}

// Run blocks until ctx is cancelled. M1: no work is performed.
func (a *Agent) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
```

- [ ] **Step 4: Write `internal/node/agent/agent_test.go`** (the no-op loop returns when ctx is cancelled)

```go
package agent_test

import (
	"context"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/node/agent"
	nodeconfig "github.com/nova-archive/nova/internal/node/config"
	"github.com/nova-archive/nova/internal/node/state"
	"github.com/stretchr/testify/require"
)

func TestAgentRunStopsOnContextCancel(t *testing.T) {
	a := agent.New(&nodeconfig.Config{}, state.NewMemStore())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("agent.Run did not return after cancel")
	}
}
```

- [ ] **Step 5: Write `internal/node/transfer/transfer.go` and `internal/node/audit/responder.go`** (interface seams; real impls are M4 / M6)

```go
// Package transfer is the donor's blob-fetch + verify seam: stream ciphertext
// from a source, re-import deterministically per IPFS_IMPORT_RULES, and compare
// the computed root CID to the assigned CID (D4). M1 ships only the interface;
// the streaming + re-import implementation lands in M4.
package transfer

import (
	"context"
	"errors"
	"io"
)

// ErrNotImplemented marks the M1 stub.
var ErrNotImplemented = errors.New("transfer: not implemented until P2-M4")

// Verifier fetches and verifies a blob by deterministic re-import.
type Verifier interface {
	VerifyReimport(ctx context.Context, r io.Reader, expectCID string) error
}

type stub struct{}

// NewStub returns an M1 placeholder Verifier.
func NewStub() Verifier { return stub{} }

func (stub) VerifyReimport(context.Context, io.Reader, string) error { return ErrNotImplemented }
```

```go
// Package audit is the donor's possession-challenge responder seam: answer a
// coordinator challenge from the LOCAL blockstore only (no lawful in-window
// fetch). M1 ships only the interface; the synchronous responder lands in M6.
package audit

import (
	"context"
	"errors"
)

// ErrNotImplemented marks the M1 stub.
var ErrNotImplemented = errors.New("audit: not implemented until P2-M6")

// Challenge / Response are placeholder shapes refined in M6.
type Challenge struct {
	CID         string
	BlockIndex  int64
	Nonce       string
}
type Response struct {
	Digest string
}

// Responder answers possession challenges.
type Responder interface {
	Respond(ctx context.Context, c Challenge) (Response, error)
}

type stub struct{}

// NewStub returns an M1 placeholder Responder.
func NewStub() Responder { return stub{} }

func (stub) Respond(context.Context, Challenge) (Response, error) {
	return Response{}, ErrNotImplemented
}
```

- [ ] **Step 6: Run the node-subsystem suites + build**

Run: `go test ./internal/node/... && go build ./internal/node/...`
Expected: state + agent tests PASS; transfer/audit compile.

- [ ] **Step 7: Commit**

```bash
git add internal/node/state/ internal/node/agent/ internal/node/transfer/ internal/node/audit/
git commit -s -m "feat(node): state/agent/transfer/audit skeletons (no-op transport, M2/M4/M6 seams) (P2-M1)" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 6: `cmd/node` — donor entrypoint (`--config` / `--validate` / `--healthcheck`)

**Files:**
- Create: `cmd/node/main.go`, `cmd/node/main_test.go`

- [ ] **Step 1: Write `cmd/node/main.go`**

```go
// Command nova-node is the Nova donor pinning node. In P2-M1 it loads and
// validates node.yaml, serves a loopback health endpoint, and runs a no-op
// agent loop — NO live federation. Live registration/transport arrive in M2+.
//
// Flags:
//
//	--config PATH    node.yaml path (required)
//	--validate       load + validate, then exit (0 ok / non-zero on error)
//	--healthcheck    GET the configured health endpoint, then exit (container HEALTHCHECK)
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nova-archive/nova/internal/node/agent"
	nodeconfig "github.com/nova-archive/nova/internal/node/config"
	"github.com/nova-archive/nova/internal/node/state"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "nova-node:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet(stderr)
	var (
		configPath  = fs.String("config", "", "path to node.yaml")
		validate    = fs.Bool("validate", false, "validate config and exit")
		healthcheck = fs.Bool("healthcheck", false, "probe the health endpoint and exit")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return fmt.Errorf("--config is required")
	}
	cfg, err := nodeconfig.LoadFromFile(*configPath)
	if err != nil {
		return err
	}
	switch {
	case *validate:
		fmt.Fprintln(stdout, "nova-node: config OK")
		return nil
	case *healthcheck:
		return probeHealth(cfg.HealthListenAddr)
	default:
		return serve(cfg, stdout)
	}
}

func probeHealth(addr string) error {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health endpoint returned %d", resp.StatusCode)
	}
	return nil
}

func serve(cfg *nodeconfig.Config, stdout io.Writer) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok","mode":"node-skeleton"}`)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	// Bind synchronously so a bad/occupied address fails fast instead of being
	// swallowed in a goroutine while the process blocks forever.
	ln, err := net.Listen("tcp", cfg.HealthListenAddr)
	if err != nil {
		return fmt.Errorf("health listen %s: %w", cfg.HealthListenAddr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srvErr := make(chan error, 1)
	go func() {
		if e := srv.Serve(ln); e != nil && !errors.Is(e, http.ErrServerClosed) {
			srvErr <- e
		}
	}()
	fmt.Fprintf(stdout, "nova-node: health on %s (no federation in M1)\n", cfg.HealthListenAddr)

	// The M1 agent is a no-op that returns on ctx cancel; start it for lifecycle
	// symmetry with M2+, but block on a signal OR a server failure.
	ag := agent.New(cfg, state.NewMemStore())
	go func() { _ = ag.Run(ctx) }()

	var runErr error
	select {
	case <-ctx.Done(): // SIGINT/SIGTERM
	case runErr = <-srvErr: // health server failed after bind
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if sErr := srv.Shutdown(shutCtx); sErr != nil && runErr == nil {
		runErr = sErr
	}
	return runErr
}
```

- [ ] **Step 2: Write `cmd/node/main.go`'s flag helper** — add to the same file (kept separate for a clear error stream in tests):

```go
import "flag"

// newFlagSet builds a ContinueOnError flag set writing usage to w.
func newFlagSet(w io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("nova-node", flag.ContinueOnError)
	fs.SetOutput(w)
	return fs
}
```

(Merge the `flag` import into the existing import block; do not duplicate the block.)

- [ ] **Step 3: Write the failing test `cmd/node/main_test.go`**

```go
package main

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeValidConfig(t *testing.T, healthAddr string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range []string{"fed.crt", "fed.key", "neb.crt", "neb.key", "swarm.key"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600))
	}
	storage := filepath.Join(dir, "data")
	require.NoError(t, os.MkdirAll(storage, 0o700))
	cfg := "coordinator_url: https://coord.example\n" +
		"federation_cert_path: " + filepath.Join(dir, "fed.crt") + "\n" +
		"federation_key_path: " + filepath.Join(dir, "fed.key") + "\n" +
		"nebula_cert_path: " + filepath.Join(dir, "neb.crt") + "\n" +
		"nebula_key_path: " + filepath.Join(dir, "neb.key") + "\n" +
		"swarm_key_path: " + filepath.Join(dir, "swarm.key") + "\n" +
		"storage_dir: " + storage + "\n" +
		"bandwidth_budget_bytes_per_day: 53687091200\n"
	if healthAddr != "" {
		cfg += "health_listen_addr: " + healthAddr + "\n"
	}
	path := filepath.Join(dir, "node.yaml")
	require.NoError(t, os.WriteFile(path, []byte(cfg), 0o600))
	return path
}

func TestValidateAcceptsValidConfig(t *testing.T) {
	path := writeValidConfig(t, "")
	err := run([]string{"--validate", "--config", path}, io.Discard, io.Discard)
	require.NoError(t, err)
}

func TestValidateRejectsMissingConfigFlag(t *testing.T) {
	err := run([]string{"--validate"}, io.Discard, io.Discard)
	require.ErrorContains(t, err, "--config is required")
}

func TestValidateRejectsMissingCertFile(t *testing.T) {
	path := writeValidConfig(t, "")
	require.NoError(t, os.Remove(filepath.Join(filepath.Dir(path), "fed.crt")))
	err := run([]string{"--validate", "--config", path}, io.Discard, io.Discard)
	require.ErrorContains(t, err, "federation_cert_path")
}

func TestServeFailsFastOnBindError(t *testing.T) {
	// Occupy a port, then point the donor's health addr at it. Bare run() →
	// serve() must return the bind error (not block) because net.Listen fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	path := writeValidConfig(t, ln.Addr().String())
	err = run([]string{"--config", path}, io.Discard, io.Discard)
	require.ErrorContains(t, err, "health listen")
}
```

- [ ] **Step 4: Run it to verify it fails, then passes**

Run: `go test ./cmd/node/ -v`
Expected: initially FAIL if any wiring is off; after Steps 1–2 are correct → all PASS.

- [ ] **Step 5: Confirm the donor builds standalone**

```bash
CGO_ENABLED=0 go build -o /tmp/nova-node ./cmd/node && echo OK
```
Expected: `OK` (pure-Go static build).

- [ ] **Step 6: Commit**

```bash
git add cmd/node/
git commit -s -m "feat(node): nova-node entrypoint — --config/--validate/--healthcheck + loopback health (P2-M1)" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 7: Dependency-boundary gate (the load-bearing milestone deliverable)

**Files:**
- Create: `scripts/check_node_deps.sh`
- Modify: `Makefile`

- [ ] **Step 1: Write `scripts/check_node_deps.sh`** (deny-by-default over **all** non-stdlib deps — first-party *and* third-party; stdlib filtered out). The allowlist is intentionally tiny; the donor's only third-party runtime dep is `gopkg.in/yaml.v3`. Run it once after Task 4 — if it surfaces an unexpected transitive (e.g. something yaml.v3 pulls), that is the gate doing its job: add only genuinely-needed entries, deliberately.

```bash
#!/usr/bin/env bash
# Fails if the cmd/node build graph imports anything outside the donor-safe
# allowlist. DENY-BY-DEFAULT over ALL non-stdlib deps (first-party AND
# third-party): a heavy/risky transitive dep is a violation just like an
# operator-only package. Stdlib is filtered out via go list's .Standard flag.
# Test deps (testify, etc.) do NOT appear in `go list -deps ./cmd/node`, so they
# need no allowlisting. This is the load-bearing P2-M1 boundary gate.
set -euo pipefail

MOD="github.com/nova-archive/nova"
# Donor-safe runtime roots. Adding an entry is a deliberate, reviewed act.
ALLOWED=(
  "$MOD/cmd/node"
  "$MOD/internal/secret"
  "$MOD/internal/node"
  "$MOD/internal/federation/wire"
  "gopkg.in/yaml.v3"   # donor config parsing — the only third-party runtime dep
)

deps="$(go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./cmd/node)"

violations=()
while IFS= read -r p; do
  [ -z "$p" ] && continue
  ok=0
  for a in "${ALLOWED[@]}"; do
    case "$p" in "$a"|"$a"/*) ok=1; break ;; esac
  done
  [ "$ok" -eq 0 ] && violations+=("$p")
done <<< "$deps"

if [ "${#violations[@]}" -ne 0 ]; then
  echo "FAIL: cmd/node imports non-allowlisted package(s):" >&2
  printf '  %s\n' "${violations[@]}" >&2
  exit 1
fi
echo "OK: cmd/node dependency boundary clean"
```

- [ ] **Step 2: Make it executable and run it**

```bash
chmod +x scripts/check_node_deps.sh
./scripts/check_node_deps.sh
```
Expected: `OK: cmd/node dependency boundary clean`.

- [ ] **Step 3: Prove the gate fails red (demonstrate, then revert — do NOT commit the violation)**

```bash
# Temporarily add an operator-only import to cmd/node/main.go, e.g.:
#   _ "github.com/nova-archive/nova/internal/db"
./scripts/check_node_deps.sh ; echo "exit=$?"   # expect FAIL + exit=1
git checkout -- cmd/node/main.go                # revert the violation
./scripts/check_node_deps.sh                    # expect OK again
```
Expected: FAIL (exit 1) with `internal/db` listed, then OK after revert.

- [ ] **Step 4: Add the `node-deps-check` Makefile target** — append to the node section (see Task 8 for the full block; add this line):

```makefile
node-deps-check:
	./scripts/check_node_deps.sh
```

- [ ] **Step 5: Commit**

```bash
git add scripts/check_node_deps.sh Makefile
git commit -s -m "feat(ci): deny-by-default cmd/node dependency-boundary gate (P2-M1)" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 8: Split Dockerfiles + minimal donor image + Makefile node targets

**Files:**
- Rename: `docker/Dockerfile` → `docker/coordinator.Dockerfile`
- Create: `docker/node.Dockerfile`, `scripts/check_node_image.sh`
- Modify: `docker/docker-compose.yml:77`, `Makefile`

- [ ] **Step 1: Rename the coordinator Dockerfile (contents unchanged) and re-point references**

```bash
git mv docker/Dockerfile docker/coordinator.Dockerfile
```
Then edit `docker/docker-compose.yml:77` `dockerfile: docker/Dockerfile` → `dockerfile: docker/coordinator.Dockerfile`, and the `Makefile` `docker-build` target `-f docker/Dockerfile` → `-f docker/coordinator.Dockerfile`.

- [ ] **Step 2: Write `docker/node.Dockerfile`** (CGO-off pure-Go, distroless static nonroot — CA certs included, no curl/libvips/node)

```dockerfile
# syntax=docker/dockerfile:1

# ---- build: pure-Go donor binary (no cgo, no libvips) ----
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/nova-node ./cmd/node

# ---- runtime: distroless static (ships CA certs + nonroot user; no shell, no curl) ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/nova-node /usr/local/bin/nova-node
USER nonroot:nonroot
# The binary checks itself; the image needs no curl/wget.
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD ["/usr/local/bin/nova-node", "--healthcheck", "--config", "/etc/nova/node.yaml"]
ENTRYPOINT ["/usr/local/bin/nova-node"]
CMD ["--config", "/etc/nova/node.yaml"]
```

- [ ] **Step 3: Write `scripts/check_node_image.sh`** (forbidden-inventory scan over the exported rootfs)

```bash
#!/usr/bin/env bash
# Fails if the donor image contains operator-only artifacts. Exports the image
# rootfs and greps the file list for forbidden patterns. Run after the image is
# built (make node-image).
set -euo pipefail
IMAGE="${1:-nova-node:dev}"

forbidden='libvips|libvips-dev|/novactl|/migrate|/coordinator|node_modules|/usr/bin/curl|/usr/bin/wget|master-key'

cid="$(docker create "$IMAGE")"
trap 'docker rm -f "$cid" >/dev/null 2>&1 || true' EXIT
listing="$(docker export "$cid" | tar -t 2>/dev/null)"

if hits="$(printf '%s\n' "$listing" | grep -E "$forbidden" || true)"; [ -n "$hits" ]; then
  echo "FAIL: donor image contains forbidden artifact(s):" >&2
  printf '  %s\n' "$hits" >&2
  exit 1
fi
echo "OK: donor image inventory clean"
```

- [ ] **Step 4: Add the Makefile node target block** (append after the `docker-build` target)

```makefile
.PHONY: node-build node-validate node-deps-check node-image node-image-inventory node-sbom

node-build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/nova-node ./cmd/node

# Runs the binary's validate behavior over good + malformed fixtures (table-driven).
node-validate:
	$(GOTESTV) ./cmd/node/... ./internal/node/config/... -count=1

node-deps-check:
	./scripts/check_node_deps.sh

node-image:
	docker build -f docker/node.Dockerfile -t nova-node:dev .

node-image-inventory: node-image
	./scripts/check_node_image.sh nova-node:dev

# Local SBOM (requires syft on PATH). CI uses the same tool on the built image.
node-sbom: node-image
	mkdir -p dist
	syft nova-node:dev -o spdx-json=dist/nova-node.sbom.spdx.json
```

- [ ] **Step 5: Verify the split builds independently and the inventory is clean**

```bash
make docker-build       # coordinator image still builds via the renamed Dockerfile
chmod +x scripts/check_node_image.sh
make node-image-inventory
```
Expected: coordinator image builds; `OK: donor image inventory clean`.

- [ ] **Step 6: Confirm the coordinator smoke path is unaffected by the rename**

```bash
grep -n 'docker/coordinator.Dockerfile' docker/docker-compose.yml Makefile
make smoke   # if Docker is available; else confirm scripts/smoke.sh uses compose (no direct Dockerfile ref)
```
Expected: both references updated; smoke (which drives `docker/docker-compose.yml`) is green.

- [ ] **Step 7: Commit**

```bash
git add docker/ Makefile scripts/check_node_image.sh
git commit -s -m "build(node): split coordinator/node Dockerfiles + minimal distroless donor image + inventory scan (P2-M1)" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 9: `deploy/donor/` — donor compose + annotated config

**Files:**
- Create: `deploy/donor/node.yaml.example`, `deploy/donor/compose.yaml`

- [ ] **Step 1: Write `deploy/donor/node.yaml.example`** (annotated; placeholder paths)

```yaml
# nova-node (donor) configuration. Copy to node.yaml and edit.
# All secret material is referenced by PATH; this file holds no secret bytes.

# Coordinator federation endpoint (reachable over the Nebula overlay from M2).
coordinator_url: https://coordinator.nebula.example:8443

# Two-cert model: the Nebula cert authorizes the overlay; the federation cert
# authorizes the HTTP API. Mount these read-only.
federation_cert_path: /etc/nova/federation.crt
federation_key_path:  /run/secrets/nova_node_federation_key
nebula_cert_path:     /etc/nebula/node.crt
nebula_key_path:      /run/secrets/nebula_key

# Private-IPFS swarm key (shared with the operator's swarm).
swarm_key_path: /run/secrets/ipfs_swarm_key

# Where replica ciphertext is stored (created if absent).
storage_dir: /var/lib/nova-node/data

# Authoritative daily egress budget (bytes/day). The donor refuses work beyond
# this regardless of coordinator scheduling (D11). 50 GiB shown.
bandwidth_budget_bytes_per_day: 53687091200

# Operator-declared anti-affinity hints (informational until operator-verified
# at the coordinator, D8).
failure_domain:
  provider: example-vps
  asn: "64500"
  region: us-east

# Health endpoint bind address (loopback by default; the federation listener
# binds the Nebula interface from M2).
health_listen_addr: 127.0.0.1:9100
```

- [ ] **Step 2: Write `deploy/donor/compose.yaml`** (Nebula sidecar + nova-node, no published ports, M14 hardening floors)

```yaml
# Donor deployment — NOT a profile in the operator compose (different actor,
# trust, secrets, ports, storage). No published ports: all federation traffic
# is mTLS over the Nebula overlay. M1 stands up the shape; live federation is M2+.
name: nova-donor

services:
  nebula:
    # Nebula runs as a SIDECAR so nova-node needs no NET_ADMIN. Pin a digest in
    # production (see docs/quickstart/donor.md). Config/cert wiring + a real
    # readiness check land in M2 — no healthcheck here yet (the image's probe
    # command and config are not established until then; gating nova-node on a
    # failing probe would wedge it).
    image: nebulaoss/nebula:latest
    cap_add: ["NET_ADMIN"]
    devices: ["/dev/net/tun:/dev/net/tun"]
    volumes:
      - ./nebula:/etc/nebula:ro
    restart: unless-stopped

  nova-node:
    build:
      context: ../..
      dockerfile: docker/node.Dockerfile
    image: nova-node:dev
    # Share the Nebula network namespace so the donor binds the overlay address.
    network_mode: "service:nebula"
    # Start-order only (no health gate yet — see the nebula service note). M2
    # adds a real Nebula readiness condition.
    depends_on:
      - nebula
    read_only: true
    tmpfs:
      - /tmp
    security_opt:
      - no-new-privileges:true
    cap_drop: ["ALL"]
    volumes:
      - ./node.yaml:/etc/nova/node.yaml:ro
      - ./secrets:/run/secrets:ro
      - node-data:/var/lib/nova-node/data
    restart: unless-stopped
    # HEALTHCHECK is defined in the image (nova-node --healthcheck).

volumes:
  node-data:
```

- [ ] **Step 3: Validate the example config with the built binary**

```bash
# The example uses placeholder paths that don't exist locally, so validation is
# expected to FAIL on the first missing path — proving the refuse-to-start floor.
make node-build
./bin/nova-node --validate --config deploy/donor/node.yaml.example ; echo "exit=$? (non-zero expected: placeholder paths absent)"
```
Expected: non-zero exit naming the first missing `*_path` (the floor works; the table-driven tests in Task 4/6 cover the valid case with real fixtures).

- [ ] **Step 4: Commit**

```bash
git add deploy/donor/
git commit -s -m "build(node): deploy/donor compose (Nebula sidecar, no ports, hardened) + annotated node.yaml.example (P2-M1)" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 10: CI supply-chain pipeline + donor docs stub

**Files:**
- Modify: `.github/workflows/ci.yml`
- Create: `docs/quickstart/donor.md`

- [ ] **Step 1: Add the three donor jobs to `.github/workflows/ci.yml`** (append under `jobs:`; mirror the existing `docker-build` job's checkout/buildx style)

```yaml
  donor-deps-boundary:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - name: cmd/node dependency boundary
        run: make node-deps-check

  donor-build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: docker/setup-buildx-action@v3
      - name: Build donor image (no push)
        run: make node-image
      - name: Forbidden-inventory scan
        run: make node-image-inventory
      - name: Generate SBOM (syft)
        uses: anchore/sbom-action@v0
        with:
          image: nova-node:dev
          format: spdx-json
          output-file: nova-node.sbom.spdx.json
      - uses: actions/upload-artifact@v4
        with:
          name: nova-node-sbom
          path: nova-node.sbom.spdx.json

  # Pushes the donor image BY DIGEST to GHCR and signs that digest with cosign
  # keyless/OIDC. Trusted ref only: never on pull requests (untrusted refs must
  # not mint signatures). The local-only workflow never pushes, so this is
  # exercised the first time CI runs on main.
  donor-sbom-sign:
    runs-on: ubuntu-latest
    needs: donor-build
    if: github.event_name == 'push' && github.ref == 'refs/heads/main'
    permissions:
      contents: read
      id-token: write
      packages: write
      attestations: write
    steps:
      - uses: actions/checkout@v4
      - uses: docker/setup-buildx-action@v3
      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - name: Build + push donor image by digest
        id: push
        uses: docker/build-push-action@v6
        with:
          context: .
          file: docker/node.Dockerfile
          push: true
          tags: ghcr.io/nova-archive/nova-node:sha-${{ github.sha }}
      - uses: sigstore/cosign-installer@v3
      - name: Sign the image digest (keyless)
        run: cosign sign --yes ghcr.io/nova-archive/nova-node@${{ steps.push.outputs.digest }}
      - name: Attest provenance to the digest
        uses: actions/attest-build-provenance@v1
        with:
          subject-name: ghcr.io/nova-archive/nova-node
          subject-digest: ${{ steps.push.outputs.digest }}
          push-to-registry: true
      # The SBOM must describe the EXACT signed digest, not the local :dev image
      # donor-build scanned. Generate + cosign-attest it against the digest so
      # "signed image + SBOM for that digest" is actually true.
      - uses: anchore/sbom-action/download-syft@v0
      - name: SBOM for the pushed digest + attest
        run: |
          DIGEST_REF=ghcr.io/nova-archive/nova-node@${{ steps.push.outputs.digest }}
          syft "$DIGEST_REF" -o spdx-json=nova-node.sbom.spdx.json
          cosign attest --yes --type spdxjson \
            --predicate nova-node.sbom.spdx.json "$DIGEST_REF"
```

- [ ] **Step 2: Write `docs/quickstart/donor.md`** (release-trust note stub; full walkthrough is P2-M7)

```markdown
# Running a Nova donor node (`nova-node`)

> **Status: P2-M1 stub.** The full volunteer walkthrough — digest pinning,
> `cosign verify`, Nebula enrollment, and operational runbooks — lands in
> **P2-M7**. This page records only the release-trust model.

## Release trust

`nova-node` images are published to `ghcr.io/nova-archive/nova-node` and signed
with **cosign keyless (GitHub OIDC)**; each image carries an SBOM and a build
provenance attestation. There is **one** trust path: keyless signatures. Nova
does not publish a local-key signing path.

In production, **pin a digest, not a tag**, and verify the signature before
running a privileged network daemon (the exact `cosign verify` invocation +
identity/issuer policy is documented in the P2-M7 walkthrough):

    docker pull ghcr.io/nova-archive/nova-node@sha256:<digest>
    cosign verify ghcr.io/nova-archive/nova-node@sha256:<digest> ...

## What M1 ships

A minimal, dependency-boundary-enforced donor binary that loads + validates
`node.yaml` and serves a loopback health endpoint. **No federation yet** —
registration, transport, replication, healing, and audits arrive in M2–M7.
```

- [ ] **Step 3: Validate the workflow YAML parses**

```bash
python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/ci.yml')); print('ci.yml OK')"
```
Expected: `ci.yml OK`.

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/ci.yml docs/quickstart/donor.md
git commit -s -m "ci(node): donor-deps-boundary + donor-build(SBOM+inventory) + keyless donor-sbom-sign; donor docs stub (P2-M1)" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 11: Milestone close — full build/test/lint/link sweep + roadmap

**Files:**
- Modify: `docs/ROADMAP.md` (P2-M1 row → ✅)

- [ ] **Step 1: Full verification sweep**

```bash
go build ./... && go vet ./...
make test-unit
make node-deps-check
make node-validate
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml')); print('ci.yml OK')"
python3 scripts/check_doc_links.py docs    # expect 0 broken (the M1 plan now exists)
make migrations-frozen                     # still green: M1 added no migration
# Docker/syft-dependent (run locally if available; otherwise CI covers them):
make node-image-inventory                  # donor image builds + forbidden-inventory clean
make node-sbom                             # local SBOM (skip if syft not installed)
make docker-build                          # coordinator image still builds via the renamed Dockerfile
make lint                                  # if golangci-lint is installed locally (else CI-only)
```
Expected: Go build/vet/test green; `node-deps-check` and `node-validate` pass; `ci.yml OK`; link checker reports 0 broken; `migrations-frozen` green; the donor image builds with a clean inventory and the coordinator image still builds (Docker-dependent steps may be CI-only on hosts without Docker/syft).

- [ ] **Step 2: Update the `docs/ROADMAP.md` Phase-2 table** — change the `P2-M1` row's status to ✅ with the tag, design, and plan links (mirror the M0.x rows' format), summarizing: `cmd/node` + `internal/node/*` + `internal/federation/wire` + `internal/secret` extraction; deny-by-default boundary gate; split Dockerfiles + distroless donor image + inventory scan; `deploy/donor/`; CI SBOM + keyless signing; no schema/migration; no live federation.

- [ ] **Step 3: Commit the roadmap**

```bash
git add docs/ROADMAP.md
git commit -s -m "docs(m1): mark P2-M1 build/repo-separation complete (P2-M1)" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

- [ ] **Step 4: Finish the branch per the milestone workflow** — invoke `superpowers:finishing-a-development-branch`: local fast-forward merge into `main` + annotated tag `p2-m1-build-repo-separation`; **no remote push**.

---

## Self-review notes (spec coverage)

- **Decisions 1 (shared leaf + donor-local config):** Task 1 (extract `internal/secret`), Task 4 (donor-local `internal/node/config`), Task 7 (boundary excludes `internal/config`/`internal/api`). ✓
- **Decision 2 (CI keyless pipeline, no local keypair, docs at M7):** Task 10 (keyless-only `donor-sbom-sign`, trusted-ref condition, permissions; docs stub points to M7). ✓
- **Path-based `node.yaml` + shallow validation (review #1, #9):** Task 4 (`*_path`, readable/not-dir, no PEM parse, storage_dir create). ✓
- **Canonical token + no-replay Verify (review #2, #3):** Task 2 (format defined; `Verify` checks signature over received segment + window + structure; test list has missing-jti/malformed-token, no replay). ✓
- **Concrete GHCR/ref/permissions (review #4):** Task 10. ✓
- **Exact boundary mechanics + disallow all `internal/api` (review #5):** Task 7 (`go list -deps -f '{{if not .Standard}}…'`, deny-by-default first-party). ✓
- **Ultra-minimal third-party (review #6):** Task 2 (ids/cid are opaque strings; no go-cid/uuid). ✓
- **Self-healthcheck, no curl (review #7):** Task 6 (`--healthcheck`), Task 8 (image HEALTHCHECK invokes the binary; distroless static). ✓
- **Verified image inventory (review #8):** Task 8 (`scripts/check_node_image.sh`) + Task 10 (CI step; demonstrated-fail noted in design). ✓
- **No migration:** asserted in Task 11 (`migrations-frozen` stays green). ✓

### Execution-review hardening (2026-06-15)

- **No minting API in the donor-imported `wire`** — `SignToken` removed; only
  format primitives (`SigningInput`/`AssembleToken`) + `Verify` are exported.
  Signing-with-private-key (minting) is coordinator-only `internal/federation/tokens`
  (M4); tests sign locally with a throwaway key. (Task 2)
- **Boundary gate is deny-by-default for third-party too** — not just first-party;
  the only allowed third-party runtime dep is `gopkg.in/yaml.v3`. (Task 7)
- **`serve()` fails fast on bind errors** via synchronous `net.Listen` + select on
  signal/server-error, with a regression test. (Task 6)
- **SBOM is generated + cosign-attested against the exact pushed digest**, so
  "signed image + SBOM for that digest" holds. (Task 10)
- **`Verify` is stricter** — protocol == `fed/v1`, `Generation > 0`, `MaxBytes > 0`,
  `NotBefore < NotAfter`; **`bandwidth.Take` rejects `n <= 0`**; **`MemStore` honors
  jti expiry**; **config validates** coordinator_url shape, `health_listen_addr`
  host:port, and storage-dir writability. (Tasks 2/3/4/5)
- **Nebula compose gate weakened to start-order only** (no phantom healthcheck that
  would wedge `nova-node`); real readiness is M2. (Task 9)
- **Every commit command carries the `Co-Authored-By` trailer**; the final sweep
  also runs image-inventory, SBOM, coordinator build, lint, and workflow-YAML parse.
  (Tasks 1–11)

---

## Execution handoff

Recommended: **subagent-driven** — dispatch a fresh subagent per task with a two-stage review between tasks (the boundary gate in Task 7 and the `internal/secret` refactor in Task 1 especially benefit from a clean reviewer). Alternatively inline execution with checkpoints. Start only on the operator's go.
