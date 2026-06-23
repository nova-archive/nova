# P2-M4 v1 Opaque Replication Vertical Slice — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the first donor replica real — a manually assigned CID flows
`assign → coordinator-as-source signed grant → donor fetch → deterministic
re-import + root-CID verify → local pin → persist → production ack`, so an
`ack` first means *verified local storage* over the v1 envelope.

**Architecture:** The coordinator mints short-lived Ed25519 repair tokens
(coordinator-only `internal/federation/tokens`) and serves `GET /fed/v1/blob/{cid}`
from its origin Kubo on the M2 Nebula-bound mTLS listener; tokens are minted
*per-serve* into `PinChange.Source` (never persisted). The donor — a **fetch-only**
client in M4 — fetches bounded ciphertext, re-imports it through a hardened Kubo
**sidecar** (loopback HTTP API; the donor never embeds Kubo), compares the
computed root CID, persists a durable "verified, ack-pending" record **before**
acking, and drives the existing M3 `ack`/`fail` state machine. No schema change,
no donor-backed reads, no pruning, no donor-as-source repair (those are P2-M4.1 /
P2-M5).

**Tech Stack:** Go (`crypto/ed25519`, `log/slog`, `net/http`), Postgres via
`sqlc`/`pgx` (read-only query addition only — **no migration**), Kubo HTTP API
(`/api/v0/{block/put,add,pin/ls,pin/rm,repo/stat}`, branched to match the embedded
backend), `go-cid` for canonical root-CID equality, `gopkg.in/yaml.v3` (donor
config), `testify` (tests).

## Global Constraints

- **No schema migration.** M4 adds at most read-only sqlc queries; it modifies
  **no** file under `internal/db/migrations/`. `scripts/check-migrations-frozen.sh`
  (`migrations-frozen`) MUST stay green.
- **Donor dependency boundary.** `go list -deps ./cmd/node` is deny-by-default
  (`scripts/check_node_deps.sh`, CI `donor-deps-boundary`). The donor build graph
  MUST NOT import `internal/ipfs` (embedded Kubo), `internal/db`,
  `internal/masterkey`, `internal/moderation`, `internal/auth`, `internal/setup`,
  `internal/api`, `nova-image`, or `pkg/coordinator`. New allowlist entries are a
  deliberate, reviewed act and MUST be the minimum.
- **`internal/federation/wire` stays dependency-free** (donor-safe; no
  operator-only imports; no `go-cid`/`uuid` in `wire`).
- **Donors are donor-blind** (`T1.26`): the coordinator stays the only
  decrypt/serve point; M4 moves only **opaque v1 ciphertext** to donors.
- **`ack` ⇒ verified local storage** (D4): never ack without
  fetch+re-import+root-CID-match+durable-persist.
- **Tooling:** `gofmt` only files you touch; `golangci-lint` is CI-only; sqlc via
  `make sqlc-generate`. go.mod is Go 1.26.x.
- **Identity from the verified mTLS cert only** (D-cap) — never self-asserted JSON.
- **Naming:** the operator is **Bug** in prose/docs.

## Preconditions (exist from M0–M3; do not re-create)

- `internal/federation/wire`: `Claims`, `SigningInput`, `AssembleToken`, `Verify`
  (no mint), `ProtocolV1`, `CapPinChangeLog`/`CapSnapshot`/`CapRepairStream`,
  `ChangeSource` (nil in M3), `PinChange.Source`, `HeartbeatResponse.RepairTokenPublicKey`
  (empty since M2), `Ack`, `Fail` + `NormalizeFailReason` + the full reason domain.
- `internal/federation/coordinator`: `Server{q,cfg}`, `mux()`, `authNode`,
  `handleChanges`/`handleSnapshot`/`handleAck`/`handleFail`, `writeJSON`/`writeError`,
  `pgUUIDFrom`, the `AssignPin`/`UnpinPin` seam, retention goroutine.
- `internal/ipfs`: `Backend` interface (`AddDeterministic`, `Get`, `Has`, `Pin`,
  `BlockGet`, …), `EmbeddedBackend`, and `importrules.go` (pure constants +
  `ShouldUseRawCodec`, **no Kubo imports**) inside `package ipfs`.
- `internal/node`: `agent.Agent`/`Client`/`HTTPClient`, `state.FileStore`/`FileAssignmentStore`,
  `bandwidth.Bucket`, `config.Config`, `transfer` (M1 stub `Verifier`).
- `internal/secret`: `ResolveSecret(envKey, envFileKey, defaultMountPath) (value, Source, error)`.
- `internal/config`: `Federation` struct + `Validate`/`FederationTimers`/`FederationRetention`.
- `pin_assignments` state machine + `acked_at` + `AckPinAssignment`/`FailPinAssignment`
  queries (migration `0012`). `blobs.byte_size bigint NOT NULL`.
- `novactl pin assign|unpin|list` (the "verified holders" line is currently empty).
- Tests run against a throwaway Postgres via the existing harness used by
  `internal/federation/coordinator/*_test.go`.

## File structure

**Shared (donor-safe):**
- `internal/ipfs/importspec/importspec.go` *(NEW)* — the deterministic-import
  constants + `ShouldUseRawCodec`, moved out of `package ipfs` so both binaries
  share identical params without the donor importing embedded Kubo.
- `internal/ipfs/importrules.go` *(MODIFY)* — re-export from `importspec` for
  back-compat (coordinator call sites unchanged).
- `internal/federation/wire/messages.go` *(MODIFY)* — add `CapBlobTransfer`;
  pubkey base64url helpers.

**Coordinator (operator-only):**
- `internal/federation/tokens/tokens.go` *(NEW)* — Ed25519 `Signer` (mint),
  seed load, `ReservedCoordinatorSourceID`.
- `internal/federation/coordinator/blob.go` *(NEW)* — `GET /fed/v1/blob/{cid}` +
  the `jtiCache` replay seam.
- `internal/federation/coordinator/pins.go` *(MODIFY)* — `Source` population in
  `handleChanges`.
- `internal/federation/coordinator/handlers.go` *(MODIFY)* — heartbeat pubkey;
  register requires `blob-transfer/v1`.
- `internal/federation/coordinator/server.go` *(MODIFY)* — `Backend` + `Signer`
  fields; mount blob route; `SourceBootTime`.
- `internal/db/queries/blobs.sql` *(MODIFY)* — `GetBlobByteSize :one` (read-only).
- `internal/config/{types.go,federation.go}` *(MODIFY)* — repair-token config.
- `cmd/coordinator/main.go` *(MODIFY)* — load seed; pass `Backend`+`Signer`.

**Donor (`cmd/node`):**
- `internal/node/ipfsclient/client.go` *(NEW)* — Kubo sidecar HTTP client.
- `internal/node/transfer/transfer.go` *(REPLACE stub)* — real `Verifier` +
  `SourceFetcher`.
- `internal/node/state/progress.go` *(NEW)* — durable verify/ack progress.
- `internal/node/agent/client.go` *(MODIFY)* — `Ack`/`Fail`; HTTP `SourceFetcher`.
- `internal/node/agent/agent.go` *(MODIFY)* — fetch→verify→pin→persist→ack loop +
  startup reconcile.
- `internal/node/config/config.go` *(MODIFY)* — `storage_max_bytes`, kubo addr.
- `cmd/node/main.go` *(MODIFY)* — wire sidecar client, fetcher, transfer.

**CI / docs:**
- `scripts/check_node_deps.sh` *(MODIFY)* — reviewed allowlist extension.
- `docs/specs/FEDERATION_PROTOCOL.md`, `docs/THREAT_MODEL.md`, `docs/ROADMAP.md`,
  the master design table, `docs/quickstart/donor.md`,
  `docs/VOLUNTEER_DEPLOYMENT_GUIDANCE.md` *(MODIFY)*.

---

## Task 1: `wire` — `blob-transfer/v1` capability + pubkey encoding

**Files:**
- Modify: `internal/federation/wire/messages.go`
- Modify: `internal/federation/wire/token.go`
- Test: `internal/federation/wire/messages_test.go`, `internal/federation/wire/token_test.go`

**Interfaces:**
- Produces: `wire.CapBlobTransfer = "blob-transfer/v1"`;
  `wire.EncodePublicKey(ed25519.PublicKey) string` and
  `wire.DecodePublicKey(string) (ed25519.PublicKey, error)` (base64url raw 32 bytes).

- [ ] **Step 1: Write the failing test**

```go
// in messages_test.go
func TestCapBlobTransferConst(t *testing.T) {
	if wire.CapBlobTransfer != "blob-transfer/v1" {
		t.Fatalf("got %q", wire.CapBlobTransfer)
	}
}

// in token_test.go
func TestPublicKeyRoundTrip(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	s := wire.EncodePublicKey(pub)
	got, err := wire.DecodePublicKey(s)
	if err != nil || !got.Equal(pub) {
		t.Fatalf("round-trip failed: %v", err)
	}
	if _, err := wire.DecodePublicKey("not-base64-!!"); err == nil {
		t.Fatal("expected decode error")
	}
	if _, err := wire.DecodePublicKey(wire.EncodePublicKey(pub)[:10]); err == nil {
		t.Fatal("expected length error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/federation/wire/ -run 'CapBlobTransfer|PublicKeyRoundTrip' -v`
Expected: FAIL — `undefined: wire.CapBlobTransfer` / `wire.EncodePublicKey`.

- [ ] **Step 3: Add the constant and helpers**

In `messages.go`, extend the capability block:

```go
const (
	CapPinChangeLog   = "pin-change-log/v1"
	CapSnapshot       = "snapshot/v1"
	CapBlobTransfer   = "blob-transfer/v1" // M4: donor can fetch source-bearing assignments, verify, pin, ack
	CapRepairStream   = "repair-stream/v1" // RESERVED for M5 donor-as-source; not advertised in M4
	CapAuditBlockHash = "audit-block-hash/v1"
)

// CoordinatorSourceID is the reserved synthetic source identity for
// coordinator-as-source transfers (D-M4-2). It is a protocol constant shared by
// both sides — defined here in the dependency-free wire package so the donor
// (which cannot import the operator-only tokens package) can reference it as
// Ack.FetchedFromNodeID. It is NOT a nodes row. Fixed forever.
const CoordinatorSourceID = "00000000-0000-0000-0000-000000000001"
```

In `token.go`, add (reuse the existing `b64 = base64.RawURLEncoding`):

```go
// EncodePublicKey renders an Ed25519 public key as base64url(raw 32 bytes) for
// delivery to donors via HeartbeatResponse.RepairTokenPublicKey (D-M4-7).
func EncodePublicKey(pub ed25519.PublicKey) string { return b64.EncodeToString(pub) }

// DecodePublicKey parses the wire form back into an Ed25519 public key.
func DecodePublicKey(s string) (ed25519.PublicKey, error) {
	raw, err := b64.DecodeString(s)
	if err != nil {
		return nil, ErrMalformedToken
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, ErrMalformedToken
	}
	return ed25519.PublicKey(raw), nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/federation/wire/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/federation/wire/messages.go internal/federation/wire/token.go
git add internal/federation/wire/
git commit -m "feat(p2-m4): wire blob-transfer/v1 capability + Ed25519 pubkey encoding (P2-M4)"
```

---

## Task 2: Extract `internal/ipfs/importspec` (donor-safe import params)

**Files:**
- Create: `internal/ipfs/importspec/importspec.go`
- Modify: `internal/ipfs/importrules.go`
- Test: `internal/ipfs/importspec/importspec_test.go`

**Interfaces:**
- Produces: `importspec.RawCodecThresholdBytes`, `ChunkerSizeBytes`, `ChunkerSpec`,
  `HashAlg`, `CodecRaw`, `CodecDagPB`, `MaxLinkCount`, `ShouldUseRawCodec(int64) bool`.
- The donor imports **only** `internal/ipfs/importspec` (never `internal/ipfs`).

- [ ] **Step 1: Write the failing test**

```go
package importspec_test

import (
	"testing"
	"github.com/nova-archive/nova/internal/ipfs/importspec"
)

func TestImportSpecValues(t *testing.T) {
	if importspec.ChunkerSpec != "size-262144" || importspec.ChunkerSizeBytes != 262144 {
		t.Fatal("chunker drift")
	}
	if importspec.RawCodecThresholdBytes != 1<<20 {
		t.Fatal("threshold drift")
	}
	if !importspec.ShouldUseRawCodec(1<<20) || importspec.ShouldUseRawCodec((1<<20)+1) {
		t.Fatal("raw threshold boundary wrong")
	}
	if importspec.HashAlg != "sha2-256" || importspec.CodecRaw != "raw" || importspec.CodecDagPB != "dag-pb" {
		t.Fatal("codec/hash drift")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ipfs/importspec/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Create `internal/ipfs/importspec/importspec.go`**

Move the constants + function from `importrules.go` verbatim:

```go
// Package importspec holds the deterministic IPFS import parameters
// (IPFS_IMPORT_RULES.md, Tier-1). It is dependency-free so BOTH the operator's
// embedded Kubo (internal/ipfs) and the donor's Kubo-sidecar client
// (internal/node/ipfsclient) share identical params and therefore produce
// bit-identical root CIDs. It MUST NOT import Kubo or any operator-only package.
package importspec

const (
	RawCodecThresholdBytes int64 = 1 << 20 // 1 MiB
	ChunkerSizeBytes       int64 = 262144  // 256 KiB
	ChunkerSpec                  = "size-262144"
	HashAlg                      = "sha2-256"
	CodecRaw                     = "raw"
	CodecDagPB                   = "dag-pb"
	MaxLinkCount                 = 174
)

// ShouldUseRawCodec reports whether an envelope of envelopeSize bytes imports via
// the single-block raw-codec shortcut (true) or the dag-pb chunked path (false).
func ShouldUseRawCodec(envelopeSize int64) bool { return envelopeSize <= RawCodecThresholdBytes }
```

- [ ] **Step 4: Re-point `internal/ipfs/importrules.go`**

Replace its body with aliases so coordinator call sites are unchanged:

```go
package ipfs

import "github.com/nova-archive/nova/internal/ipfs/importspec"

// Deterministic import params now live in the dependency-light importspec
// sub-package (so the donor can share them without embedded Kubo). Re-exported
// here for the coordinator's existing call sites.
const (
	RawCodecThresholdBytes = importspec.RawCodecThresholdBytes
	ChunkerSizeBytes       = importspec.ChunkerSizeBytes
	ChunkerSpec            = importspec.ChunkerSpec
	HashAlg                = importspec.HashAlg
	CodecRaw               = importspec.CodecRaw
	CodecDagPB             = importspec.CodecDagPB
	MaxLinkCount           = importspec.MaxLinkCount
)

// ShouldUseRawCodec is re-exported from importspec.
func ShouldUseRawCodec(envelopeSize int64) bool { return importspec.ShouldUseRawCodec(envelopeSize) }
```

- [ ] **Step 5: Verify nothing else broke**

Run: `go build ./... && go test ./internal/ipfs/... -run 'ImportSpec|ImportRules' -v`
Expected: PASS; existing `importrules_test.go` still green (constants unchanged).

- [ ] **Step 6: Commit**

```bash
gofmt -w internal/ipfs/importspec/importspec.go internal/ipfs/importrules.go
git add internal/ipfs/
git commit -m "refactor(p2-m4): extract dependency-light internal/ipfs/importspec (P2-M4)"
```

---

## Task 3: `internal/federation/tokens` — Ed25519 mint + seed load + reserved source id

**Files:**
- Create: `internal/federation/tokens/tokens.go`
- Test: `internal/federation/tokens/tokens_test.go`

**Interfaces:**
- Produces:
  - `tokens.ReservedCoordinatorSourceID` (aliases `wire.CoordinatorSourceID`).
  - `tokens.LoadSigner(defaultPath string) (*Signer, error)` — resolves the seed
    via `secret.ResolveSecret`, using the operator-configured
    `federation.repair_signing_key_path` as the mount path (empty ⇒ the built-in
    `/run/secrets/...` default). Env / `_FILE` still override.
  - `tokens.NewSignerFromSeed([]byte) (*Signer, error)`.
  - `(*Signer).PublicKeyWire() string` (base64url raw 32 bytes).
  - `(*Signer).Mint(claims wire.Claims) (string, error)`.
- Consumes: `wire.SigningInput`, `wire.AssembleToken`, `wire.EncodePublicKey`.

- [ ] **Step 1: Write the failing test**

```go
package tokens_test

import (
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/federation/tokens"
	"github.com/nova-archive/nova/internal/federation/wire"
)

func newClaims(now time.Time) wire.Claims {
	return wire.Claims{
		JTI: "jti-1", AssignmentID: "a-1", Generation: 1, CID: "bafy-x",
		SourceNodeID: tokens.ReservedCoordinatorSourceID, DestNodeID: "node-1",
		NotBefore: now.Unix(), NotAfter: now.Add(5 * time.Minute).Unix(),
		MaxBytes: 1 << 20, ProtocolVersion: wire.ProtocolV1,
	}
}

func TestMintVerifyRoundTrip(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	s, err := tokens.NewSignerFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tok, err := s.Mint(newClaims(now))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := wire.DecodePublicKey(s.PublicKeyWire())
	if err != nil {
		t.Fatal(err)
	}
	got, err := wire.Verify(pub, tok, now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.AssignmentID != "a-1" || got.SourceNodeID != tokens.ReservedCoordinatorSourceID {
		t.Fatalf("claims mismatch: %+v", got)
	}
}

func TestMintRejectsBadSeed(t *testing.T) {
	if _, err := tokens.NewSignerFromSeed([]byte("short")); err == nil {
		t.Fatal("expected seed-length error")
	}
}

func TestVerifyRejectsTamperAndExpiry(t *testing.T) {
	seed := make([]byte, 32)
	s, _ := tokens.NewSignerFromSeed(seed)
	now := time.Now()
	tok, _ := s.Mint(newClaims(now))
	pub, _ := wire.DecodePublicKey(s.PublicKeyWire())
	if _, err := wire.Verify(pub, tok+"x", now); err == nil {
		t.Fatal("expected signature error on tamper")
	}
	if _, err := wire.Verify(pub, tok, now.Add(10*time.Minute)); err == nil {
		t.Fatal("expected expiry error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/federation/tokens/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Create `internal/federation/tokens/tokens.go`**

```go
// Package tokens is the coordinator-ONLY Ed25519 repair-token mint (D1). It holds
// the private signing key; donors only ever Verify (internal/federation/wire).
// This package MUST NEVER enter the cmd/node build graph.
package tokens

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/secret"
)

// ReservedCoordinatorSourceID aliases the shared protocol constant
// wire.CoordinatorSourceID (D-M4-2) so coordinator-side code reads naturally; the
// donor references wire.CoordinatorSourceID directly (it cannot import this
// operator-only package). It is NOT a nodes row; donors echo it as
// Ack.FetchedFromNodeID.
const ReservedCoordinatorSourceID = wire.CoordinatorSourceID

// Secret resolver coordinates for the repair-signing seed (base64url or hex,
// 32-byte Ed25519 seed). Keys never enter the DB (D-M4-7).
const (
	envKey           = "NOVA_FEDERATION_REPAIR_SIGNING_KEY"
	envFileKey       = "NOVA_FEDERATION_REPAIR_SIGNING_KEY_FILE"
	defaultMountPath = "/run/secrets/federation-repair-signing-key"
)

// Signer mints repair tokens. Construct via LoadSigner or NewSignerFromSeed.
type Signer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

// LoadSigner resolves the seed via the standard secret chain (env / _FILE /
// mount path) and derives the key. defaultPath is the operator-configured
// federation.repair_signing_key_path; empty selects the built-in mount default.
func LoadSigner(defaultPath string) (*Signer, error) {
	mountPath := defaultPath
	if mountPath == "" {
		mountPath = defaultMountPath
	}
	v, _, err := secret.ResolveSecret(envKey, envFileKey, mountPath)
	if err != nil {
		return nil, fmt.Errorf("tokens: load repair-signing key: %w", err)
	}
	seed, err := decodeSeed(strings.TrimSpace(v))
	if err != nil {
		return nil, err
	}
	return NewSignerFromSeed(seed)
}

func decodeSeed(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil && len(b) == ed25519.SeedSize {
		return b, nil
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == ed25519.SeedSize {
		return b, nil
	}
	return nil, errors.New("tokens: seed must be base64url or hex of 32 bytes")
}

// NewSignerFromSeed derives an Ed25519 keypair from a 32-byte seed.
func NewSignerFromSeed(seed []byte) (*Signer, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("tokens: seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return &Signer{priv: priv, pub: priv.Public().(ed25519.PublicKey)}, nil
}

// PublicKeyWire returns base64url(raw 32-byte public key) for HeartbeatResponse.
func (s *Signer) PublicKeyWire() string { return wire.EncodePublicKey(s.pub) }

// Mint signs claims into the wire token "signingInput.base64url(sig)".
func (s *Signer) Mint(c wire.Claims) (string, error) {
	in, err := wire.SigningInput(c)
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(s.priv, []byte(in))
	return wire.AssembleToken(in, sig), nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/federation/tokens/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/federation/tokens/tokens.go
git add internal/federation/tokens/
git commit -m "feat(p2-m4): coordinator Ed25519 repair-token mint + seed load (P2-M4)"
```

---

## Task 4: Coordinator config — repair-token fields

**Files:**
- Modify: `internal/config/types.go:99-115` (the `Federation` struct)
- Modify: `internal/config/federation.go`
- Test: `internal/config/federation_test.go`

**Interfaces:**
- Produces: `Federation.RepairTokenTTLSeconds`, `Federation.RepairSigningKeyPath`,
  `Federation.MaxTransferBytes`, `Federation.SourceNebulaAddr`, and
  `Federation.RepairTokenTTL() time.Duration` (default 300s, clamped 60–1800),
  `Federation.MaxTransfer() int64` (default = `max_blob_bytes` floor, else 100 MiB).

- [ ] **Step 1: Write the failing test**

```go
func TestRepairTokenTTLDefaultAndClamp(t *testing.T) {
	if d := (Federation{}).RepairTokenTTL(); d != 300*time.Second {
		t.Fatalf("default ttl: %v", d)
	}
	if d := (Federation{RepairTokenTTLSeconds: 5}).RepairTokenTTL(); d != 60*time.Second {
		t.Fatalf("low clamp: %v", d)
	}
	if d := (Federation{RepairTokenTTLSeconds: 9000}).RepairTokenTTL(); d != 1800*time.Second {
		t.Fatalf("high clamp: %v", d)
	}
}

func TestMaxTransferDefault(t *testing.T) {
	if n := (Federation{}).MaxTransfer(); n != 100*1024*1024 {
		t.Fatalf("default max transfer: %d", n)
	}
	if n := (Federation{MaxTransferBytes: 5 << 20}).MaxTransfer(); n != 5<<20 {
		t.Fatalf("explicit: %d", n)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run 'RepairTokenTTL|MaxTransfer' -v`
Expected: FAIL — undefined fields/methods.

- [ ] **Step 3: Extend the `Federation` struct (`types.go`)**

Add to the struct:

```go
	// M4: repair-token + transfer.
	RepairTokenTTLSeconds int    `yaml:"repair_token_ttl_seconds"`
	RepairSigningKeyPath  string `yaml:"repair_signing_key_path"`
	MaxTransferBytes      int64  `yaml:"max_transfer_bytes"`
	SourceNebulaAddr      string `yaml:"source_nebula_addr"`
```

- [ ] **Step 4: Add the accessors (`federation.go`)**

```go
// RepairTokenTTL returns the repair-token validity window (default 300s, clamped
// to the spec range 60..1800).
func (f Federation) RepairTokenTTL() time.Duration {
	s := f.RepairTokenTTLSeconds
	if s == 0 {
		s = 300
	}
	if s < 60 {
		s = 60
	}
	if s > 1800 {
		s = 1800
	}
	return time.Duration(s) * time.Second
}

// MaxTransfer is the hard cap on a single coordinator-as-source transfer (default
// 100 MiB).
func (f Federation) MaxTransfer() int64 {
	if f.MaxTransferBytes > 0 {
		return f.MaxTransferBytes
	}
	return 100 * 1024 * 1024
}
```

- [ ] **Step 5: Run tests + commit**

Run: `go test ./internal/config/ -v`
Expected: PASS.

```bash
gofmt -w internal/config/types.go internal/config/federation.go
git add internal/config/
git commit -m "feat(p2-m4): operator config for repair tokens + max transfer (P2-M4)"
```

---

## Task 5: `GetBlobByteSize` query (read-only; no migration)

**Files:**
- Modify: `internal/db/queries/blobs.sql` (add one `:one` query)
- Run: `make sqlc-generate`
- Test: covered by Task 6's handler test (no standalone DB test needed)

**Interfaces:**
- Produces: `gen.Queries.GetBlobByteSize(ctx, cid string) (int64, error)`.

> If an equivalent single-column read already exists (`grep -n "byte_size" internal/db/queries/*.sql`), reuse it and skip the add — but DO regenerate if you add SQL. This touches **no** migration file.

- [ ] **Step 1: Add the query**

Append to `internal/db/queries/blobs.sql`:

```sql
-- name: GetBlobByteSize :one
-- M4 coordinator-as-source preflight: the on-disk envelope size for max_bytes
-- enforcement before streaming (D-M4-3). Only `active` blobs are sourceable for
-- federation replication — quarantined / tombstoned / soft_deleted blobs MUST
-- NOT be served to donors (a no-row result becomes 404 blob_unavailable at the
-- endpoint, which is the correct refusal). `blobs.state` is the `blob_state`
-- enum (`active`, `quarantined`, `tombstoned`, `soft_deleted`, …).
SELECT byte_size FROM blobs WHERE cid = $1 AND state = 'active';
```

- [ ] **Step 2: Regenerate + verify frozen gate**

Run:
```bash
make sqlc-generate
bash scripts/check-migrations-frozen.sh
go build ./internal/db/...
```
Expected: codegen adds `GetBlobByteSize`; `migrations-frozen` prints OK (no migration changed); build passes.

- [ ] **Step 3: Commit**

```bash
git add internal/db/queries/blobs.sql internal/db/gen/
git commit -m "feat(p2-m4): GetBlobByteSize read query for source preflight (P2-M4)"
```

---

## Task 6: Coordinator `GET /fed/v1/blob/{cid}` source endpoint + replay seam

**Files:**
- Create: `internal/federation/coordinator/blob.go`
- Modify: `internal/federation/coordinator/server.go` (Server fields, mux route, SourceBootTime)
- Test: `internal/federation/coordinator/blob_test.go`

**Interfaces:**
- Consumes: `tokens.Signer` (verify uses its public key), `ipfs.Backend.Get`,
  `gen.Queries.GetBlobByteSize`, `wire.Verify`, `authNode`.
- Produces: `Server` gains `backend ipfs.Backend`, `signer *tokens.Signer`,
  `sourceBootTime time.Time`, `jti *jtiCache`; route `GET /fed/v1/blob/{cid}`.

- [ ] **Step 1: Write the failing test**

```go
package coordinator

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/federation/tokens"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// fakeBackend implements the narrow blobSource interface (Step 3): Get returns
// the canned bytes for its CID, or pgx-free "not found" for any other CID.
type fakeBackend struct {
	cid  string
	data []byte
}

func fakeBackendFor(cidStr string, data []byte) fakeBackend { return fakeBackend{cid: cidStr, data: data} }

func (f fakeBackend) Get(_ context.Context, c cid.Cid) (io.ReadCloser, error) {
	if c.String() != f.cid {
		return nil, errors.New("not found")
	}
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

// mkCID returns a valid canonical CIDv1(raw, sha2-256) string for data — the
// handler runs gocid.Decode(path) and 400s on a non-CID, so tests must use real
// CIDs (its String() round-trips to what fakeBackend stores).
func mkCID(t *testing.T, data []byte) string {
	t.Helper()
	mh, err := multihash.Sum(data, multihash.SHA2_256, -1)
	if err != nil {
		t.Fatal(err)
	}
	return cid.NewCidV1(cid.Raw, mh).String()
}

func TestBlobServeHappyPath(t *testing.T) {
	env := newFedTestEnv(t) // existing harness: registers a node, gives mTLS client + Server
	signer, _ := tokens.NewSignerFromSeed(make([]byte, 32))
	body := []byte("ciphertext-bytes")
	c := mkCID(t, body)
	env.insertBlob(t, c, int64(len(body))) // active blob row so GetBlobByteSize succeeds
	env.srv.SetSourceDeps(signer, fakeBackendFor(c, body), time.Now().Add(-time.Minute))

	tok, _ := signer.Mint(wire.Claims{
		JTI: "j1", AssignmentID: "a1", Generation: 1, CID: c,
		SourceNodeID: tokens.ReservedCoordinatorSourceID, DestNodeID: env.nodeID,
		NotBefore: time.Now().Add(-10 * time.Second).Unix(),
		NotAfter:  time.Now().Add(5 * time.Minute).Unix(),
		MaxBytes:  1 << 20, ProtocolVersion: wire.ProtocolV1,
	})
	resp := env.GET("/fed/v1/blob/"+c, map[string]string{"X-Nova-Repair-Token": tok})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != string(body) {
		t.Fatalf("body %q", got)
	}
}

func TestBlobRejectsWrongDest(t *testing.T)   { /* dest_node_id != caller → 403 source_unauthorized */ }
func TestBlobRejectsExpired(t *testing.T)     { /* not_after in past → 403 */ }
func TestBlobRejectsPreBootToken(t *testing.T){ /* not_before < sourceBootTime → 403 pre_boot */ }
func TestBlobRejectsReplay(t *testing.T)      { /* second use of same jti → 403 replay */ }
func TestBlobUnknownCID404(t *testing.T)      { /* missing origin → 404 blob_unavailable */ }
func TestBlobOversizeRejected(t *testing.T)   { /* byte_size > max_bytes → 413 before body */ }
```

> Fill the stubbed sub-tests using the same `newFedTestEnv` harness the existing
> `integration_test.go` / `pins_hardening_test.go` use (mTLS client + registered
> node). Assert the `wire.ErrorResponse.Code` for each rejection.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/federation/coordinator/ -run TestBlob -v`
Expected: FAIL — `SetSourceDeps` / route undefined.

- [ ] **Step 3: Add Server fields + route + boot time (`server.go`)**

Extend `Config` and `Server`:

```go
// blobSource is the narrow read surface the source endpoint needs (accept narrow
// interfaces): *ipfs.EmbeddedBackend satisfies it via its Get(ctx, cid.Cid).
// Using it keeps the federation Server testable with a tiny fake and avoids
// widening the operator-only ipfs import surface.
type blobSource interface {
	Get(ctx context.Context, c cid.Cid) (io.ReadCloser, error)
}

// in Config:
	RepairTokenTTL   time.Duration
	MaxTransferBytes int64
	SourceNebulaAddr string

// in Server:
	backend        blobSource
	signer         *tokens.Signer
	sourceBootTime time.Time
	jti            *jtiCache
```

Add a setter used at construction (and in tests) and register the route in `mux()`:

```go
// SetSourceDeps wires the coordinator-as-source dependencies (D-M4-2). Called by
// cmd/coordinator after constructing the embedded backend + signer.
func (s *Server) SetSourceDeps(signer *tokens.Signer, backend blobSource, bootTime time.Time) {
	s.signer = signer
	s.backend = backend
	s.sourceBootTime = bootTime
	if s.jti == nil {
		s.jti = newJTICache()
	}
}
```

In `mux()` add:

```go
	m.HandleFunc("GET /fed/v1/blob/{cid}", s.handleBlob)
```

- [ ] **Step 4: Create `internal/federation/coordinator/blob.go`**

```go
package coordinator

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nova-archive/nova/internal/federation/tokens"
	"github.com/nova-archive/nova/internal/federation/wire"
	gocid "github.com/ipfs/go-cid"
)

// jtiCache is an in-memory single-use replay cache for repair-token jti values
// (D-M4-9). Entries expire at the token's not_after; combined with the
// source_boot_time floor, restart leaves no usable replay window.
type jtiCache struct {
	mu   sync.Mutex
	seen map[string]time.Time // jti -> expiry
}

func newJTICache() *jtiCache { return &jtiCache{seen: map[string]time.Time{}} }

// useOnce returns false if jti was already seen (replay). It also opportunistically
// sweeps expired entries.
func (c *jtiCache) useOnce(jti string, exp time.Time, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.seen {
		if now.After(e) {
			delete(c.seen, k)
		}
	}
	if _, ok := c.seen[jti]; ok {
		return false
	}
	c.seen[jti] = exp
	return true
}

func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	node, ok := s.authNode(w, r)
	if !ok {
		return
	}
	if s.signer == nil || s.backend == nil {
		writeError(w, http.StatusServiceUnavailable, "source_unavailable", "")
		return
	}
	cidStr := r.PathValue("cid")
	now := time.Now()

	tok := r.Header.Get("X-Nova-Repair-Token")
	pub, err := wire.DecodePublicKey(s.signer.PublicKeyWire())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "pubkey")
		return
	}
	claims, err := wire.Verify(pub, tok, now)
	if err != nil {
		slog.Info("fed.token.rejected", "reason", "verify", "err", err)
		writeError(w, http.StatusForbidden, wire.FailReasonSourceUnauthorized, "token verify failed")
		return
	}
	// Bindings: source is us, dest is the caller, cid matches the path.
	if claims.SourceNodeID != tokens.ReservedCoordinatorSourceID || claims.DestNodeID != node.String() || claims.CID != cidStr {
		slog.Info("fed.token.rejected", "reason", "binding", "cid", cidStr)
		writeError(w, http.StatusForbidden, wire.FailReasonSourceUnauthorized, "token binding mismatch")
		return
	}
	// Restart-safe replay defense (D-M4-9).
	if claims.NotBefore < s.sourceBootTime.Unix() {
		slog.Info("fed.token.rejected", "reason", "pre_boot")
		writeError(w, http.StatusForbidden, wire.FailReasonSourceUnauthorized, "token predates source boot")
		return
	}
	if !s.jti.useOnce(claims.JTI, time.Unix(claims.NotAfter, 0), now) {
		slog.Info("fed.token.rejected", "reason", "replay", "jti", claims.JTI)
		writeError(w, http.StatusForbidden, wire.FailReasonSourceUnauthorized, "token already used")
		return
	}

	// Preflight size (D-M4-3): reject before writing any body byte.
	size, err := s.q.GetBlobByteSize(r.Context(), cidStr)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, wire.FailReasonBlobUnavailable, "")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "size")
		return
	}
	maxBytes := claims.MaxBytes
	if s.cfg.MaxTransferBytes > 0 && s.cfg.MaxTransferBytes < maxBytes {
		maxBytes = s.cfg.MaxTransferBytes
	}
	if size > maxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "blob_too_large", "")
		return
	}

	c, err := gocid.Decode(cidStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_cid", "")
		return
	}
	rc, err := s.backend.Get(r.Context(), c)
	if err != nil {
		writeError(w, http.StatusNotFound, wire.FailReasonBlobUnavailable, "")
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	n, _ := io.Copy(w, io.LimitReader(rc, maxBytes))
	slog.Info("fed.blob.served", "cid", cidStr, "bytes", n, "dest_node_id", node, "jti", claims.JTI)
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/federation/coordinator/ -run TestBlob -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
gofmt -w internal/federation/coordinator/blob.go internal/federation/coordinator/server.go
git add internal/federation/coordinator/blob.go internal/federation/coordinator/server.go
git commit -m "feat(p2-m4): coordinator-as-source /fed/v1/blob/{cid} with token + replay + preflight (P2-M4)"
```

---

## Task 7: `/pins/changes` dynamic `Source` + heartbeat pubkey + register requires `blob-transfer/v1`

**Files:**
- Modify: `internal/federation/coordinator/pins.go` (`handleChanges` Source fill)
- Modify: `internal/federation/coordinator/handlers.go` (heartbeat pubkey; register required cap)
- Test: `internal/federation/coordinator/changes_test.go`, `heartbeat_test.go`, `register_test.go`

**Interfaces:**
- Consumes: `Server.signer`, `Server.cfg.RepairTokenTTL`, `cfg.SourceNebulaAddr`.
- Produces: for each **pending `assign`** row, `PinChange.Source = {NodeID:
  ReservedCoordinatorSourceID, NebulaAddr: cfg.SourceNebulaAddr, Token: <minted>}`;
  `HeartbeatResponse.RepairTokenPublicKey = signer.PublicKeyWire()`; register fails
  `missing_capability` unless the donor advertises `blob-transfer/v1`.

- [ ] **Step 1: Write the failing test**

```go
func TestChangesPopulatesSourceForPendingAssign(t *testing.T) {
	env := newFedTestEnv(t)
	signer, _ := tokens.NewSignerFromSeed(make([]byte, 32))
	env.srv.SetSourceDeps(signer, fakeBackendFor("bafy-x", nil), time.Now())
	env.assign(t, "bafy-x", env.nodeID) // uses AssignPin seam

	resp := env.changes(t, 0)
	if len(resp.Changes) != 1 || resp.Changes[0].Source == nil {
		t.Fatalf("expected source-bearing assign, got %+v", resp.Changes)
	}
	src := resp.Changes[0].Source
	pub, _ := wire.DecodePublicKey(signer.PublicKeyWire())
	claims, err := wire.Verify(pub, src.Token, time.Now())
	if err != nil || claims.DestNodeID != env.nodeID || claims.CID != "bafy-x" {
		t.Fatalf("bad minted token: %v %+v", err, claims)
	}
}

func TestMintedTokenAcceptedImmediatelyAfterBoot(t *testing.T) {
	// Regression (D-M4-9): a token minted right after boot must NOT have a
	// not_before earlier than source_boot_time, or the blob endpoint self-rejects.
	env := newFedTestEnv(t)
	signer, _ := tokens.NewSignerFromSeed(make([]byte, 32))
	boot := time.Now()
	env.srv.SetSourceDeps(signer, fakeBackendFor("bafy-x", []byte("ct")), boot)
	env.insertBlob(t, "bafy-x", 2)
	env.assign(t, "bafy-x", env.nodeID)

	resp := env.changes(t, 0)
	src := resp.Changes[0].Source
	pub, _ := wire.DecodePublicKey(signer.PublicKeyWire())
	claims, _ := wire.Verify(pub, src.Token, time.Now())
	if claims.NotBefore < boot.Unix() {
		t.Fatalf("not_before %d < source_boot_time %d (would self-reject)", claims.NotBefore, boot.Unix())
	}
	// And the endpoint accepts it end-to-end.
	if r := env.GET("/fed/v1/blob/bafy-x", map[string]string{"X-Nova-Repair-Token": src.Token}); r.StatusCode != http.StatusOK {
		t.Fatalf("fresh-boot token rejected: %d", r.StatusCode)
	}
}

func TestHeartbeatReturnsPublicKey(t *testing.T) {
	env := newFedTestEnv(t)
	signer, _ := tokens.NewSignerFromSeed(make([]byte, 32))
	env.srv.SetSourceDeps(signer, nil, time.Now())
	hb := env.heartbeat(t)
	if hb.RepairTokenPublicKey != signer.PublicKeyWire() {
		t.Fatalf("pubkey not delivered: %q", hb.RepairTokenPublicKey)
	}
}

func TestRegisterRequiresBlobTransfer(t *testing.T) {
	// A donor advertising only pin-change-log/v1 + snapshot/v1 (no blob-transfer/v1)
	// gets 400 missing_capability when the coordinator requires it.
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/federation/coordinator/ -run 'ChangesPopulatesSource|MintedTokenAcceptedImmediatelyAfterBoot|HeartbeatReturnsPublicKey|RegisterRequiresBlobTransfer' -v`
Expected: FAIL — Source nil; pubkey empty; register accepts.

- [ ] **Step 3: Mint `Source` in `handleChanges` (`pins.go`)**

In the row→`wire.PinChange` loop, after building `changes[i]`, add source minting
for pending assigns:

```go
	for i, row := range rows {
		changes[i] = wire.PinChange{
			Sequence: row.Sequence, AssignmentID: uuid.UUID(row.AssignmentID.Bytes).String(),
			Generation: row.Generation, Kind: row.Kind, CID: row.Cid, ByteSize: row.ByteSize,
		}
		if row.Kind == wire.ChangeKindAssign && s.signer != nil {
			if src := s.mintSource(changes[i], node, time.Now()); src != nil {
				changes[i].Source = src
			}
		}
	}
```

Add the helper (same file):

```go
// mintSource builds a fresh coordinator-as-source grant for a pending assign
// (D-M4-8). Tokens are minted per-serve and NEVER persisted in pin_changes.
func (s *Server) mintSource(ch wire.PinChange, dest uuid.UUID, now time.Time) *wire.ChangeSource {
	jti, err := uuid.NewRandom()
	if err != nil {
		return nil
	}
	// NotBefore is now-skew, but NEVER earlier than sourceBootTime — otherwise the
	// blob endpoint's pre-boot replay floor (D-M4-9) would reject a token we just
	// minted right after startup.
	nb := now.Add(-5 * time.Second)
	if nb.Before(s.sourceBootTime) {
		nb = s.sourceBootTime
	}
	tok, err := s.signer.Mint(wire.Claims{
		JTI: jti.String(), AssignmentID: ch.AssignmentID, Generation: ch.Generation,
		CID: ch.CID, SourceNodeID: tokens.ReservedCoordinatorSourceID, DestNodeID: dest.String(),
		NotBefore: nb.Unix(),
		NotAfter:  now.Add(s.cfg.RepairTokenTTL).Unix(),
		MaxBytes:  ch.ByteSize, ProtocolVersion: wire.ProtocolV1,
	})
	if err != nil {
		slog.Warn("fed.token.mint_failed", "cid", ch.CID, "err", err)
		return nil
	}
	slog.Info("fed.token.minted", "assignment_id", ch.AssignmentID, "cid", ch.CID, "dest_node_id", dest)
	return &wire.ChangeSource{NodeID: tokens.ReservedCoordinatorSourceID, NebulaAddr: s.cfg.SourceNebulaAddr, Token: tok}
}
```

> `MaxBytes` is set to the recorded `ByteSize` so the grant is exactly the blob;
> the endpoint's `MaxTransferBytes` clamp (Task 6) is the hard ceiling.

- [ ] **Step 4: Heartbeat pubkey (`handlers.go`)**

In `handleHeartbeat`, set the pubkey when a signer is configured:

```go
	resp := wire.HeartbeatResponse{ConfigUpdates: &cu, CurrentEpoch: head}
	if s.signer != nil {
		resp.RepairTokenPublicKey = s.signer.PublicKeyWire()
	}
```

- [ ] **Step 5: Require `blob-transfer/v1` at register**

The required-capability set is `Config.RequiredCapabilities`. In `cmd/coordinator`
(Task 13) this becomes `[pin-change-log/v1, snapshot/v1, blob-transfer/v1]`. The
existing negotiation already returns `400 missing_capability` on a gap — so the
register test only needs the required set configured. Confirm the existing
negotiation path handles the new id (no code change beyond the config value);
write `TestRegisterRequiresBlobTransfer` against a `Server` built with that
required set.

- [ ] **Step 6: Run tests + commit**

Run: `go test ./internal/federation/coordinator/ -run 'ChangesPopulatesSource|MintedTokenAcceptedImmediatelyAfterBoot|HeartbeatReturnsPublicKey|RegisterRequiresBlobTransfer' -v`
Expected: PASS.

```bash
gofmt -w internal/federation/coordinator/pins.go internal/federation/coordinator/handlers.go
git add internal/federation/coordinator/pins.go internal/federation/coordinator/handlers.go
git commit -m "feat(p2-m4): mint per-serve Source tokens; deliver repair pubkey; require blob-transfer/v1 (P2-M4)"
```

---

## Task 8: Donor `internal/node/ipfsclient` — Kubo sidecar HTTP client

**Files:**
- Create: `internal/node/ipfsclient/client.go`
- Test: `internal/node/ipfsclient/client_test.go`

**Interfaces:**
- Produces:
  - `ipfsclient.New(apiAddr string) *Client`
  - `(*Client).AddDeterministic(ctx, envelope []byte) (rootCID string, err error)`
    — **branches exactly like `EmbeddedBackend.AddDeterministic`** (verified
    against `internal/ipfs/embedded.go:231`): `importspec.ShouldUseRawCodec(len)`
    ⇒ `/api/v0/block/put?cid-codec=raw&mhtype=sha2-256&pin=true` (the HTTP
    equivalent of `Block().Put(Format("raw"), Hash(sha2-256), Pin(true))`); else
    `/api/v0/add?chunker=size-262144&cid-version=1&raw-leaves=true&hash=sha2-256&pin=true`
    (`Unixfs().Add(...)` with the balanced default layout). Takes `[]byte` (not a
    reader) for **parity with the embedded backend's signature** and to branch on
    the actual byte length, matching what the coordinator imported.
  - `(*Client).Has(ctx, cidStr string) (bool, error)` — **`/api/v0/pin/ls?arg=<cid>&type=recursive`**
    (pinned, not merely present), mirroring `EmbeddedBackend.Has`'s recursive
    pinset check (`embedded.go:343`). A 200 ⇒ pinned; a not-pinned error ⇒ false.
  - `(*Client).Unpin(ctx, cidStr string) error` — `/api/v0/pin/rm?arg=<cid>`
    (`Pin().Rm`), for donor-side unpin (D-M4-5 / Task 12).
  - `(*Client).RepoStoredBytes(ctx) (int64, error)` — `/api/v0/repo/stat` `RepoSize`.
- Consumes: `internal/ipfs/importspec`.

- [ ] **Step 1: Write the failing test (against an httptest fake Kubo)**

```go
// fakeKubo records the path + query of the last add-family call and serves
// canned responses for block/put, add, pin/ls, pin/rm, repo/stat.
type fakeKubo struct {
	addPath  string
	addQuery url.Values
	pinned   map[string]bool
}

func newFakeKubo(t *testing.T) (*fakeKubo, *httptest.Server) {
	k := &fakeKubo{pinned: map[string]bool{"bafyKNOWN": true}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/block/put":
			k.addPath, k.addQuery = r.URL.Path, r.URL.Query()
			io.Copy(io.Discard, r.Body)
			json.NewEncoder(w).Encode(map[string]any{"Key": "bafyRAW", "Size": 1})
		case "/api/v0/add":
			k.addPath, k.addQuery = r.URL.Path, r.URL.Query()
			io.Copy(io.Discard, r.Body)
			json.NewEncoder(w).Encode(map[string]string{"Hash": "bafyDAGPB"})
		case "/api/v0/pin/ls":
			if k.pinned[r.URL.Query().Get("arg")] {
				json.NewEncoder(w).Encode(map[string]any{"Keys": map[string]any{r.URL.Query().Get("arg"): map[string]string{"Type": "recursive"}}})
				return
			}
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"Message": "path is not pinned"})
		case "/api/v0/pin/rm":
			delete(k.pinned, r.URL.Query().Get("arg"))
			json.NewEncoder(w).Encode(map[string]any{"Pins": []string{r.URL.Query().Get("arg")}})
		case "/api/v0/repo/stat":
			json.NewEncoder(w).Encode(map[string]any{"RepoSize": 4096})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return k, srv
}

func TestAddDeterministicRawCodecPath(t *testing.T) {
	k, srv := newFakeKubo(t)
	c := ipfsclient.New(srv.URL)
	root, err := c.AddDeterministic(context.Background(), bytes.Repeat([]byte("x"), 1024)) // <= 1 MiB ⇒ raw
	if err != nil || root != "bafyRAW" {
		t.Fatalf("root=%q err=%v", root, err)
	}
	if k.addPath != "/api/v0/block/put" || k.addQuery.Get("cid-codec") != "raw" ||
		k.addQuery.Get("mhtype") != importspec.HashAlg || k.addQuery.Get("pin") != "true" {
		t.Fatalf("raw path params drift: %s %v", k.addPath, k.addQuery)
	}
}

func TestAddDeterministicDagPBPath(t *testing.T) {
	k, srv := newFakeKubo(t)
	c := ipfsclient.New(srv.URL)
	root, err := c.AddDeterministic(context.Background(), bytes.Repeat([]byte("x"), (1<<20)+1)) // > 1 MiB ⇒ dag-pb
	if err != nil || root != "bafyDAGPB" {
		t.Fatalf("root=%q err=%v", root, err)
	}
	if k.addPath != "/api/v0/add" || k.addQuery.Get("chunker") != importspec.ChunkerSpec ||
		k.addQuery.Get("cid-version") != "1" || k.addQuery.Get("raw-leaves") != "true" || k.addQuery.Get("hash") != importspec.HashAlg {
		t.Fatalf("dag-pb path params drift: %s %v", k.addPath, k.addQuery)
	}
}

func TestRawThresholdBoundary(t *testing.T) {
	_, srv := newFakeKubo(t)
	c := ipfsclient.New(srv.URL)
	// exactly threshold ⇒ raw (bafyRAW); threshold+1 ⇒ dag-pb (bafyDAGPB)
	if r, _ := c.AddDeterministic(context.Background(), make([]byte, importspec.RawCodecThresholdBytes)); r != "bafyRAW" {
		t.Fatalf("threshold should be raw, got %q", r)
	}
	if r, _ := c.AddDeterministic(context.Background(), make([]byte, importspec.RawCodecThresholdBytes+1)); r != "bafyDAGPB" {
		t.Fatalf("threshold+1 should be dag-pb, got %q", r)
	}
}

func TestHasMeansPinnedAndUnpin(t *testing.T) {
	_, srv := newFakeKubo(t)
	c := ipfsclient.New(srv.URL)
	if ok, _ := c.Has(context.Background(), "bafyKNOWN"); !ok {
		t.Fatal("expected Has true for pinned cid")
	}
	if ok, _ := c.Has(context.Background(), "bafyMISSING"); ok {
		t.Fatal("expected Has false for unpinned cid")
	}
	if err := c.Unpin(context.Background(), "bafyKNOWN"); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	if ok, _ := c.Has(context.Background(), "bafyKNOWN"); ok {
		t.Fatal("expected Has false after unpin")
	}
	if n, _ := c.RepoStoredBytes(context.Background()); n != 4096 {
		t.Fatalf("repo size %d", n)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/node/ipfsclient/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Create `internal/node/ipfsclient/client.go`**

```go
// Package ipfsclient is the donor's Kubo-sidecar blockstore client over the
// loopback HTTP API (D-M4-10). It mirrors internal/ipfs.EmbeddedBackend's
// deterministic import EXACTLY — same raw/dag-pb branch on importspec, same
// params — so the donor's root CIDs match the coordinator's bit-for-bit. The
// donor NEVER embeds Kubo; cmd/node must not import internal/ipfs.
package ipfsclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"

	"github.com/nova-archive/nova/internal/ipfs/importspec"
)

type Client struct {
	api string
	hc  *http.Client
}

func New(apiAddr string) *Client { return &Client{api: apiAddr, hc: &http.Client{}} }

func (c *Client) post(ctx context.Context, path string, q url.Values, body io.Reader, contentType string) (*http.Response, error) {
	u := c.api + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return c.hc.Do(req)
}

// AddDeterministic imports envelope with IMPORT_RULES params + pin, branching
// EXACTLY like EmbeddedBackend.AddDeterministic (embedded.go:231): raw-codec
// single block at/under the threshold, dag-pb UnixFS above it. Returns the root
// CID string.
func (c *Client) AddDeterministic(ctx context.Context, envelope []byte) (string, error) {
	if importspec.ShouldUseRawCodec(int64(len(envelope))) {
		return c.blockPutRaw(ctx, envelope)
	}
	return c.unixfsAdd(ctx, envelope)
}

// blockPutRaw mirrors addRaw: Block().Put(Format("raw"), Hash(sha2-256), Pin).
func (c *Client) blockPutRaw(ctx context.Context, envelope []byte) (string, error) {
	q := url.Values{"cid-codec": {importspec.CodecRaw}, "mhtype": {importspec.HashAlg}, "pin": {"true"}}
	resp, err := c.post(ctx, "/api/v0/block/put", q, bytes.NewReader(envelope), "application/octet-stream")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ipfsclient: block put status %d", resp.StatusCode)
	}
	var out struct{ Key string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Key == "" {
		return "", fmt.Errorf("ipfsclient: empty block-put key")
	}
	return out.Key, nil
}

// unixfsAdd mirrors addDagPB: Unixfs().Add(CidVersion 1, sha2-256, raw-leaves,
// size-262144 chunker, balanced layout (the /add default), pin).
func (c *Client) unixfsAdd(ctx context.Context, envelope []byte) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "blob")
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(envelope); err != nil {
		return "", err
	}
	mw.Close()
	q := url.Values{
		"chunker": {importspec.ChunkerSpec}, "cid-version": {"1"},
		"raw-leaves": {"true"}, "hash": {importspec.HashAlg}, "pin": {"true"},
	}
	resp, err := c.post(ctx, "/api/v0/add", q, &body, mw.FormDataContentType())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ipfsclient: add status %d", resp.StatusCode)
	}
	var out struct{ Hash string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Hash == "" {
		return "", fmt.Errorf("ipfsclient: empty add hash")
	}
	return out.Hash, nil
}

// Has reports whether the CID is RECURSIVELY PINNED (not merely present),
// mirroring EmbeddedBackend.Has (embedded.go:343). A non-200 from pin/ls means
// not pinned.
func (c *Client) Has(ctx context.Context, cidStr string) (bool, error) {
	q := url.Values{"arg": {cidStr}, "type": {"recursive"}}
	resp, err := c.post(ctx, "/api/v0/pin/ls", q, nil, "")
	if err != nil {
		return false, err
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK, nil
}

// Unpin removes the recursive pin (Pin().Rm) so Kubo GC can reclaim (D-M4-5).
func (c *Client) Unpin(ctx context.Context, cidStr string) error {
	resp, err := c.post(ctx, "/api/v0/pin/rm", url.Values{"arg": {cidStr}}, nil, "")
	if err != nil {
		return err
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ipfsclient: pin rm status %d", resp.StatusCode)
	}
	return nil
}

// RepoStoredBytes returns the Kubo repo size in bytes for storage accounting.
func (c *Client) RepoStoredBytes(ctx context.Context) (int64, error) {
	resp, err := c.post(ctx, "/api/v0/repo/stat", nil, nil, "")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var out struct{ RepoSize int64 }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.RepoSize, nil
}
```

- [ ] **Step 4: Run tests + commit**

Run: `go test ./internal/node/ipfsclient/ -v`
Expected: PASS.

```bash
gofmt -w internal/node/ipfsclient/client.go
git add internal/node/ipfsclient/
git commit -m "feat(p2-m4): donor Kubo-sidecar HTTP client (shared importspec) (P2-M4)"
```

---

## Task 9: Donor `transfer` — real Verifier + `SourceFetcher`

**Files:**
- Replace: `internal/node/transfer/transfer.go` (drop the M1 stub)
- Test: `internal/node/transfer/transfer_test.go`

**Interfaces:**
- Produces:
  - `transfer.SourceFetcher` interface: `Fetch(ctx, src wire.ChangeSource, cid string, maxBytes int64) (io.ReadCloser, error)`.
  - `transfer.Pinner` interface: `AddDeterministic(ctx, envelope []byte) (string, error)`
    (satisfied by `*ipfsclient.Client`; `[]byte` for parity with the embedded
    backend and so the raw/dag-pb branch sees the true length).
  - `transfer.Verify(ctx, fetcher SourceFetcher, pinner Pinner, src wire.ChangeSource, cid string, maxBytes int64) error`
    — fetch → read into a bounded buffer (reads `maxBytes+1`; **> maxBytes ⇒
    oversize FailErr**, never a silently truncated import) → `AddDeterministic` →
    canonical root-CID compare; returns a classified `*transfer.FailErr{Reason}`
    on failure.
- Consumes: `wire.ChangeSource`, `github.com/ipfs/go-cid` (canonical equality),
  `wire.FailReason*`.

- [ ] **Step 1: Write the failing test**

```go
type fakeFetcher struct{ data []byte; status int }
func (f fakeFetcher) Fetch(_ context.Context, _ wire.ChangeSource, _ string, _ int64) (io.ReadCloser, error) {
	switch f.status {
	case 404:
		return nil, transfer.ErrSourceMissing
	case 403:
		return nil, transfer.ErrSourceUnauthorized
	}
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

// fakePinner echoes a fixed root (or an error). It records the bytes it received
// so a test can assert AddDeterministic saw the full (untruncated) envelope.
type fakePinner struct{ root string; err error; got []byte }
func (p *fakePinner) AddDeterministic(_ context.Context, envelope []byte) (string, error) {
	p.got = append([]byte(nil), envelope...)
	return p.root, p.err
}

func TestVerifyMatch(t *testing.T) {
	p := &fakePinner{root: "bafyX"}
	err := transfer.Verify(context.Background(), fakeFetcher{data: []byte("ciphertext")}, p,
		wire.ChangeSource{}, "bafyX", 1<<20)
	if err != nil {
		t.Fatalf("expected match, got %v", err)
	}
	if string(p.got) != "ciphertext" {
		t.Fatalf("pinner saw %q, want full envelope", p.got)
	}
}
func TestVerifyMismatchClassifiesCIDMismatch(t *testing.T) {
	err := transfer.Verify(context.Background(), fakeFetcher{data: []byte("x")}, &fakePinner{root: "bafyWRONG"},
		wire.ChangeSource{}, "bafyEXPECTED", 1<<20)
	var fe *transfer.FailErr
	if !errors.As(err, &fe) || fe.Reason != wire.FailReasonCIDMismatch {
		t.Fatalf("want cid_mismatch, got %v", err)
	}
}
func TestVerifySource404ClassifiesBlobUnavailable(t *testing.T) {
	err := transfer.Verify(context.Background(), fakeFetcher{status: 404}, &fakePinner{},
		wire.ChangeSource{}, "bafyX", 1<<20)
	var fe *transfer.FailErr
	if !errors.As(err, &fe) || fe.Reason != wire.FailReasonBlobUnavailable {
		t.Fatalf("want blob_unavailable, got %v", err)
	}
}
func TestVerifySource403ClassifiesSourceUnauthorized(t *testing.T) {
	err := transfer.Verify(context.Background(), fakeFetcher{status: 403}, &fakePinner{},
		wire.ChangeSource{}, "bafyX", 1<<20)
	var fe *transfer.FailErr
	if !errors.As(err, &fe) || fe.Reason != wire.FailReasonSourceUnauthorized {
		t.Fatalf("want source_unauthorized, got %v", err)
	}
}
func TestVerifyKuboErrorClassified(t *testing.T) {
	err := transfer.Verify(context.Background(), fakeFetcher{data: []byte("x")}, &fakePinner{err: errors.New("boom")},
		wire.ChangeSource{}, "bafyX", 1<<20)
	var fe *transfer.FailErr
	if !errors.As(err, &fe) || fe.Reason != wire.FailReasonKuboError {
		t.Fatalf("want kubo_error, got %v", err)
	}
}
func TestVerifyOversizeNotImported(t *testing.T) {
	p := &fakePinner{root: "bafyX"}
	err := transfer.Verify(context.Background(), fakeFetcher{data: bytes.Repeat([]byte("x"), 11)}, p,
		wire.ChangeSource{}, "bafyX", 10) // source served 11 bytes under a 10-byte grant
	var fe *transfer.FailErr
	if !errors.As(err, &fe) || fe.Reason != wire.FailReasonOther {
		t.Fatalf("want oversize FailErr(other), got %v", err)
	}
	if p.got != nil {
		t.Fatal("oversize source must NOT be imported (no truncated pin)")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/node/transfer/ -v`
Expected: FAIL — stub returns `ErrNotImplemented`; types undefined.

- [ ] **Step 3: Replace `internal/node/transfer/transfer.go`**

```go
// Package transfer is the donor's fetch + verify path (D-M4-3): pull ciphertext
// from a source, re-import it deterministically via the Kubo sidecar, and compare
// the computed root CID to the assigned CID (D4). M4 is coordinator-as-source only.
package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"

	gocid "github.com/ipfs/go-cid"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// ErrSourceMissing is returned by a SourceFetcher when the source has no such CID.
var ErrSourceMissing = errors.New("transfer: source missing cid")

// ErrSourceUnauthorized is returned when the source rejects the repair token.
var ErrSourceUnauthorized = errors.New("transfer: source unauthorized")

// FailErr carries a classified fail reason (wire.FailReason*) for /pins/{cid}/fail.
type FailErr struct {
	Reason string
	Err    error
}

func (e *FailErr) Error() string { return fmt.Sprintf("transfer failed (%s): %v", e.Reason, e.Err) }
func (e *FailErr) Unwrap() error { return e.Err }

// SourceFetcher fetches bytes for a CID from a designated source under a grant.
type SourceFetcher interface {
	Fetch(ctx context.Context, src wire.ChangeSource, cid string, maxBytes int64) (io.ReadCloser, error)
}

// Pinner deterministically re-imports + pins an envelope, returning the root CID
// ([]byte for parity with the embedded backend and the raw/dag-pb size branch).
type Pinner interface {
	AddDeterministic(ctx context.Context, envelope []byte) (string, error)
}

// Verify fetches, re-imports, and confirms the root CID equals cid. It reads at
// most maxBytes+1 and refuses an over-grant source WITHOUT importing it (so a
// misbehaving source can never get a truncated blob pinned + mislabeled
// cid_mismatch — important when this verifier is reused for M5 donor sources).
// On any failure it returns a *FailErr with the spec reason (D-M4-8).
func Verify(ctx context.Context, fetcher SourceFetcher, pinner Pinner, src wire.ChangeSource, cid string, maxBytes int64) error {
	rc, err := fetcher.Fetch(ctx, src, cid, maxBytes)
	if err != nil {
		switch {
		case errors.Is(err, ErrSourceMissing):
			return &FailErr{Reason: wire.FailReasonBlobUnavailable, Err: err}
		case errors.Is(err, ErrSourceUnauthorized):
			return &FailErr{Reason: wire.FailReasonSourceUnauthorized, Err: err}
		default:
			return &FailErr{Reason: wire.FailReasonNetworkError, Err: err}
		}
	}
	defer rc.Close()

	// Read up to maxBytes+1 so we can DETECT an over-grant response.
	envelope, err := io.ReadAll(io.LimitReader(rc, maxBytes+1))
	if err != nil {
		return &FailErr{Reason: wire.FailReasonNetworkError, Err: err}
	}
	if int64(len(envelope)) > maxBytes {
		return &FailErr{Reason: wire.FailReasonOther, Err: fmt.Errorf("source served > max_bytes (%d)", maxBytes)}
	}

	root, err := pinner.AddDeterministic(ctx, envelope)
	if err != nil {
		return &FailErr{Reason: wire.FailReasonKuboError, Err: err}
	}
	want, err := gocid.Decode(cid)
	if err != nil {
		return &FailErr{Reason: wire.FailReasonCIDMismatch, Err: fmt.Errorf("bad assigned cid: %w", err)}
	}
	got, err := gocid.Decode(root)
	if err != nil {
		return &FailErr{Reason: wire.FailReasonCIDMismatch, Err: fmt.Errorf("bad computed cid: %w", err)}
	}
	if !got.Equals(want) {
		return &FailErr{Reason: wire.FailReasonCIDMismatch, Err: fmt.Errorf("root %s != assigned %s", got, want)}
	}
	return nil
}
```

- [ ] **Step 4: Run tests + commit**

Run: `go test ./internal/node/transfer/ -v`
Expected: PASS.

```bash
gofmt -w internal/node/transfer/transfer.go
git add internal/node/transfer/
git commit -m "feat(p2-m4): donor transfer fetch+reimport+root-CID verify with classified fails (P2-M4)"
```

---

## Task 10: Donor durable verify/ack progress

**Files:**
- Create: `internal/node/state/progress.go`
- Test: `internal/node/state/progress_test.go`

**Interfaces:**
- Produces a durable per-CID progress store (atomic-JSON, like `FileAssignmentStore`):
  - `state.ProgressVerifiedPending`, `state.ProgressAckDelivered` constants.
  - `(*FileProgressStore).Set(cid string, p Progress) error`
  - `(*FileProgressStore).Get(cid string) (Progress, bool)`
  - `(*FileProgressStore).PendingAcks() []ProgressEntry` (entries in
    `verified-ack-pending`, for startup reconcile).
  - `(*FileProgressStore).Clear(cid string) error`
- Reuses the atomic write helper (`fsyncDir`/temp→rename) from `registration.go`.

- [ ] **Step 1: Write the failing test**

```go
func TestProgressPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	s, err := state.NewFileProgressStore(dir)
	if err != nil { t.Fatal(err) }
	if err := s.Set("bafyX", state.Progress{AssignmentID: "a1", Generation: 1, ByteSize: 10, State: state.ProgressVerifiedPending}); err != nil {
		t.Fatal(err)
	}
	// reload from disk
	s2, _ := state.NewFileProgressStore(dir)
	p, ok := s2.Get("bafyX")
	if !ok || p.State != state.ProgressVerifiedPending || p.AssignmentID != "a1" {
		t.Fatalf("reload mismatch: %+v ok=%v", p, ok)
	}
	if got := s2.PendingAcks(); len(got) != 1 || got[0].CID != "bafyX" {
		t.Fatalf("pending acks: %+v", got)
	}
	if err := s2.Set("bafyX", state.Progress{AssignmentID: "a1", Generation: 1, State: state.ProgressAckDelivered}); err != nil {
		t.Fatal(err)
	}
	if got := s2.PendingAcks(); len(got) != 0 {
		t.Fatalf("expected no pending after delivered: %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/node/state/ -run TestProgress -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Create `internal/node/state/progress.go`**

```go
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Progress states (D-M4-5). desired/pending is the absence of a Progress entry.
const (
	ProgressVerifiedPending = "verified-ack-pending"
	ProgressAckDelivered    = "acked-delivered"
)

// Progress is the donor's local record of fetch/verify/ack for one CID.
type Progress struct {
	AssignmentID string `json:"assignment_id"`
	Generation   int64  `json:"generation"`
	ByteSize     int64  `json:"byte_size"`
	State        string `json:"state"`
}

// ProgressEntry pairs a CID with its progress (for PendingAcks).
type ProgressEntry struct {
	CID string
	Progress
}

// FileProgressStore persists Progress as a single atomic-JSON map under
// storage_dir/state/progress.json (set-before-ack ordering lives in the agent).
type FileProgressStore struct {
	mu   sync.Mutex
	path string
	m    map[string]Progress
}

func NewFileProgressStore(stateDir string) (*FileProgressStore, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	s := &FileProgressStore{path: filepath.Join(stateDir, "progress.json"), m: map[string]Progress{}}
	data, err := os.ReadFile(s.path)
	if err == nil {
		_ = json.Unmarshal(data, &s.m)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *FileProgressStore) Get(cid string) (Progress, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[cid]
	return p, ok
}

func (s *FileProgressStore) Set(cid string, p Progress) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[cid] = p
	return s.flushLocked()
}

func (s *FileProgressStore) Clear(cid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, cid)
	return s.flushLocked()
}

func (s *FileProgressStore) PendingAcks() []ProgressEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ProgressEntry
	for cid, p := range s.m {
		if p.State == ProgressVerifiedPending {
			out = append(out, ProgressEntry{CID: cid, Progress: p})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CID < out[j].CID })
	return out
}

// flushLocked writes the map atomically (temp → fsync → rename → dir-fsync),
// reusing the package's atomic-write convention.
func (s *FileProgressStore) flushLocked() error {
	data, err := json.Marshal(s.m)
	if err != nil {
		return err
	}
	return atomicWrite(s.path, data) // helper shared with registration.go / assignments.go
}
```

> If the existing atomic writer is named differently (`writeFileAtomic`,
> `atomicWriteFile`), use that exact name — `grep -n "func.*[Aa]tomic" internal/node/state/*.go`.

- [ ] **Step 4: Run tests + commit**

Run: `go test ./internal/node/state/ -run TestProgress -v`
Expected: PASS.

```bash
gofmt -w internal/node/state/progress.go
git add internal/node/state/progress.go
git commit -m "feat(p2-m4): donor durable verify/ack progress store (P2-M4)"
```

---

## Task 11: Donor client `Ack`/`Fail` + HTTP `SourceFetcher`

**Files:**
- Modify: `internal/node/agent/client.go`
- Test: `internal/node/agent/client_test.go`

**Interfaces:**
- Produces on `*HTTPClient`:
  - `Ack(ctx, cid string, a wire.Ack) error` — POST `/fed/v1/pins/{cid}/ack`,
    204 (apply) and 204 (idempotent replay) both succeed; 409 → `ErrStaleAssignment`.
  - `Fail(ctx, cid string, f wire.Fail) error` — POST `/fed/v1/pins/{cid}/fail`.
  - `Fetch(ctx, src wire.ChangeSource, cid string, maxBytes int64) (io.ReadCloser, error)`
    — GET the source's `/fed/v1/blob/{cid}` with `X-Nova-Repair-Token`; 404 →
    `transfer.ErrSourceMissing`, 403 → `transfer.ErrSourceUnauthorized`. (For M4
    the source addr is the coordinator; the client reuses its mTLS transport.)
- Sentinel: `agent.ErrStaleAssignment`.

- [ ] **Step 1: Write the failing test**

```go
func TestAckSuccessAndIdempotentAndStale(t *testing.T) {
	var status int
	srv := newMTLSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/ack") { http.NotFound(w, r); return }
		w.WriteHeader(status)
		if status == http.StatusConflict {
			json.NewEncoder(w).Encode(wire.ErrorResponse{Code: wire.CodeStaleAssignment})
		}
	})
	c := agent.NewHTTPClient(srv.URL, srv.ClientTLS)
	status = http.StatusNoContent
	if err := c.Ack(context.Background(), "bafyX", wire.Ack{AssignmentID: "a1", Generation: 1, CID: "bafyX"}); err != nil {
		t.Fatalf("204 should succeed: %v", err)
	}
	status = http.StatusConflict
	if err := c.Ack(context.Background(), "bafyX", wire.Ack{AssignmentID: "a1", Generation: 1, CID: "bafyX"}); !errors.Is(err, agent.ErrStaleAssignment) {
		t.Fatalf("409 should be ErrStaleAssignment: %v", err)
	}
}

func TestFetchClassifiesStatus(t *testing.T) {
	// 200 → reader with body; 404 → transfer.ErrSourceMissing; 403 → transfer.ErrSourceUnauthorized
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/node/agent/ -run 'TestAck|TestFetch' -v`
Expected: FAIL — methods undefined.

- [ ] **Step 3: Add to `internal/node/agent/client.go`**

```go
var ErrStaleAssignment = errors.New("agent: stale_assignment")

func (c *HTTPClient) Ack(ctx context.Context, cid string, a wire.Ack) error {
	err := c.post(ctx, "/fed/v1/pins/"+url.PathEscape(cid)+"/ack", a, nil, http.StatusNoContent)
	if err != nil && strings.Contains(err.Error(), wire.CodeStaleAssignment) {
		return ErrStaleAssignment
	}
	return err
}

func (c *HTTPClient) Fail(ctx context.Context, cid string, f wire.Fail) error {
	return c.post(ctx, "/fed/v1/pins/"+url.PathEscape(cid)+"/fail", f, nil, http.StatusNoContent)
}

// Fetch pulls ciphertext for cid from the source under its repair token. In M4 the
// source is the coordinator (src.NebulaAddr == coordinator); the donor reuses its
// own mTLS transport against c.base. (M5 will dial src.NebulaAddr for donor sources.)
func (c *HTTPClient) Fetch(ctx context.Context, src wire.ChangeSource, cid string, maxBytes int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/fed/v1/blob/"+url.PathEscape(cid), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Nova-Repair-Token", src.Token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, nil
	case http.StatusNotFound:
		resp.Body.Close()
		return nil, transfer.ErrSourceMissing
	case http.StatusForbidden:
		resp.Body.Close()
		return nil, transfer.ErrSourceUnauthorized
	default:
		resp.Body.Close()
		return nil, fmt.Errorf("fetch: status %d", resp.StatusCode)
	}
}
```

Add imports: `"io"`, `"net/url"`, `"github.com/nova-archive/nova/internal/node/transfer"`.

- [ ] **Step 4: Run tests + commit**

Run: `go test ./internal/node/agent/ -run 'TestAck|TestFetch' -v`
Expected: PASS.

```bash
gofmt -w internal/node/agent/client.go
git add internal/node/agent/client.go
git commit -m "feat(p2-m4): donor client Ack/Fail + source Fetch (P2-M4)"
```

---

## Task 12: Donor agent — fetch→verify→pin→persist→ack loop + startup reconcile

**Files:**
- Modify: `internal/node/agent/agent.go`
- Test: `internal/node/agent/agent_transfer_test.go` (new file)

**Interfaces:**
- Consumes: `transfer.Verify`, `transfer.SourceFetcher`, `transfer.Pinner`,
  `state.FileProgressStore`, `Client.Ack`/`Fail`, `state.AssignmentStore`.
- The `Client` interface gains `Ack`/`Fail`; `Agent` gains `fetcher
  transfer.SourceFetcher`, a `pinner` (a local interface: `AddDeterministic` +
  `Has` + `Unpin` + `RepoStoredBytes`), `progress *state.FileProgressStore`,
  `storageMax int64`.
- Behavior: after each sync, for every desired `pending` assignment carrying a
  `Source`, run `replicateOne` **unless** progress already records this exact
  `(assignment_id, generation)` as `acked-delivered` — a generation bump (M3
  reassign) is NOT skipped; stale-generation progress is cleared. On success
  persist `verified-ack-pending` → `Ack` → `acked-delivered`; on
  `*transfer.FailErr` send `Fail`. **Unpin handling:** when a sync applies an
  `unpin` change, clear progress **and** `Unpin` the CID from the sidecar so
  ciphertext is not pinned forever after `novactl pin unpin`. On startup,
  reconcile `PendingAcks()` by re-checking `Has`, then retry `Ack` **only if the
  current desired assignment still matches `(assignment_id, generation)`** (else
  clear stale progress).

- [ ] **Step 1: Write the failing test (fake client + fetcher + pinner)**

```go
func TestReplicateOneHappyPathAcks(t *testing.T) {
	h := newAgentHarness(t) // wires fake Client, fetcher returning "x", pinner returning "bafyX"
	h.assignments.Replace([]state.DesiredAssignment{{CID: "bafyX", AssignmentID: "a1", Generation: 1, ByteSize: 1, State: "pending"}})
	h.sourceFor("bafyX", validToken)

	h.agent.ReplicatePending(context.Background())

	if !h.client.ackedCID("bafyX") {
		t.Fatal("expected ack for bafyX")
	}
	if p, _ := h.progress.Get("bafyX"); p.State != state.ProgressAckDelivered {
		t.Fatalf("progress %+v", p)
	}
}

func TestReplicateOneCIDMismatchFails(t *testing.T) {
	h := newAgentHarness(t) // pinner returns "bafyWRONG"
	h.assignments.Replace([]state.DesiredAssignment{{CID: "bafyX", AssignmentID: "a1", Generation: 1, State: "pending"}})
	h.sourceFor("bafyX", validToken)
	h.agent.ReplicatePending(context.Background())
	if r := h.client.lastFailReason("bafyX"); r != wire.FailReasonCIDMismatch {
		t.Fatalf("want cid_mismatch fail, got %q", r)
	}
}

func TestReassignAtNewGenerationIsNotSkipped(t *testing.T) {
	// gen 1 acked-delivered, then the coordinator reassigns at gen 2 (M3 unpin +
	// re-assign). The acked gen-1 progress must NOT cause gen 2 to be skipped.
	h := newAgentHarness(t)
	h.progress.Set("bafyX", state.Progress{AssignmentID: "a1", Generation: 1, State: state.ProgressAckDelivered})
	h.assignments.Replace([]state.DesiredAssignment{{CID: "bafyX", AssignmentID: "a1", Generation: 2, ByteSize: 1, State: "pending"}})
	h.sourceFor("bafyX", validToken)
	h.agent.ReplicatePending(context.Background())
	if got := h.client.lastAckGeneration("bafyX"); got != 2 {
		t.Fatalf("expected gen-2 ack, got %d", got)
	}
}

func TestUnpinClearsProgressAndUnpinsLocally(t *testing.T) {
	// assign → ack, then an unpin change arrives: progress cleared + sidecar Unpin.
	h := newAgentHarness(t)
	h.progress.Set("bafyX", state.Progress{AssignmentID: "a1", Generation: 1, State: state.ProgressAckDelivered})
	h.pinner.has["bafyX"] = true
	h.agent.HandleUnpin(context.Background(), "bafyX")
	if _, ok := h.progress.Get("bafyX"); ok {
		t.Fatal("progress must be cleared on unpin")
	}
	if h.pinner.has["bafyX"] {
		t.Fatal("sidecar pin must be removed on unpin")
	}
}

func TestStartupReconcileRetriesAckWhenStillPinnedAndMatching(t *testing.T) {
	h := newAgentHarness(t)
	h.progress.Set("bafyX", state.Progress{AssignmentID: "a1", Generation: 1, State: state.ProgressVerifiedPending})
	h.assignments.Replace([]state.DesiredAssignment{{CID: "bafyX", AssignmentID: "a1", Generation: 1, State: "pending"}})
	h.pinner.has["bafyX"] = true // sidecar still holds it
	h.agent.ReconcilePendingAcks(context.Background())
	if !h.client.ackedCID("bafyX") {
		t.Fatal("expected idempotent ack retry")
	}
}

func TestStartupReconcileDropsStaleGenerationProgress(t *testing.T) {
	// progress says gen 1 verified-pending, but the current desired assignment is
	// gen 2 — the gen-1 ack must NOT be retried; stale progress is cleared.
	h := newAgentHarness(t)
	h.progress.Set("bafyX", state.Progress{AssignmentID: "a1", Generation: 1, State: state.ProgressVerifiedPending})
	h.assignments.Replace([]state.DesiredAssignment{{CID: "bafyX", AssignmentID: "a1", Generation: 2, State: "pending"}})
	h.pinner.has["bafyX"] = true
	h.agent.ReconcilePendingAcks(context.Background())
	if h.client.ackedCID("bafyX") {
		t.Fatal("must not ack a superseded generation")
	}
	if _, ok := h.progress.Get("bafyX"); ok {
		t.Fatal("stale-generation progress must be cleared")
	}
}

func TestStartupReconcileReFetchesWhenPinLost(t *testing.T) {
	h := newAgentHarness(t)
	h.progress.Set("bafyX", state.Progress{AssignmentID: "a1", Generation: 1, State: state.ProgressVerifiedPending})
	h.assignments.Replace([]state.DesiredAssignment{{CID: "bafyX", AssignmentID: "a1", Generation: 1, State: "pending"}})
	h.pinner.has["bafyX"] = false // GC / disk loss
	h.agent.ReconcilePendingAcks(context.Background())
	if p, ok := h.progress.Get("bafyX"); ok && p.State == state.ProgressVerifiedPending {
		t.Fatal("lost pin must be downgraded, not left ack-pending")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/node/agent/ -run 'Replicate|Reconcile|Reassign|Unpin' -v`
Expected: FAIL — methods/fields undefined.

- [ ] **Step 3: Extend `agent.go`**

Add to the `Client` interface: `Ack(ctx, cid string, a wire.Ack) error` and
`Fail(ctx, cid string, f wire.Fail) error`. Define a local `pinner` interface the
`*ipfsclient.Client` satisfies, and add `Agent` fields:

```go
// blockstore is the donor's view of its Kubo sidecar (satisfied by *ipfsclient.Client).
type blockstore interface {
	AddDeterministic(ctx context.Context, envelope []byte) (string, error) // transfer.Pinner
	Has(ctx context.Context, cid string) (bool, error)
	Unpin(ctx context.Context, cid string) error
	RepoStoredBytes(ctx context.Context) (int64, error)
}

// progressMatches reports whether durable progress refers to THIS exact
// assignment generation (D-M4-5). A generation bump invalidates older progress.
func progressMatches(p state.Progress, da state.DesiredAssignment) bool {
	return p.AssignmentID == da.AssignmentID && p.Generation == da.Generation
}

// ReplicatePending fetches+verifies+pins+acks every desired pending assignment
// that carries a Source (D-M4-5). Bounded by storageMax; classified Fail on error.
func (a *Agent) ReplicatePending(ctx context.Context) {
	for _, da := range a.assignments.List() {
		if da.State != "pending" {
			continue
		}
		// Skip ONLY if this exact (assignment_id, generation) is already acked.
		// A stale-generation progress entry is cleared so the new generation runs.
		if p, ok := a.progress.Get(da.CID); ok {
			if progressMatches(p, da) && p.State == state.ProgressAckDelivered {
				continue
			}
			if !progressMatches(p, da) {
				_ = a.progress.Clear(da.CID)
			}
		}
		src := a.sources.Source(da.CID) // most-recent Source seen for this CID (from the changes poll)
		if src == nil {
			continue // no grant yet; next poll mints one
		}
		a.replicateOne(ctx, da, *src)
	}
}

// HandleUnpin is invoked when a sync applies an `unpin` change: clear local
// progress and remove the sidecar pin so the donor does not hold ciphertext
// forever after `novactl pin unpin` (D-M4-5).
func (a *Agent) HandleUnpin(ctx context.Context, cid string) {
	_ = a.progress.Clear(cid)
	if err := a.pinner.Unpin(ctx, cid); err != nil {
		slog.Warn("node.unpin.failed", "cid", cid, "err", err)
		return
	}
	slog.Info("node.unpin.applied", "cid", cid)
}

func (a *Agent) replicateOne(ctx context.Context, da state.DesiredAssignment, src wire.ChangeSource) {
	// Storage limit (D-M4-6): refuse if pinning would exceed storage_max_bytes.
	if a.storageMax > 0 {
		if used, err := a.pinner.RepoStoredBytes(ctx); err == nil && used+da.ByteSize > a.storageMax {
			slog.Warn("node.transfer.out_of_space", "cid", da.CID, "used", used, "need", da.ByteSize)
			a.sendFail(ctx, da, wire.FailReasonOutOfSpace)
			return
		}
	}
	start := time.Now()
	err := transfer.Verify(ctx, a.fetcher, a.pinner, src, da.CID, transferMax(da.ByteSize))
	if err != nil {
		var fe *transfer.FailErr
		if errors.As(err, &fe) {
			slog.Warn("node.transfer.failed", "cid", da.CID, "reason", fe.Reason, "err", fe.Err)
			a.sendFail(ctx, da, fe.Reason)
			return
		}
		slog.Warn("node.transfer.failed", "cid", da.CID, "reason", wire.FailReasonOther, "err", err)
		a.sendFail(ctx, da, wire.FailReasonOther)
		return
	}
	slog.Info("node.transfer.verified", "cid", da.CID, "verify_ms", time.Since(start).Milliseconds())
	// PERSIST verified state BEFORE acking (crash-safe ordering, D-M4-5).
	if err := a.progress.Set(da.CID, state.Progress{AssignmentID: da.AssignmentID, Generation: da.Generation, ByteSize: da.ByteSize, State: state.ProgressVerifiedPending}); err != nil {
		slog.Warn("node.state.write_error", "cid", da.CID, "err", err)
		return // do NOT ack without durable evidence
	}
	a.deliverAck(ctx, da.CID, da.AssignmentID, da.Generation, da.ByteSize)
}

func (a *Agent) deliverAck(ctx context.Context, cid, aid string, gen, size int64) {
	err := a.client.Ack(ctx, cid, wire.Ack{
		AssignmentID: aid, Generation: gen, CID: cid, ByteSize: size,
		IPFSPinStatus: "pinned", FetchedFromNodeID: wire.CoordinatorSourceID,
	})
	if errors.Is(err, ErrStaleAssignment) {
		// Superseded while we held it; drop progress and let the next sync reconcile.
		_ = a.progress.Clear(cid)
		return
	}
	if err != nil {
		slog.Warn("node.ack.retry", "cid", cid, "err", err) // stays verified-ack-pending; retried next loop
		return
	}
	_ = a.progress.Set(cid, state.Progress{AssignmentID: aid, Generation: gen, ByteSize: size, State: state.ProgressAckDelivered})
	slog.Info("node.ack.delivered", "cid", cid, "assignment_id", aid)
}

func (a *Agent) sendFail(ctx context.Context, da state.DesiredAssignment, reason string) {
	_ = a.client.Fail(ctx, da.CID, wire.Fail{AssignmentID: da.AssignmentID, Generation: da.Generation, CID: da.CID, Reason: reason})
}

// ReconcilePendingAcks runs once at startup: for each verified-ack-pending entry,
// (1) drop it if it no longer matches the current desired assignment generation
// (superseded while we were down), (2) re-check the local pin (D-M4-5) before
// retrying the idempotent ack; a lost pin is downgraded so it re-fetches.
func (a *Agent) ReconcilePendingAcks(ctx context.Context) {
	desired := map[string]state.DesiredAssignment{}
	for _, da := range a.assignments.List() {
		desired[da.CID] = da
	}
	for _, e := range a.progress.PendingAcks() {
		da, stillDesired := desired[e.CID]
		if !stillDesired || !progressMatches(e.Progress, da) {
			slog.Info("node.sync.progress_superseded", "cid", e.CID)
			_ = a.progress.Clear(e.CID) // stale generation / no longer desired
			continue
		}
		has, err := a.pinner.Has(ctx, e.CID)
		if err != nil {
			continue
		}
		if !has {
			slog.Warn("node.transfer.pin_lost", "cid", e.CID)
			_ = a.progress.Clear(e.CID) // desired set still has it as pending → re-fetch
			continue
		}
		a.deliverAck(ctx, e.CID, e.AssignmentID, e.Generation, e.ByteSize)
	}
}
```

> `wire.CoordinatorSourceID` (Task 1) is the donor-safe constant for
> `Ack.FetchedFromNodeID` — the donor cannot import the operator-only `tokens`
> package, so it references `wire` directly. `transferMax(size)` clamps the
> per-blob fetch ceiling, and `a.sources` is a small in-memory most-recent-Source
> cache populated from the changes poll: when `syncOnce` applies an `assign`
> change carrying a non-nil `Source`, store it keyed by CID so `ReplicatePending`
> has a fresh grant to fetch under.

Hook the loop, inside `syncOnce` (after `a.assignments.ApplyChanges` persists the
batch, so local pins are torn down only once intent is durable):
- for each applied `assign` change with a non-nil `Source`, store it in `a.sources`;
- for each applied `unpin` change, call `a.HandleUnpin(ctx, ch.CID)`.

In `Run`: call `ReconcilePendingAcks` once before the ticker loop, and
`ReplicatePending` after each `syncOnce`. (Snapshot recovery in `recoverSnapshot`
replaces the desired set wholesale; after it runs, `ReconcilePendingAcks` on the
next cycle clears any progress whose CID is no longer desired.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/node/agent/ -run 'Replicate|Reconcile|Reassign|Unpin' -v && go test ./internal/node/agent/ -v`
Expected: PASS (including the existing M2/M3 agent tests).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/node/agent/agent.go internal/federation/wire/messages.go internal/federation/tokens/tokens.go
git add internal/node/agent/ internal/federation/wire/ internal/federation/tokens/
git commit -m "feat(p2-m4): donor replicate loop (fetch→verify→pin→persist→ack) + startup reconcile (P2-M4)"
```

---

## Task 13: Donor config + `cmd/node` wiring

**Files:**
- Modify: `internal/node/config/config.go` (`StorageMaxBytes`, `KuboAPIAddr`)
- Modify: `cmd/node/main.go`
- Test: `internal/node/config/config_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestStorageMaxBytesOptional(t *testing.T) {
	// storage_max_bytes omitted ⇒ 0 (unlimited); negative ⇒ error.
	c := mustLoadNodeConfig(t, baseValidYAML+"\nstorage_max_bytes: -1\n")
	_ = c // expect LoadFromBytes to error on negative
}
func TestKuboAPIAddrDefault(t *testing.T) {
	c := mustLoadNodeConfig(t, baseValidYAML) // no kubo_api_addr
	if c.KuboAPIAddr != "http://127.0.0.1:5001" {
		t.Fatalf("default kubo addr: %q", c.KuboAPIAddr)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/node/config/ -run 'StorageMaxBytes|KuboAPIAddr' -v`
Expected: FAIL — fields undefined.

- [ ] **Step 3: Extend the donor config**

Add fields + defaults + validation in `config.go`:

```go
	StorageMaxBytes int64  `yaml:"storage_max_bytes"` // 0 ⇒ unlimited (M4 enforces out_of_space)
	KuboAPIAddr     string `yaml:"kubo_api_addr"`     // loopback Kubo sidecar HTTP API
```

In `LoadFromBytes` defaults: `if c.KuboAPIAddr == "" { c.KuboAPIAddr = "http://127.0.0.1:5001" }`.
In `validate`: `if c.StorageMaxBytes < 0 { return fmt.Errorf("node config: storage_max_bytes must be >= 0") }`.

- [ ] **Step 4: Wire `cmd/node/main.go`**

Construct the sidecar client, source fetcher, progress store, and pass to the
agent:

```go
	pinner := ipfsclient.New(cfg.KuboAPIAddr)
	progress, err := state.NewFileProgressStore(filepath.Join(cfg.StorageDir, "state"))
	// existing: reg, cursor, assignments stores, HTTPClient `client`
	ag := agent.New(cfg, reg, cursor, assignments, client, hb, poll,
		agent.WithSource(client /* SourceFetcher */, pinner, progress, cfg.StorageMaxBytes))
```

> Add `agent.WithSource(...)` as a functional option (or extend `agent.New`'s
> signature) — match the existing `agent.New` shape. `client` (the `*HTTPClient`)
> satisfies both `Client` and `transfer.SourceFetcher`; `pinner` satisfies both
> `transfer.Pinner` and the `Hasser`/`RepoStoredBytes` needs.

- [ ] **Step 5: Build + boundary check + commit**

Run:
```bash
go build ./cmd/node/...
go test ./internal/node/... -v
```
Expected: PASS. (The `donor-deps-boundary` extension is Task 14 — the build will
pull `internal/ipfs/importspec` + `go-cid`; the gate is updated there.)

```bash
gofmt -w internal/node/config/config.go cmd/node/main.go
git add internal/node/config/config.go cmd/node/main.go
git commit -m "feat(p2-m4): donor storage_max_bytes + kubo sidecar wiring (P2-M4)"
```

---

## Task 14: `donor-deps-boundary` allowlist extension (reviewed)

**Files:**
- Modify: `scripts/check_node_deps.sh`
- Test: run the gate; demonstrate-red against an injected embedded-Kubo import.

- [ ] **Step 1: Run the gate to see the new violations**

Run: `bash scripts/check_node_deps.sh`
Expected: FAIL listing `.../internal/ipfs/importspec`, `.../internal/node/ipfsclient`
(covered by the existing `internal/node` prefix), `github.com/ipfs/go-cid` and its
multiformats transitive deps (`go-multihash`, `go-multibase`, `go-varint`, …).

- [ ] **Step 2: Extend the allowlist with the reviewed minimum**

In `ALLOWED`, add the new entries with a comment:

```bash
  "$MOD/internal/ipfs/importspec"   # P2-M4: shared deterministic-import params (no Kubo)
  "github.com/ipfs/go-cid"          # P2-M4: canonical root-CID equality
  "github.com/multiformats/go-multihash"
  "github.com/multiformats/go-multibase"
  "github.com/multiformats/go-varint"
  "github.com/multiformats/go-base32"
  "github.com/multiformats/go-base36"
  "github.com/mr-tron/base58"
  "lukechampine.com/blake3"         # only if go-multihash pulls it; confirm from the FAIL list
```

> Add **exactly** the packages the FAIL list shows — no more. If the transitive
> set is larger than acceptable, switch the donor to **canonical-string CID
> comparison** (drop `go-cid` from `transfer.Verify`, compare normalized strings)
> and allowlist only `importspec`. Decide from the actual gate output.

- [ ] **Step 3: Verify green + demonstrate red**

Run:
```bash
bash scripts/check_node_deps.sh   # OK
# demonstrate the gate still bites: temporarily add `import _ "github.com/nova-archive/nova/internal/ipfs"` to a cmd/node file
bash scripts/check_node_deps.sh   # FAIL listing internal/ipfs (embedded Kubo tree)
# revert the temporary import
```
Expected: clean run OK; injected embedded-Kubo import FAILs.

- [ ] **Step 4: Commit**

```bash
git add scripts/check_node_deps.sh
git commit -m "build(p2-m4): extend donor dep boundary for importspec + go-cid (reviewed) (P2-M4)"
```

---

## Task 15: e2e loopback-mTLS replication integration test

**Files:**
- Create or extend: `internal/federation/coordinator/integration_test.go`
  (add `TestM4ReplicationVerticalSlice` reusing the M2/M3 loopback harness +
  a real `EmbeddedBackend` and a real `ipfsclient` against a test Kubo, OR a
  fake-Kubo `Pinner` that echoes the coordinator's computed root CID).

**Interfaces:** end-to-end over the in-process federation `Server` + a registered
donor agent.

- [ ] **Step 1: Write the test**

```go
func TestM4ReplicationVerticalSlice(t *testing.T) {
	env := newFedTestEnv(t)
	signer, _ := tokens.NewSignerFromSeed(make([]byte, 32))
	// real embedded backend with one imported blob:
	be := env.embeddedBackend(t)
	res, _ := be.AddDeterministic(env.ctx, []byte("opaque-v1-ciphertext"))
	cidStr := res.CID.String()
	// register the blob row so GetBlobByteSize + AssignPin work:
	env.insertBlob(t, cidStr, int64(len("opaque-v1-ciphertext")))
	env.srv.SetSourceDeps(signer, be, time.Now().Add(-time.Minute))

	// assign to the donor and let the donor run one sync + replicate:
	env.assign(t, cidStr, env.nodeID)
	donor := env.newDonorAgent(t) // wires HTTPClient as Client+SourceFetcher; pinner = real ipfsclient OR echo-pinner
	donor.SyncOnce(env.ctx)
	donor.ReplicatePending(env.ctx)

	// the coordinator now sees a VERIFIED HOLDER (state=acked):
	state := env.pinState(t, cidStr, env.nodeID)
	if state != "acked" {
		t.Fatalf("expected acked holder, got %q", state)
	}
}

func TestM4CrashBeforeAckRecovers(t *testing.T) {
	// progress=verified-ack-pending persisted, ack never sent; new agent instance
	// runs ReconcilePendingAcks → coordinator transitions to acked (idempotent).
}

func TestM4CIDMismatchPostsFail(t *testing.T) {
	// pinner returns a different root → donor posts fail(cid_mismatch); pin state
	// becomes 'failed'; novactl pin list shows no verified holder.
}
```

> Reuse the harness helpers the existing `integration_test.go` already provides
> (`newFedTestEnv`, mTLS client, `assign`, `pinState`). The "echo-pinner" test
> double (a `transfer.Pinner` that returns the same root the coordinator
> computed) avoids requiring a live Kubo in CI while still exercising the full
> token→fetch→verify→persist→ack path.

- [ ] **Step 2: Run + commit**

Run: `go test ./internal/federation/coordinator/ -run TestM4 -v`
Expected: PASS.

```bash
git add internal/federation/coordinator/integration_test.go
git commit -m "test(p2-m4): e2e coordinator-as-source replication + crash recovery + fail paths (P2-M4)"
```

---

## Task 16: `cmd/coordinator` wiring + docs

**Files:**
- Modify: `cmd/coordinator/main.go`
- Modify: `docs/specs/FEDERATION_PROTOCOL.md`, `docs/THREAT_MODEL.md`,
  `docs/ROADMAP.md`, `docs/superpowers/specs/phase2/2026-06-11-phase2-federation-design.md`,
  `docs/quickstart/donor.md`, `docs/VOLUNTEER_DEPLOYMENT_GUIDANCE.md`,
  `deploy/donor/compose.yaml`

- [ ] **Step 1: Wire the coordinator federation source deps**

In `cmd/coordinator/main.go`, where the federation `Server` is constructed:

```go
	if fedCfg.Enabled() {
		signer, err := tokens.LoadSigner(opcfg.Federation.RepairSigningKeyPath)
		if err != nil {
			return fmt.Errorf("federation repair signer: %w", err)
		}
		// fed Config gains RepairTokenTTL/MaxTransferBytes/SourceNebulaAddr + required cap blob-transfer/v1
		fedSrv := coordinator.New(q, coordinator.Config{
			/* existing M2/M3 fields */,
			RequiredCapabilities: []string{wire.CapPinChangeLog, wire.CapSnapshot, wire.CapBlobTransfer},
			RepairTokenTTL:       opcfg.Federation.RepairTokenTTL(),
			MaxTransferBytes:     opcfg.Federation.MaxTransfer(),
			SourceNebulaAddr:     opcfg.Federation.SourceNebulaAddr,
		})
		fedSrv.SetSourceDeps(signer, backend /* the embedded ipfs.Backend already built for the app */, time.Now())
		// existing Listen()/Run() wiring unchanged
	}
```

- [ ] **Step 2: Build + full test + gates**

Run:
```bash
go build ./...
go test ./internal/federation/... ./internal/node/... ./internal/config/... ./internal/ipfs/...
bash scripts/check_node_deps.sh
bash scripts/check-migrations-frozen.sh
```
Expected: all PASS / OK.

- [ ] **Step 3: Docs**

- `FEDERATION_PROTOCOL.md` — confirm the "Donor inbound endpoint" / token-claim
  text matches the implemented coordinator-as-source path (it is largely
  P2-M0-amended already); add a one-line note that M4 ships **coordinator-as-source
  only**, donor-as-source is M5, and the reserved coordinator source id.
- `THREAT_MODEL.md` § "Phase 2 amendment" — note the `source_boot_time` +
  in-memory `jti` replay defense and that the D11 egress budget is exercised in M5.
- `ROADMAP.md` — add the **P2-M4** row (status, tag-to-be `p2-m4-replication-slice`,
  design/plan links) and **name P2-M4.1** (donor-backed reads + quorum + pruning +
  storage modes; **required before P2-M5**; the P2-M2.1 redirect is not fulfilled
  until M4.1).
- master federation design milestone table — add a note that M4 is the
  replication slice and M4.1 carries the storage/read redirect.
- `deploy/donor/compose.yaml` + `docs/quickstart/donor.md` +
  `VOLUNTEER_DEPLOYMENT_GUIDANCE.md` — add the hardened Kubo **sidecar** (private
  swarm key, no public ports, loopback API), `storage_max_bytes`, and
  `kubo_api_addr`.

- [ ] **Step 4: Commit**

```bash
gofmt -w cmd/coordinator/main.go
git add cmd/coordinator/main.go docs/ deploy/donor/compose.yaml
git commit -m "feat(p2-m4): wire coordinator federation source deps + P2-M4/P2-M4.1 docs (P2-M4)"
```

---

## Self-review notes (for the executor)

- **Spec coverage:** D-M4-1 (scope/M4.1) → Task 16 docs; D-M4-2 (coord-as-source +
  reserved id) → Tasks 3,6; D-M4-3 (verify + size bounds) → Tasks 4,6,9; D-M4-4 (no
  migration) → Global Constraints + Task 5 (read-only query); D-M4-5 (crash-safe +
  Has re-check) → Tasks 10,12; D-M4-6 (storage limit; D11 egress is M5) → Tasks
  12,13; D-M4-7 (mint + seed) → Task 3; D-M4-8 (per-serve Source) → Task 7; D-M4-9
  (boot-time + jti) → Task 6; D-M4-10 (sidecar + importspec) → Tasks 2,8,13,14;
  D-M4-11 (`blob-transfer/v1`) → Tasks 1,7,16; D-M4-12 (client/fetcher split) →
  Tasks 9,11; D-M4-13 (observability) → slog calls across Tasks 6,7,9,12.
- **`CoordinatorSourceID` placement:** defined once in `wire` (donor-safe, Task 1)
  and aliased by `tokens.ReservedCoordinatorSourceID` (Task 3); the donor
  references `wire.CoordinatorSourceID` directly and never imports `tokens`.
- **No-migration invariant:** only Task 5 touches the DB layer, adding a read-only
  query (no migration file changes); `migrations-frozen` verified in Tasks 5 & 16.
- **Boundary invariant:** the donor build graph adds only `internal/ipfs/importspec`
  + `go-cid`/multiformats (Task 14), never `internal/ipfs` (embedded Kubo),
  demonstrated-red.

**Review-patch coverage (2026-06-23 conditional approval):**
- *Must-fix 1 — raw/dag-pb parity:* `ipfsclient.AddDeterministic` branches on
  `importspec.ShouldUseRawCodec` (`block/put` raw vs `add` dag-pb), verified
  against `embedded.go:231`; threshold / threshold+1 / multi-block tests (Task 8).
- *Must-fix 2 — `Has` = pinned:* `/api/v0/pin/ls?type=recursive`, mirroring
  `embedded.go:343`; `+ Unpin` (`pin/rm`) (Task 8).
- *Must-fix 3 — boot-time clamp:* `mintSource` clamps `not_before ≥ source_boot_time`;
  regression test `TestMintedTokenAcceptedImmediatelyAfterBoot` (Task 7).
- *Must-fix 4 — config path used:* `tokens.LoadSigner(defaultPath)` consumes
  `federation.repair_signing_key_path` (Tasks 3, 16).
- *Must-fix 5 — generation + unpin:* `progressMatches` gates skip/reconcile by
  `(assignment_id, generation)`; `HandleUnpin` clears progress + sidecar `Unpin`;
  tests `TestReassignAtNewGenerationIsNotSkipped`, `TestUnpinClearsProgressAndUnpinsLocally`,
  `TestStartupReconcileDropsStaleGenerationProgress` (Tasks 8, 12).
- *Should-fix — oversize detection:* `transfer.Verify` reads `maxBytes+1` and
  refuses an over-grant source **without** importing it (`TestVerifyOversizeNotImported`, Task 9).
- *Should-fix — concrete tests:* `cidLike`/“see helper” removed; narrow
  `blobSource` interface + `mkCID` real-CID helper + `fakeBackend` (Task 6).
- *Should-fix — sourceable state:* `GetBlobByteSize` restricted to `state = 'active'`
  (quarantined/tombstoned/soft-deleted not served) (Task 5).
