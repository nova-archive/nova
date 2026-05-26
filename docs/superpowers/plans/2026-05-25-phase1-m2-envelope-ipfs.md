# Phase 1 M2 — Envelope + IPFS Round-trip Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Encrypt arbitrary bytes with the v1 envelope codec, import the envelope into an embedded Kubo daemon deterministically (per `IPFS_IMPORT_RULES.md`), fetch the bytes back, decrypt, and prove byte-equality. Ship the supporting machinery: master-key wrap/unwrap with version tracking, a refuse-to-start Kubo hardening validator covering every rule in `KUBO_HARDENING.md`, and a Postgres-backed job queue with worker pool to be consumed by later milestones.

**Architecture:** Three orthogonal subsystems land together because the M2 exit test exercises all of them. `internal/envelope` owns the wire format and master-key cryptography but is DB-naïve. `internal/ipfs` owns Kubo embedding and the hardening validator but is envelope-naïve. `internal/jobs` is a generic Postgres queue with no knowledge of envelopes or IPFS — it ships in M2 because every M3+ milestone needs it. The exit test composes the three by hand to prove the seams fit together. No HTTP handlers, no coordinator wiring, and no specific job kinds ship in M2 — those land in M3 (read path) and M4+ (specific kinds).

**Tech Stack:** Go 1.22+ (currently 1.25.x in `go.mod`), `golang.org/x/crypto/chacha20poly1305` for XChaCha20-Poly1305, `github.com/ipfs/kubo` (embedded via `core` + `core/coreapi`), `github.com/ipfs/boxo/files` for `files.NewBytesFile`, `github.com/ipfs/go-cid`, `github.com/multiformats/go-multihash`, `github.com/jackc/pgx/v5` (already vendored) for direct SQL against the jobs table, `goose/v3` (already vendored) for applying embedded migrations in tests, `testcontainers-go` (already vendored) for postgres in integration tests.

**Author:** Bug Plowman (operator), Claude (implementation partner).

**Status:** pending — first M2 task starts after this plan is approved.

---

## Scope

### In scope

- `internal/envelope`:
  - `Codec` interface (versioned; v2 slot reserved).
  - `v1` single-shot XChaCha20-Poly1305 codec per `ENCRYPTION_ENVELOPE.md`.
  - Master-key wrap/unwrap (XChaCha20-Poly1305, 72-byte wrapped key).
  - Multi-version master-key keystore with DB-backed `master_key_versions` bootstrap.
  - Golden test vectors covering header, wrap, round-trip, edge cases.
- `internal/ipfs`:
  - `Backend` interface (Add, Get, Has, Pin, Unpin, BlockstoreHas, BlockGet, Close).
  - `KuboConfig` Go type mirroring the JSON fields the validator inspects.
  - `ValidateConfig(cfg, mode)` covering every row of the `KUBO_HARDENING.md` validator table.
  - `EmbeddedBackend` running an in-process Kubo node, deterministic-import flags hard-wired.
  - Threshold (1 MiB) raw-codec shortcut path.
- `internal/jobs`:
  - `Queue` over the partitioned `jobs` table from migration `0002_jobs.sql`.
  - `Enqueue`, `Lease`, `Complete`, `Fail` (retryable + dead), `ReclaimExpiredLeases`.
  - Worker pool with handler registration, exponential backoff, lease-refresh reclaim ticker.
- End-to-end integration test composing envelope + ipfs + (synthetic) job kind.

### Out of scope for M2

- HTTP handlers, chi router, middleware (M3).
- `pkg/coordinator` lifecycle (M3).
- Specific job kinds — `integrity_audit_run`, `scheduled_tombstone`, `derivative_prewarm`, `master_key_rotate_row`, `webhook_emit`, `signing_key_rotate` (M4+; each ships with its consumer milestone).
- Public-archival opt-out path (uses plaintext bytes directly; covered in M4 when uploads land).
- sqlc tooling. M1 self-review deferred sqlc to M2; this plan defers it again to M3 where the read-path query surface justifies the codegen overhead. The jobs queue uses hand-written pgx queries in M2.
- v2 streaming-AEAD codec — Phase 2.
- Master-key rotation (`novactl keys rotate-master`) — M10. M2 ships the multi-version keystore mechanism so M10 can plug rotation onto it without refactor.

---

## File structure

### Created in M2

| Path | Purpose |
|---|---|
| `internal/envelope/envelope.go` | `Codec` interface, version constants, version-byte dispatcher (`Decode`) |
| `internal/envelope/v1.go` | v1 single-shot XChaCha20-Poly1305 codec |
| `internal/envelope/keywrap.go` | Wrap/Unwrap with master key (XChaCha20-Poly1305, AAD="") |
| `internal/envelope/keystore.go` | DB-aware multi-version master-key holder; `Bootstrap`, `Wrap`, `Unwrap` |
| `internal/envelope/errors.go` | Sentinel errors (`ErrEnvelopeUnsupported`, `ErrEnvelopeTooShort`, etc.) |
| `internal/envelope/envelope_test.go` | Codec interface + dispatch tests |
| `internal/envelope/v1_test.go` | v1 round-trip + edge-case tests |
| `internal/envelope/keywrap_test.go` | Wrap/unwrap unit tests |
| `internal/envelope/keystore_test.go` | Keystore integration tests (testcontainers) |
| `internal/envelope/vectors_test.go` | Golden-vector tests reading `testdata/vectors.json` |
| `internal/envelope/testdata/vectors.json` | Authoritative test vectors (deterministic seeded RNG) |
| `internal/ipfs/backend.go` | `Backend` interface + `Mode` enum + helper types |
| `internal/ipfs/importrules.go` | Deterministic Add option constants; threshold (1 MiB) |
| `internal/ipfs/validate.go` | `KuboConfig` struct + `ValidateConfig(cfg, mode)` |
| `internal/ipfs/embedded.go` | `EmbeddedBackend` constructor + methods |
| `internal/ipfs/errors.go` | Sentinel errors (`ErrCIDNotPinned`, `ErrBlockNotFound`, etc.) |
| `internal/ipfs/validate_test.go` | Table-driven hardening-rule violation tests |
| `internal/ipfs/embedded_test.go` | Round-trip integration tests (offline node) |
| `internal/jobs/types.go` | `Job` struct, `State` constants, `Handler` func type |
| `internal/jobs/queue.go` | `Queue` (`Enqueue`, `Lease`, `Complete`, `Fail`, `ReclaimExpiredLeases`) |
| `internal/jobs/worker.go` | `WorkerPool` (`RegisterHandler`, `Run`) |
| `internal/jobs/backoff.go` | Exponential backoff helper with cap |
| `internal/jobs/errors.go` | Sentinel errors (`ErrNoJobsAvailable`, `ErrJobNotFound`, etc.) |
| `internal/jobs/queue_test.go` | Queue integration tests (testcontainers) |
| `internal/jobs/worker_test.go` | Worker pool integration tests (testcontainers) |
| `internal/jobs/backoff_test.go` | Backoff unit tests |
| `internal/dbtest/dbtest.go` | Shared test helper: spin up postgres + apply migrations |
| `internal/integration/m2_roundtrip_test.go` | End-to-end M2 exit test |

### Modified in M2

| Path | Why |
|---|---|
| `go.mod` / `go.sum` | Add `github.com/ipfs/kubo`, `github.com/ipfs/boxo`, `github.com/ipfs/go-cid`, `github.com/multiformats/go-multihash` |
| `docs/superpowers/plans/2026-05-25-phase1-single-node-mvp.md` | Mark M2 status `completed`, link this plan |

### Not created in M2 (planned but deferred)

- `internal/envelope/v2.go` — reserved slot for Phase 2 streaming AEAD. The version dispatcher in `envelope.go` returns `ErrEnvelopeUnsupported` for `version == 0x02` to keep the surface bit-stable.
- `internal/db/queries/*.sql`, `internal/db/sqlc.yaml`, `internal/db/gen/` — sqlc tooling deferred to M3.

---

## Architectural decisions for M2

### Envelope layering

```
                      ┌─────────────────────────────────┐
                      │  internal/envelope/keystore.go  │  (DB-aware)
                      │  ─ DB pool ←──── pgxpool         │
                      │  ─ active version label          │
                      │  ─ Bootstrap(ctx)                │
                      │  ─ Wrap(plaintext) → wrapped,id  │
                      │  ─ Unwrap(wrapped,id) → plain    │
                      └────────────────┬────────────────┘
                                       │ delegates to
                      ┌────────────────▼────────────────┐
                      │  internal/envelope/keywrap.go   │  (pure crypto)
                      │  ─ Wrap(masterKey, perBlobKey)   │
                      │  ─ Unwrap(masterKey, wrapped)    │
                      └─────────────────────────────────┘
                      ┌─────────────────────────────────┐
                      │  internal/envelope/envelope.go  │  (interface)
                      │  ─ Codec interface               │
                      │  ─ Decode(bytes) → version,Codec │
                      └────────────────┬────────────────┘
                                       │ implements
                      ┌────────────────▼────────────────┐
                      │  internal/envelope/v1.go        │  (pure crypto)
                      │  ─ v1Codec.Encrypt               │
                      │  ─ v1Codec.Decrypt               │
                      └─────────────────────────────────┘
```

`keywrap.go` and `v1.go` are stateless pure functions over byte slices. They are unit-testable without a DB or Kubo. `keystore.go` is the only DB-aware piece; it composes the pure functions and adds version tracking.

### IPFS backend layering

```
                      ┌─────────────────────────────────┐
                      │  internal/ipfs/backend.go       │  (interface only)
                      │  ─ Backend interface             │
                      │  ─ Mode enum (Private|PublicDHT) │
                      └────────────────┬────────────────┘
                                       │ implements
                      ┌────────────────▼────────────────┐
                      │  internal/ipfs/embedded.go      │  (in-process Kubo)
                      │  ─ EmbeddedBackend               │
                      │  ─ fsrepo + core.NewNode + api   │
                      └─────────────────────────────────┘
                      ┌─────────────────────────────────┐
                      │  internal/ipfs/validate.go      │  (pure logic)
                      │  ─ KuboConfig struct             │
                      │  ─ ValidateConfig(cfg, mode)     │
                      └─────────────────────────────────┘
                      ┌─────────────────────────────────┐
                      │  internal/ipfs/importrules.go   │  (constants)
                      │  ─ ChunkerSizeBytes              │
                      │  ─ RawCodecThresholdBytes        │
                      │  ─ AddOpts function              │
                      └─────────────────────────────────┘
```

`ValidateConfig` is pure logic against a `KuboConfig` struct so the table-driven test in `validate_test.go` does not need a running Kubo node. `EmbeddedBackend.Run` calls `ValidateConfig` on its own loaded config before starting the node.

### Jobs queue contract

The queue exposes a small surface designed so M4+ milestones can register kinds without touching `internal/jobs`:

```go
type Handler func(ctx context.Context, payload []byte) error

type Queue interface {
    Enqueue(ctx context.Context, kind string, payload []byte, opts ...EnqueueOpt) (jobID string, err error)
    Lease(ctx context.Context, leaseDuration time.Duration) (*Job, error)
    Complete(ctx context.Context, jobID string) error
    Fail(ctx context.Context, jobID string, errMsg string) error
    ReclaimExpiredLeases(ctx context.Context) (count int, err error)
}
```

`Fail` decides internally whether the failure transitions the job to `pending` (retryable, `attempts < max_attempts`) or `dead`, applying exponential backoff to `not_before` on retry. This keeps every retry policy in one place.

`WorkerPool.Run(ctx)` blocks until `ctx` is cancelled. It runs N concurrent leasing goroutines plus one reclaim-ticker goroutine. Workers call `Lease` in a loop with a 250 ms polling interval (matching the design's `Job lifecycle` section).

---

## Risks and notes for the implementer

1. **Kubo dependency weight.** Pulling `github.com/ipfs/kubo` into the module brings hundreds of transitive deps and inflates `go.sum` substantially. If the build fails for memory or disk reasons, the fallback is to vendor `github.com/ipfs/boxo` directly (UnixFS + blockstore live there) and instantiate the import pipeline without the full Kubo `core.IpfsNode`. The M2 exit criterion mandates Kubo coreapi specifically — only pivot to boxo-only if Kubo embedding is materially infeasible, and surface the change to Bug before committing.

2. **Kubo version drift.** CID-stability is guaranteed within a Kubo major version per `IPFS_IMPORT_RULES.md` § "What MAY vary". We `go get @latest` here; the M2 implementer records the resolved version in the dependency-add commit message. Subsequent Kubo updates require re-running the integration test to verify CIDs still match.

3. **Test slowness.** The `EmbeddedBackend` integration tests spin up a Kubo node per test (offline mode, ~2-5 s). The M2 end-to-end test additionally spins up a postgres container. Use `t.Parallel()` aggressively where independent; aim for total M2 test runtime ≤ 90 s on commodity hardware. If it exceeds 3 min, refactor toward a shared per-package Kubo node with `TestMain`.

4. **Bootstrap of `master_key_versions`.** The design notes that `cmd/migrate` "bootstraps `master_key_versions` row v1 on first run". M2 implements this differently: the bootstrap row is created by `envelope.Keystore.Bootstrap(ctx)` because `cmd/migrate` deliberately does not load `NOVA_MASTER_KEY` (migrate is run by ops without secrets). The coordinator (M3) will call `Keystore.Bootstrap` at startup. Document this in `OPERATOR_CHECKLIST.md` at M14.

5. **No XChaCha20-Poly1305 helper in stdlib.** Use `golang.org/x/crypto/chacha20poly1305.NewX(key)` which returns the 24-byte-nonce variant. The 12-byte-nonce `New(key)` is the wrong primitive — using it silently would produce non-spec-compliant envelopes.

6. **Deterministic test vectors.** Vectors live in `testdata/vectors.json` and are committed. Generation uses a fixed seed for the per-blob-key and nonces; the generator function is in `vectors_test.go` and is gated behind a `-update` flag (Go's standard `testdata/golden` pattern). Hand-editing the JSON is forbidden — implementers regenerate with the flag and review the diff.

---

## M2 — Envelope + IPFS round-trip: Detailed Tasks

### Task 1: Add M2 Go dependencies

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1.1: Verify current module state**

```bash
go list -m | head -5
go mod tidy
```

Expected: `github.com/nova-archive/nova` is listed; tidy is a no-op (M1 left `go.sum` clean).

- [ ] **Step 1.2: Add Kubo and friends**

```bash
go get github.com/ipfs/kubo@latest
go get github.com/ipfs/boxo@latest
go get github.com/ipfs/go-cid@latest
go get github.com/multiformats/go-multihash@latest
```

If any `go get` fails (Kubo's transitive deps can collide with pgx's via OpenTelemetry versions), surface to Bug rather than forcing with `replace` directives.

- [ ] **Step 1.3: Tidy and record resolved versions**

```bash
go mod tidy
go list -m github.com/ipfs/kubo github.com/ipfs/boxo github.com/ipfs/go-cid github.com/multiformats/go-multihash
```

Expected: all four resolve; record the versions for the commit message.

- [ ] **Step 1.4: Smoke-compile**

```bash
go build ./...
```

Expected: builds. (No M2 source files yet; this just confirms the new deps compile with the existing module.)

- [ ] **Step 1.5: Commit**

```bash
git add go.mod go.sum
git commit -s -m "chore: add Phase 1 M2 deps (ipfs/kubo, ipfs/boxo, go-cid, go-multihash)

Records resolved versions in the commit body for future reference:
  github.com/ipfs/kubo            <resolved>
  github.com/ipfs/boxo            <resolved>
  github.com/ipfs/go-cid          <resolved>
  github.com/multiformats/go-multihash <resolved>"
```

Fill in the `<resolved>` strings before committing.

---

### Task 2: Envelope errors + Codec interface + version dispatcher (TDD)

**Files:**
- Create: `internal/envelope/errors.go`
- Create: `internal/envelope/envelope.go`
- Create: `internal/envelope/envelope_test.go`

- [ ] **Step 2.1: Write the failing test**

Create `internal/envelope/envelope_test.go`:

```go
package envelope_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/nova-archive/nova/internal/envelope"
	"github.com/stretchr/testify/require"
)

func TestDecodeRejectsTooShortBytes(t *testing.T) {
	t.Parallel()
	// Header is 32 bytes; anything less cannot be a valid envelope.
	for _, n := range []int{0, 1, 31} {
		buf := bytes.Repeat([]byte{0xAA}, n)
		_, _, err := envelope.Decode(buf)
		require.ErrorIs(t, err, envelope.ErrEnvelopeTooShort, "n=%d", n)
	}
}

func TestDecodeRejectsBadMagic(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 48)        // header + 16-byte tag minimum
	copy(buf[:4], []byte("XXXX"))  // not "NOVE"
	buf[4] = 0x01                  // version
	buf[5] = 0x01                  // algorithm
	_, _, err := envelope.Decode(buf)
	require.ErrorIs(t, err, envelope.ErrEnvelopeBadMagic)
}

func TestDecodeRejectsUnsupportedVersion(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 48)
	copy(buf[:4], []byte("NOVE"))
	buf[4] = 0xFF                  // unknown version
	buf[5] = 0x01
	_, _, err := envelope.Decode(buf)
	require.ErrorIs(t, err, envelope.ErrEnvelopeUnsupported)
}

func TestDecodeRejectsNonZeroReserved(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 48)
	copy(buf[:4], []byte("NOVE"))
	buf[4] = 0x01
	buf[5] = 0x01
	buf[6] = 0x00
	buf[7] = 0x01                  // reserved must be 0x0000
	_, _, err := envelope.Decode(buf)
	require.ErrorIs(t, err, envelope.ErrEnvelopeUnsupported)
}

func TestDecodeRejectsBadAlgorithm(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 48)
	copy(buf[:4], []byte("NOVE"))
	buf[4] = 0x01
	buf[5] = 0x02                  // unknown algorithm
	_, _, err := envelope.Decode(buf)
	require.ErrorIs(t, err, envelope.ErrEnvelopeUnsupported)
}

func TestDecodeReturnsV1CodecOnValidHeader(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 48)
	copy(buf[:4], []byte("NOVE"))
	buf[4] = 0x01
	buf[5] = 0x01
	version, codec, err := envelope.Decode(buf)
	require.NoError(t, err)
	require.Equal(t, byte(0x01), version)
	require.NotNil(t, codec)
	require.Equal(t, byte(0x01), codec.Version())
}

func TestErrSentinelsAreDistinct(t *testing.T) {
	t.Parallel()
	require.False(t, errors.Is(envelope.ErrEnvelopeTooShort, envelope.ErrEnvelopeBadMagic))
	require.False(t, errors.Is(envelope.ErrEnvelopeBadMagic, envelope.ErrEnvelopeUnsupported))
}
```

- [ ] **Step 2.2: Run test, verify it fails**

```bash
go test ./internal/envelope/...
```

Expected: FAIL — package does not yet declare `Decode`, `Codec`, sentinel errors.

- [ ] **Step 2.3: Create errors.go**

```go
// Package envelope owns Nova's encryption envelope wire format.
//
// The v1 wire format is specified in docs/specs/ENCRYPTION_ENVELOPE.md.
// Implementations of the Codec interface MUST be byte-for-byte
// compatible with the spec; integrity audits will catch drift.
//
// The package is deliberately DB-naïve: only keystore.go imports the
// pgx pool. The pure-crypto pieces (v1.go, keywrap.go) accept byte
// slices and master-key bytes directly so they can be exercised in
// fast unit tests and re-used wherever needed (e.g., novactl tooling).
package envelope

import "errors"

// Sentinel errors returned by Decode and the Codec.Decrypt path.
// Callers compare with errors.Is to map to HTTP statuses or audit
// failures.
var (
	// ErrEnvelopeTooShort: byte slice is shorter than the 32-byte
	// header. Maps to a 400 Bad Request at the API layer; in the
	// integrity audit this surfaces as envelope_decode = fail.
	ErrEnvelopeTooShort = errors.New("envelope: too short")

	// ErrEnvelopeBadMagic: header does not start with ASCII "NOVE".
	ErrEnvelopeBadMagic = errors.New("envelope: bad magic")

	// ErrEnvelopeUnsupported: header parses but the version, algorithm,
	// or reserved bytes are not recognized by this implementation.
	// Distinct from BadMagic to support future migrations.
	ErrEnvelopeUnsupported = errors.New("envelope: unsupported version or algorithm")

	// ErrEnvelopeAuthFailed: AEAD authentication tag failed. Always
	// treat as adversarial: do not retry, do not leak detail.
	ErrEnvelopeAuthFailed = errors.New("envelope: authentication failed")

	// ErrKeyWrongLength: per-blob or master key is not 32 bytes.
	ErrKeyWrongLength = errors.New("envelope: key wrong length")

	// ErrWrappedKeyWrongLength: wrapped key blob is not 72 bytes.
	ErrWrappedKeyWrongLength = errors.New("envelope: wrapped key wrong length")
)
```

- [ ] **Step 2.4: Create envelope.go**

```go
package envelope

const (
	// HeaderSize is the fixed envelope header length: 4 (magic) +
	// 1 (version) + 1 (algorithm) + 2 (reserved) + 24 (nonce).
	HeaderSize = 32

	// TagSize is the Poly1305 authentication tag length.
	TagSize = 16

	// NonceSize is the XChaCha20 extended-nonce length.
	NonceSize = 24

	// KeySize is the symmetric key length for both per-blob and
	// master keys.
	KeySize = 32

	// WrappedKeySize is wrap_nonce (24) || ct_of_key (32) || tag (16).
	WrappedKeySize = 72

	// VersionV1 = 0x01 per ENCRYPTION_ENVELOPE.md.
	VersionV1 byte = 0x01

	// VersionV2 = 0x02. Reserved for the Phase 2 streaming-AEAD codec.
	// The dispatcher recognises the byte and returns ErrEnvelopeUnsupported
	// in Phase 1 so future v2 envelopes do not silently round-trip as v1.
	VersionV2 byte = 0x02

	// AlgorithmXChaCha20Poly1305 = 0x01.
	AlgorithmXChaCha20Poly1305 byte = 0x01
)

// magic is ASCII "NOVE" — the envelope header marker.
var magic = [4]byte{0x4E, 0x4F, 0x56, 0x45}

// Codec is the abstract operation set every envelope version provides.
// v1 (Phase 1) implements the single-shot variant; v2 (Phase 2) will
// add a streaming Decrypter without changing this interface — the
// streaming variant will be a separate optional interface that v1
// does not implement.
type Codec interface {
	// Version returns the envelope-format version byte this codec emits
	// and accepts. Useful for assertions and audit logging.
	Version() byte

	// Encrypt produces the wire-format envelope for the plaintext under
	// the given per-blob key. The returned bytes are:
	//   header(32) || ciphertext(len(plaintext)) || tag(16)
	// Implementations MUST generate a fresh random nonce per call.
	Encrypt(plaintext, perBlobKey []byte) ([]byte, error)

	// Decrypt verifies and returns the plaintext for envelope bytes under
	// the given per-blob key. Authentication failures return
	// ErrEnvelopeAuthFailed; format failures return one of the
	// ErrEnvelope* sentinels.
	Decrypt(envelope, perBlobKey []byte) ([]byte, error)
}

// Decode parses the envelope header, validates magic/version/algorithm/
// reserved bytes, and returns the version byte plus the Codec capable of
// decrypting the envelope. It does not perform decryption — callers must
// provide the per-blob key separately.
//
// Returns ErrEnvelopeTooShort, ErrEnvelopeBadMagic, or
// ErrEnvelopeUnsupported on header validation failure.
func Decode(b []byte) (version byte, codec Codec, err error) {
	if len(b) < HeaderSize {
		return 0, nil, ErrEnvelopeTooShort
	}
	if !(b[0] == magic[0] && b[1] == magic[1] && b[2] == magic[2] && b[3] == magic[3]) {
		return 0, nil, ErrEnvelopeBadMagic
	}
	v := b[4]
	algo := b[5]
	reserved := uint16(b[6])<<8 | uint16(b[7])
	if reserved != 0 {
		return 0, nil, ErrEnvelopeUnsupported
	}
	switch v {
	case VersionV1:
		if algo != AlgorithmXChaCha20Poly1305 {
			return 0, nil, ErrEnvelopeUnsupported
		}
		return v, V1(), nil
	case VersionV2:
		// Reserved for Phase 2 streaming AEAD. Phase 1 refuses cleanly
		// rather than reaching for a codec that does not exist.
		return 0, nil, ErrEnvelopeUnsupported
	default:
		return 0, nil, ErrEnvelopeUnsupported
	}
}
```

(The `V1()` function returns the v1 codec; it lives in `v1.go` and gets implemented in Task 3.)

- [ ] **Step 2.5: Create a stub v1.go so the package compiles**

Create `internal/envelope/v1.go` with just enough to compile (full impl lands in Task 3):

```go
package envelope

// v1Codec implements the single-shot XChaCha20-Poly1305 envelope from
// docs/specs/ENCRYPTION_ENVELOPE.md § "Envelope wire format". The full
// implementation lands in Task 3; this stub exists so the version
// dispatcher in envelope.go links.
type v1Codec struct{}

// V1 returns the singleton v1 codec.
func V1() Codec { return v1Codec{} }

func (v1Codec) Version() byte { return VersionV1 }

func (v1Codec) Encrypt(plaintext, perBlobKey []byte) ([]byte, error) {
	panic("v1Codec.Encrypt: not implemented yet (Task 3)")
}

func (v1Codec) Decrypt(envelope, perBlobKey []byte) ([]byte, error) {
	panic("v1Codec.Decrypt: not implemented yet (Task 3)")
}
```

- [ ] **Step 2.6: Run test, verify it passes**

```bash
go test ./internal/envelope/...
```

Expected: PASS — all six tests green. (The codec stub never gets called by the Task 2 tests; only `Decode` is exercised.)

- [ ] **Step 2.7: Commit**

```bash
git add internal/envelope/errors.go internal/envelope/envelope.go internal/envelope/v1.go internal/envelope/envelope_test.go
git commit -s -m "feat(envelope): Codec interface, version dispatcher, sentinel errors

Header parsing + Decode() routing for v1 (XChaCha20-Poly1305). v2 byte
is reserved and returns ErrEnvelopeUnsupported so Phase 2 streaming
envelopes do not silently round-trip as v1."
```

---

### Task 3: Envelope v1 codec implementation (TDD)

**Files:**
- Modify: `internal/envelope/v1.go` (replace stub)
- Create: `internal/envelope/v1_test.go`

- [ ] **Step 3.1: Write the failing tests**

Create `internal/envelope/v1_test.go`:

```go
package envelope_test

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/nova-archive/nova/internal/envelope"
	"github.com/stretchr/testify/require"
)

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	_, err := rand.Read(buf)
	require.NoError(t, err)
	return buf
}

func TestV1RoundTripEmpty(t *testing.T) {
	t.Parallel()
	key := randBytes(t, envelope.KeySize)
	v1 := envelope.V1()

	env, err := v1.Encrypt(nil, key)
	require.NoError(t, err)

	got, err := v1.Decrypt(env, key)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestV1RoundTripSmall(t *testing.T) {
	t.Parallel()
	key := randBytes(t, envelope.KeySize)
	plain := []byte("hello, nova")
	v1 := envelope.V1()

	env, err := v1.Encrypt(plain, key)
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(env, []byte("NOVE")), "envelope must start with NOVE magic")
	require.Equal(t, envelope.HeaderSize+len(plain)+envelope.TagSize, len(env))

	got, err := v1.Decrypt(env, key)
	require.NoError(t, err)
	require.Equal(t, plain, got)
}

func TestV1RoundTripLarge(t *testing.T) {
	t.Parallel()
	key := randBytes(t, envelope.KeySize)
	plain := randBytes(t, 5*1024*1024) // 5 MiB
	v1 := envelope.V1()

	env, err := v1.Encrypt(plain, key)
	require.NoError(t, err)
	got, err := v1.Decrypt(env, key)
	require.NoError(t, err)
	require.Equal(t, plain, got)
}

func TestV1DistinctNoncesPerEncrypt(t *testing.T) {
	t.Parallel()
	key := randBytes(t, envelope.KeySize)
	plain := []byte("identical bytes")
	v1 := envelope.V1()

	a, err := v1.Encrypt(plain, key)
	require.NoError(t, err)
	b, err := v1.Encrypt(plain, key)
	require.NoError(t, err)

	require.NotEqual(t, a, b,
		"two encryptions of identical plaintext under identical key must differ "+
			"(would otherwise leak equality information via CID collision)")
	// Nonces sit at offset 8..32 in the envelope.
	require.NotEqual(t, a[8:32], b[8:32], "nonces must differ")
}

func TestV1EncryptRejectsWrongKeyLength(t *testing.T) {
	t.Parallel()
	v1 := envelope.V1()
	for _, n := range []int{0, 16, 31, 33, 64} {
		_, err := v1.Encrypt([]byte("hi"), make([]byte, n))
		require.ErrorIs(t, err, envelope.ErrKeyWrongLength, "n=%d", n)
	}
}

func TestV1DecryptRejectsWrongKeyLength(t *testing.T) {
	t.Parallel()
	v1 := envelope.V1()
	key := randBytes(t, envelope.KeySize)
	env, err := v1.Encrypt([]byte("hi"), key)
	require.NoError(t, err)
	for _, n := range []int{0, 16, 31, 33, 64} {
		_, err := v1.Decrypt(env, make([]byte, n))
		require.ErrorIs(t, err, envelope.ErrKeyWrongLength, "n=%d", n)
	}
}

func TestV1DecryptDetectsTampering(t *testing.T) {
	t.Parallel()
	key := randBytes(t, envelope.KeySize)
	plain := []byte("must not survive tampering")
	v1 := envelope.V1()

	env, err := v1.Encrypt(plain, key)
	require.NoError(t, err)

	// Flip a ciphertext byte (after the header, before the tag).
	tampered := append([]byte{}, env...)
	tampered[envelope.HeaderSize] ^= 0x01

	_, err = v1.Decrypt(tampered, key)
	require.ErrorIs(t, err, envelope.ErrEnvelopeAuthFailed)
}

func TestV1DecryptDetectsTagFlip(t *testing.T) {
	t.Parallel()
	key := randBytes(t, envelope.KeySize)
	plain := []byte("must not survive tampering")
	v1 := envelope.V1()

	env, err := v1.Encrypt(plain, key)
	require.NoError(t, err)

	tampered := append([]byte{}, env...)
	tampered[len(tampered)-1] ^= 0x01

	_, err = v1.Decrypt(tampered, key)
	require.ErrorIs(t, err, envelope.ErrEnvelopeAuthFailed)
}

func TestV1DecryptRejectsWrongKey(t *testing.T) {
	t.Parallel()
	keyA := randBytes(t, envelope.KeySize)
	keyB := randBytes(t, envelope.KeySize)
	v1 := envelope.V1()

	env, err := v1.Encrypt([]byte("payload"), keyA)
	require.NoError(t, err)
	_, err = v1.Decrypt(env, keyB)
	require.ErrorIs(t, err, envelope.ErrEnvelopeAuthFailed)
}

func TestV1HeaderShape(t *testing.T) {
	t.Parallel()
	key := randBytes(t, envelope.KeySize)
	v1 := envelope.V1()

	env, err := v1.Encrypt([]byte("x"), key)
	require.NoError(t, err)

	require.Equal(t, byte('N'), env[0])
	require.Equal(t, byte('O'), env[1])
	require.Equal(t, byte('V'), env[2])
	require.Equal(t, byte('E'), env[3])
	require.Equal(t, envelope.VersionV1, env[4])
	require.Equal(t, envelope.AlgorithmXChaCha20Poly1305, env[5])
	require.Equal(t, byte(0), env[6])
	require.Equal(t, byte(0), env[7])
}
```

- [ ] **Step 3.2: Run tests, verify they fail**

```bash
go test ./internal/envelope/...
```

Expected: FAIL with panic — the v1 codec is still stubbed.

- [ ] **Step 3.3: Implement v1.go**

Replace `internal/envelope/v1.go`:

```go
package envelope

import (
	"crypto/rand"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

type v1Codec struct{}

// V1 returns the singleton v1 codec.
func V1() Codec { return v1Codec{} }

func (v1Codec) Version() byte { return VersionV1 }

// Encrypt produces a v1 envelope: NOVE || 0x01 || 0x01 || 0x0000 ||
// nonce(24) || ciphertext || tag(16). The per-blob key MUST be 32 bytes.
// A fresh random nonce is generated each call.
func (v1Codec) Encrypt(plaintext, perBlobKey []byte) ([]byte, error) {
	if len(perBlobKey) != KeySize {
		return nil, ErrKeyWrongLength
	}
	aead, err := chacha20poly1305.NewX(perBlobKey)
	if err != nil {
		return nil, fmt.Errorf("envelope v1: aead init: %w", err)
	}

	out := make([]byte, HeaderSize, HeaderSize+len(plaintext)+TagSize)
	copy(out[0:4], magic[:])
	out[4] = VersionV1
	out[5] = AlgorithmXChaCha20Poly1305
	// out[6], out[7] are already zero (reserved)
	if _, err := rand.Read(out[8:HeaderSize]); err != nil {
		return nil, fmt.Errorf("envelope v1: rand nonce: %w", err)
	}

	// Seal appends ciphertext+tag to dst.
	nonce := out[8:HeaderSize]
	out = aead.Seal(out, nonce, plaintext, nil)
	return out, nil
}

// Decrypt verifies the AEAD tag and returns the plaintext. Returns
// ErrEnvelopeTooShort/ErrEnvelopeBadMagic/ErrEnvelopeUnsupported on
// header trouble, ErrKeyWrongLength on bad key, ErrEnvelopeAuthFailed
// on tag mismatch.
func (v1Codec) Decrypt(env, perBlobKey []byte) ([]byte, error) {
	if len(perBlobKey) != KeySize {
		return nil, ErrKeyWrongLength
	}
	// We accept callers having pre-validated the header via Decode, but
	// re-validate here because callers may invoke Decrypt directly when
	// they already know the codec.
	if len(env) < HeaderSize+TagSize {
		return nil, ErrEnvelopeTooShort
	}
	if !(env[0] == magic[0] && env[1] == magic[1] && env[2] == magic[2] && env[3] == magic[3]) {
		return nil, ErrEnvelopeBadMagic
	}
	if env[4] != VersionV1 || env[5] != AlgorithmXChaCha20Poly1305 {
		return nil, ErrEnvelopeUnsupported
	}
	if env[6] != 0 || env[7] != 0 {
		return nil, ErrEnvelopeUnsupported
	}

	aead, err := chacha20poly1305.NewX(perBlobKey)
	if err != nil {
		return nil, fmt.Errorf("envelope v1: aead init: %w", err)
	}

	nonce := env[8:HeaderSize]
	ctAndTag := env[HeaderSize:]
	plaintext, err := aead.Open(nil, nonce, ctAndTag, nil)
	if err != nil {
		// chacha20poly1305 returns its own opaque error on tag mismatch.
		// We normalise to the sentinel so callers can errors.Is against it.
		return nil, errors.Join(ErrEnvelopeAuthFailed, err)
	}
	return plaintext, nil
}
```

- [ ] **Step 3.4: Run tests, verify they pass**

```bash
go test ./internal/envelope/...
```

Expected: PASS — all v1 tests + the Task 2 dispatcher tests pass.

- [ ] **Step 3.5: Commit**

```bash
git add internal/envelope/v1.go internal/envelope/v1_test.go
git commit -s -m "feat(envelope): v1 single-shot XChaCha20-Poly1305 codec

Implements ENCRYPTION_ENVELOPE.md § 'Envelope wire format'. Fresh
random nonces per call (deterministic nonces are explicitly forbidden
by the spec). Tag-mismatch tests, wrong-key tests, header tamper tests
included in v1_test.go."
```

---

### Task 4: Master-key wrap/unwrap (TDD)

**Files:**
- Create: `internal/envelope/keywrap.go`
- Create: `internal/envelope/keywrap_test.go`

- [ ] **Step 4.1: Write the failing tests**

Create `internal/envelope/keywrap_test.go`:

```go
package envelope_test

import (
	"crypto/rand"
	"testing"

	"github.com/nova-archive/nova/internal/envelope"
	"github.com/stretchr/testify/require"
)

func TestWrapUnwrapRoundTrip(t *testing.T) {
	t.Parallel()
	mk := make([]byte, envelope.KeySize)
	pbk := make([]byte, envelope.KeySize)
	_, err := rand.Read(mk)
	require.NoError(t, err)
	_, err = rand.Read(pbk)
	require.NoError(t, err)

	wrapped, err := envelope.WrapKey(mk, pbk)
	require.NoError(t, err)
	require.Equal(t, envelope.WrappedKeySize, len(wrapped), "wrapped key MUST be 72 bytes")

	got, err := envelope.UnwrapKey(mk, wrapped)
	require.NoError(t, err)
	require.Equal(t, pbk, got)
}

func TestWrapDistinctNoncesPerCall(t *testing.T) {
	t.Parallel()
	mk := make([]byte, envelope.KeySize)
	pbk := make([]byte, envelope.KeySize)
	_, _ = rand.Read(mk)
	_, _ = rand.Read(pbk)

	a, err := envelope.WrapKey(mk, pbk)
	require.NoError(t, err)
	b, err := envelope.WrapKey(mk, pbk)
	require.NoError(t, err)

	require.NotEqual(t, a, b, "two wraps of the same key under the same master key MUST differ")
}

func TestWrapRejectsBadKeyLength(t *testing.T) {
	t.Parallel()
	good := make([]byte, envelope.KeySize)

	for _, n := range []int{0, 16, 31, 33, 64} {
		_, err := envelope.WrapKey(make([]byte, n), good)
		require.ErrorIs(t, err, envelope.ErrKeyWrongLength, "bad master key n=%d", n)
		_, err = envelope.WrapKey(good, make([]byte, n))
		require.ErrorIs(t, err, envelope.ErrKeyWrongLength, "bad per-blob key n=%d", n)
	}
}

func TestUnwrapRejectsBadWrappedLength(t *testing.T) {
	t.Parallel()
	mk := make([]byte, envelope.KeySize)
	_, _ = rand.Read(mk)
	for _, n := range []int{0, 24, 48, 71, 73, 100} {
		_, err := envelope.UnwrapKey(mk, make([]byte, n))
		require.ErrorIs(t, err, envelope.ErrWrappedKeyWrongLength, "n=%d", n)
	}
}

func TestUnwrapRejectsWrongMasterKey(t *testing.T) {
	t.Parallel()
	mkA := make([]byte, envelope.KeySize)
	mkB := make([]byte, envelope.KeySize)
	pbk := make([]byte, envelope.KeySize)
	_, _ = rand.Read(mkA)
	_, _ = rand.Read(mkB)
	_, _ = rand.Read(pbk)

	wrapped, err := envelope.WrapKey(mkA, pbk)
	require.NoError(t, err)
	_, err = envelope.UnwrapKey(mkB, wrapped)
	require.ErrorIs(t, err, envelope.ErrEnvelopeAuthFailed)
}

func TestUnwrapRejectsTamperedWrapped(t *testing.T) {
	t.Parallel()
	mk := make([]byte, envelope.KeySize)
	pbk := make([]byte, envelope.KeySize)
	_, _ = rand.Read(mk)
	_, _ = rand.Read(pbk)

	wrapped, err := envelope.WrapKey(mk, pbk)
	require.NoError(t, err)

	tampered := append([]byte{}, wrapped...)
	tampered[envelope.NonceSize] ^= 0x01 // flip a ciphertext byte

	_, err = envelope.UnwrapKey(mk, tampered)
	require.ErrorIs(t, err, envelope.ErrEnvelopeAuthFailed)
}
```

- [ ] **Step 4.2: Run tests, verify they fail**

```bash
go test ./internal/envelope/...
```

Expected: FAIL — `undefined: envelope.WrapKey`.

- [ ] **Step 4.3: Implement keywrap.go**

```go
package envelope

import (
	"crypto/rand"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// WrapKey encrypts a 32-byte per-blob key with the 32-byte operator
// master key using XChaCha20-Poly1305 with empty AAD. The returned
// 72-byte payload is:
//
//   wrap_nonce(24) || ct_of_per_blob_key(32) || tag(16)
//
// This is the byte layout stored in data_encryption_keys.wrapped_key.
//
// Each call generates a fresh random wrap_nonce; identical inputs MUST
// NOT produce identical wrapped outputs.
func WrapKey(masterKey, perBlobKey []byte) ([]byte, error) {
	if len(masterKey) != KeySize {
		return nil, ErrKeyWrongLength
	}
	if len(perBlobKey) != KeySize {
		return nil, ErrKeyWrongLength
	}

	aead, err := chacha20poly1305.NewX(masterKey)
	if err != nil {
		return nil, fmt.Errorf("envelope keywrap: aead init: %w", err)
	}

	out := make([]byte, NonceSize, WrappedKeySize)
	if _, err := rand.Read(out); err != nil {
		return nil, fmt.Errorf("envelope keywrap: rand nonce: %w", err)
	}

	// Seal appends ct+tag to out, growing it to 24+32+16 = 72 bytes.
	out = aead.Seal(out, out[:NonceSize], perBlobKey, nil)
	if len(out) != WrappedKeySize {
		return nil, fmt.Errorf("envelope keywrap: unexpected wrapped length %d", len(out))
	}
	return out, nil
}

// UnwrapKey reverses WrapKey. Returns ErrEnvelopeAuthFailed when the
// master key is wrong or the wrapped bytes were tampered with.
func UnwrapKey(masterKey, wrapped []byte) ([]byte, error) {
	if len(masterKey) != KeySize {
		return nil, ErrKeyWrongLength
	}
	if len(wrapped) != WrappedKeySize {
		return nil, ErrWrappedKeyWrongLength
	}

	aead, err := chacha20poly1305.NewX(masterKey)
	if err != nil {
		return nil, fmt.Errorf("envelope keywrap: aead init: %w", err)
	}

	nonce := wrapped[:NonceSize]
	ctAndTag := wrapped[NonceSize:]
	pbk, err := aead.Open(nil, nonce, ctAndTag, nil)
	if err != nil {
		return nil, errors.Join(ErrEnvelopeAuthFailed, err)
	}
	if len(pbk) != KeySize {
		return nil, fmt.Errorf("envelope keywrap: unexpected unwrapped length %d", len(pbk))
	}
	return pbk, nil
}
```

- [ ] **Step 4.4: Run tests, verify they pass**

```bash
go test ./internal/envelope/...
```

Expected: PASS.

- [ ] **Step 4.5: Commit**

```bash
git add internal/envelope/keywrap.go internal/envelope/keywrap_test.go
git commit -s -m "feat(envelope): master-key wrap/unwrap (XChaCha20-Poly1305, 72-byte wrapped)

Implements ENCRYPTION_ENVELOPE.md § 'Per-blob key generation and
wrapping'. Pure crypto; no DB involvement. The 72-byte wrapped layout
matches data_encryption_keys.wrapped_key."
```

---

### Task 5: Golden test vectors

**Files:**
- Create: `internal/envelope/testdata/vectors.json`
- Create: `internal/envelope/vectors_test.go`

This task pins the on-disk byte format with seeded-RNG vectors so any future change to v1 encoding (header layout, AEAD parameters, nonce position) breaks the build. We use a deterministic in-test RNG to generate the vector file via `go test -update`, and re-check the committed file on every plain `go test` run.

- [ ] **Step 5.1: Write the vector loader test (failing)**

Create `internal/envelope/vectors_test.go`:

```go
package envelope_test

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/nova-archive/nova/internal/envelope"
	"github.com/stretchr/testify/require"
)

// updateVectors regenerates testdata/vectors.json from the seeded RNG.
// Run via `go test ./internal/envelope/... -run TestVectors -update`.
// Review the diff before committing.
var updateVectors = flag.Bool("update", false, "regenerate envelope golden vectors")

type vector struct {
	Name           string `json:"name"`
	MasterKey      string `json:"master_key_hex"`        // 32 bytes hex
	PerBlobKey     string `json:"per_blob_key_hex"`      // 32 bytes hex
	Plaintext      string `json:"plaintext_hex"`         // arbitrary length hex
	WrapSeedHex    string `json:"wrap_seed_hex"`         // 24-byte wrap_nonce hex; deterministic via seeded reader
	EnvelopeSeed   string `json:"envelope_seed_hex"`     // 24-byte envelope nonce hex; deterministic via seeded reader
	WrappedKey     string `json:"wrapped_key_hex"`       // 72-byte hex; expected output
	Envelope       string `json:"envelope_hex"`          // arbitrary length hex; expected output
}

// seededReader is a deterministic byte source for vector generation.
// Implementations of rand.Read use the standard crypto/rand reader; we
// substitute this in tests via the envelope_internal_test.go hook
// declared below.
type seededReader struct {
	src *rand.ChaCha8
}

func newSeededReader(seedHex string) *seededReader {
	seed := mustHex(seedHex)
	var s [32]byte
	copy(s[:], seed)
	return &seededReader{src: rand.NewChaCha8(s)}
}

func (s *seededReader) Read(p []byte) (int, error) {
	return s.src.Read(p)
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func TestVectorsRoundTrip(t *testing.T) {
	if *updateVectors {
		regenerateVectors(t)
		return
	}

	data, err := os.ReadFile(filepath.Join("testdata", "vectors.json"))
	require.NoError(t, err, "run with -update to generate")

	var vectors []vector
	require.NoError(t, json.Unmarshal(data, &vectors))
	require.NotEmpty(t, vectors)

	for _, v := range vectors {
		v := v
		t.Run(v.Name, func(t *testing.T) {
			mk := mustHex(v.MasterKey)
			pbk := mustHex(v.PerBlobKey)
			plain := mustHex(v.Plaintext)
			wantWrapped := mustHex(v.WrappedKey)
			wantEnvelope := mustHex(v.Envelope)

			// Wrapping is deterministic only when WrapKey reads its nonce
			// from a fixed seed; we test instead that unwrap recovers the
			// per-blob key from the committed wrapped bytes.
			got, err := envelope.UnwrapKey(mk, wantWrapped)
			require.NoError(t, err, "unwrap committed wrapped_key with committed master_key")
			require.Equal(t, pbk, got)

			// Same for envelope decrypt: committed bytes must decrypt to
			// the committed plaintext under the committed key.
			gotPlain, err := envelope.V1().Decrypt(wantEnvelope, pbk)
			require.NoError(t, err, "decrypt committed envelope")
			require.Equal(t, plain, gotPlain)
		})
	}
}

// regenerateVectors is invoked when `-update` is passed. It writes a
// new testdata/vectors.json with deterministically-seeded random bytes
// for every nonce; the committed file is the authoritative on-disk
// shape we verify on every CI run.
//
// The seeds are themselves hex constants (see the slice literal below)
// so the regenerated file is byte-identical from a clean checkout.
func regenerateVectors(t *testing.T) {
	t.Helper()
	cases := []struct {
		name     string
		seedMK   string
		seedPBK  string
		seedWrap string
		seedEnv  string
		plainHex string
	}{
		{
			name:     "empty_plaintext",
			seedMK:   "00000000000000000000000000000000000000000000000000000000000000aa",
			seedPBK:  "00000000000000000000000000000000000000000000000000000000000000bb",
			seedWrap: "00000000000000000000000000000000000000000000000000000000000000cc",
			seedEnv:  "00000000000000000000000000000000000000000000000000000000000000dd",
			plainHex: "",
		},
		{
			name:     "short_ascii",
			seedMK:   "00000000000000000000000000000000000000000000000000000000000000a1",
			seedPBK:  "00000000000000000000000000000000000000000000000000000000000000a2",
			seedWrap: "00000000000000000000000000000000000000000000000000000000000000a3",
			seedEnv:  "00000000000000000000000000000000000000000000000000000000000000a4",
			plainHex: hex.EncodeToString([]byte("hello, nova\n")),
		},
		{
			name:     "binary_1kib",
			seedMK:   "00000000000000000000000000000000000000000000000000000000000000b1",
			seedPBK:  "00000000000000000000000000000000000000000000000000000000000000b2",
			seedWrap: "00000000000000000000000000000000000000000000000000000000000000b3",
			seedEnv:  "00000000000000000000000000000000000000000000000000000000000000b4",
			plainHex: hex.EncodeToString(deterministicBytes("plain-1kib", 1024)),
		},
		{
			name:     "binary_64kib",
			seedMK:   "00000000000000000000000000000000000000000000000000000000000000c1",
			seedPBK:  "00000000000000000000000000000000000000000000000000000000000000c2",
			seedWrap: "00000000000000000000000000000000000000000000000000000000000000c3",
			seedEnv:  "00000000000000000000000000000000000000000000000000000000000000c4",
			plainHex: hex.EncodeToString(deterministicBytes("plain-64kib", 64*1024)),
		},
	}

	out := make([]vector, 0, len(cases))
	for _, c := range cases {
		mk := drainExactly(newSeededReader(c.seedMK), envelope.KeySize)
		pbk := drainExactly(newSeededReader(c.seedPBK), envelope.KeySize)

		// Wrap with a deterministic nonce-source: build the wrapped key
		// directly to avoid plumbing a custom RNG through WrapKey.
		// We re-implement the wire layout here using exported helpers.
		wrapped, err := envelope.WrapKeyWithNonceForTest(mk, pbk, drainExactly(newSeededReader(c.seedWrap), envelope.NonceSize))
		require.NoError(t, err)

		env, err := envelope.V1EncryptWithNonceForTest(mustHex(c.plainHex), pbk, drainExactly(newSeededReader(c.seedEnv), envelope.NonceSize))
		require.NoError(t, err)

		out = append(out, vector{
			Name:         c.name,
			MasterKey:    hex.EncodeToString(mk),
			PerBlobKey:   hex.EncodeToString(pbk),
			Plaintext:    c.plainHex,
			WrapSeedHex:  c.seedWrap,
			EnvelopeSeed: c.seedEnv,
			WrappedKey:   hex.EncodeToString(wrapped),
			Envelope:     hex.EncodeToString(env),
		})
	}

	encoded, err := json.MarshalIndent(out, "", "  ")
	require.NoError(t, err)
	encoded = append(encoded, '\n')

	require.NoError(t, os.MkdirAll(filepath.Join("testdata"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join("testdata", "vectors.json"), encoded, 0o644))
	t.Logf("regenerated testdata/vectors.json with %d vectors", len(out))
}

func drainExactly(r io.Reader, n int) []byte {
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		panic(err)
	}
	return buf
}

// deterministicBytes produces n bytes seeded from name. Used for
// constructing reproducible "binary" plaintexts in vectors.
func deterministicBytes(name string, n int) []byte {
	var seed [32]byte
	copy(seed[:], name)
	r := rand.NewChaCha8(seed)
	buf := make([]byte, n)
	_, _ = r.Read(buf)
	return buf
}
```

This test references `envelope.WrapKeyWithNonceForTest` and `envelope.V1EncryptWithNonceForTest` — test-only helpers that take an explicit nonce. We add them in the next step rather than plumbing an `io.Reader` through the production API.

- [ ] **Step 5.2: Add test-only helpers**

Create `internal/envelope/test_helpers.go` (production code that is gated by build-tag-less but only invoked from tests):

```go
package envelope

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// WrapKeyWithNonceForTest performs WrapKey with a caller-supplied
// nonce. It exists to support deterministic golden-vector generation
// in vectors_test.go and is not used in production code paths.
//
// Production code MUST call WrapKey (which generates its own random
// nonce). Re-using a nonce across two distinct per-blob keys with the
// same master key is a catastrophic AEAD misuse.
func WrapKeyWithNonceForTest(masterKey, perBlobKey, wrapNonce []byte) ([]byte, error) {
	if len(masterKey) != KeySize {
		return nil, ErrKeyWrongLength
	}
	if len(perBlobKey) != KeySize {
		return nil, ErrKeyWrongLength
	}
	if len(wrapNonce) != NonceSize {
		return nil, fmt.Errorf("envelope keywrap: nonce wrong length %d", len(wrapNonce))
	}
	aead, err := chacha20poly1305.NewX(masterKey)
	if err != nil {
		return nil, fmt.Errorf("envelope keywrap: aead init: %w", err)
	}
	out := make([]byte, NonceSize, WrappedKeySize)
	copy(out, wrapNonce)
	out = aead.Seal(out, wrapNonce, perBlobKey, nil)
	if len(out) != WrappedKeySize {
		return nil, errors.New("envelope keywrap: unexpected wrapped length")
	}
	return out, nil
}

// V1EncryptWithNonceForTest performs v1 envelope encryption with a
// caller-supplied nonce. Test-only.
func V1EncryptWithNonceForTest(plaintext, perBlobKey, nonce []byte) ([]byte, error) {
	if len(perBlobKey) != KeySize {
		return nil, ErrKeyWrongLength
	}
	if len(nonce) != NonceSize {
		return nil, fmt.Errorf("envelope v1: nonce wrong length %d", len(nonce))
	}
	aead, err := chacha20poly1305.NewX(perBlobKey)
	if err != nil {
		return nil, fmt.Errorf("envelope v1: aead init: %w", err)
	}
	out := make([]byte, HeaderSize, HeaderSize+len(plaintext)+TagSize)
	copy(out[0:4], magic[:])
	out[4] = VersionV1
	out[5] = AlgorithmXChaCha20Poly1305
	copy(out[8:HeaderSize], nonce)
	out = aead.Seal(out, nonce, plaintext, nil)
	return out, nil
}
```

- [ ] **Step 5.3: Generate vectors**

```bash
go test ./internal/envelope/... -run TestVectorsRoundTrip -update -count=1
```

Expected: writes `internal/envelope/testdata/vectors.json` with 4 entries.

- [ ] **Step 5.4: Run vectors test without -update**

```bash
go test ./internal/envelope/... -count=1
```

Expected: PASS — vector loader reads the just-generated file and successfully unwraps and decrypts each entry.

- [ ] **Step 5.5: Sanity-check the file shape**

```bash
jq 'length, .[0] | keys' internal/envelope/testdata/vectors.json
```

Expected: prints `4`, then the list of fields per vector.

- [ ] **Step 5.6: Commit**

```bash
git add internal/envelope/test_helpers.go internal/envelope/vectors_test.go internal/envelope/testdata/vectors.json
git commit -s -m "test(envelope): golden vectors for v1 codec + master-key wrap

Four committed vectors (empty, ASCII, 1 KiB, 64 KiB) generated from
fixed ChaCha8 seeds. Vectors pin the on-disk byte format; regenerate
via 'go test ./internal/envelope/... -run TestVectorsRoundTrip -update'
and review the diff."
```

---

### Task 6: Envelope keystore — multi-version master keys (TDD)

**Files:**
- Create: `internal/dbtest/dbtest.go` (shared test helper)
- Create: `internal/envelope/keystore.go`
- Create: `internal/envelope/keystore_test.go`

This task introduces the only DB-aware piece of `internal/envelope`. It wires `master_key_versions` to the wrap/unwrap functions so callers (eventually the coordinator + nova-image) can ask the keystore to wrap a fresh per-blob key without knowing which master-key version is active.

- [ ] **Step 6.1: Create the shared `dbtest` helper**

This helper spins up postgres + applies the embedded migrations. It is used by every M2 integration test from this point onward.

Create `internal/dbtest/dbtest.go`:

```go
// Package dbtest provides a one-call helper to spin up Postgres in a
// testcontainer, apply Nova's embedded migrations, and return a
// pgxpool. Integration tests across internal/envelope, internal/jobs,
// and internal/integration use it.
//
// The helper is deliberately not in internal/db so the production code
// path does not import testcontainers (which transitively pulls Docker
// client libraries). Importing testcontainers from non-test packages
// would balloon every binary's link footprint.
package dbtest

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/nova-archive/nova/internal/db"
	"github.com/nova-archive/nova/internal/db/migrations"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// New returns a pgxpool against a freshly-migrated Postgres container.
// The container is terminated on t.Cleanup.
//
// Caller MUST treat the returned pool as scoped to the current test.
// Spawning multiple containers in parallel tests is supported but
// slow; prefer reusing the pool within a single test via subtests.
func New(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()

	container, err := postgres.RunContainer(ctx,
		postgres.WithDatabase("nova"),
		postgres.WithUsername("nova"),
		postgres.WithPassword("test-password"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = container.Terminate(shutdownCtx)
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	applyMigrations(t, ctx, dsn)

	pool, err := db.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func applyMigrations(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	sqlDB, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer sqlDB.Close()

	require.NoError(t, goose.SetDialect("postgres"))
	goose.SetBaseFS(migrations.Migrations)
	require.NoError(t, goose.UpContext(ctx, sqlDB, "."))
}

// suppress unused-import warning for stdlib's pgx driver in case the
// linker tries to trim it (we need its init() side effect).
var _ = stdlib.GetDefaultDriver
```

- [ ] **Step 6.2: Write the failing keystore test**

Create `internal/envelope/keystore_test.go`:

```go
package envelope_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/stretchr/testify/require"
)

func mustHexKey() string {
	b := make([]byte, envelope.KeySize)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func TestIntegrationKeystoreBootstrapInsertsRow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool := dbtest.New(t, ctx)
	t.Setenv("NOVA_MASTER_KEY_V1", mustHexKey())
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")

	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	require.Equal(t, "v1", ks.ActiveLabel())

	// First call inserts the master_key_versions row.
	versionID, err := ks.Bootstrap(ctx)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, versionID)

	// Second call is a no-op and returns the existing id.
	again, err := ks.Bootstrap(ctx)
	require.NoError(t, err)
	require.Equal(t, versionID, again, "Bootstrap is idempotent")
}

func TestIntegrationKeystoreWrapUnwrapRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool := dbtest.New(t, ctx)
	t.Setenv("NOVA_MASTER_KEY_V1", mustHexKey())
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")

	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)

	pbk := make([]byte, envelope.KeySize)
	_, _ = rand.Read(pbk)

	wrapped, versionID, err := ks.Wrap(pbk)
	require.NoError(t, err)
	require.Equal(t, envelope.WrappedKeySize, len(wrapped))

	got, err := ks.Unwrap(wrapped, versionID)
	require.NoError(t, err)
	require.Equal(t, pbk, got)
}

func TestIntegrationKeystoreMultiVersionUnwrap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool := dbtest.New(t, ctx)

	// Configure two master-key versions; v1 active.
	t.Setenv("NOVA_MASTER_KEY_V1", mustHexKey())
	t.Setenv("NOVA_MASTER_KEY_V2", mustHexKey())
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")

	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	v1id, err := ks.Bootstrap(ctx)
	require.NoError(t, err)

	pbk := make([]byte, envelope.KeySize)
	_, _ = rand.Read(pbk)
	wrappedV1, gotID, err := ks.Wrap(pbk)
	require.NoError(t, err)
	require.Equal(t, v1id, gotID, "Wrap uses the active version")

	// Now flip active to v2 and re-init the keystore (simulating restart).
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v2")
	ks2, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	v2id, err := ks2.Bootstrap(ctx)
	require.NoError(t, err)
	require.NotEqual(t, v1id, v2id)

	// We can still unwrap v1-wrapped keys via ks2 because both master
	// keys are loaded in process memory.
	got, err := ks2.Unwrap(wrappedV1, v1id)
	require.NoError(t, err)
	require.Equal(t, pbk, got)
}

func TestKeystoreRefusesShortHexFromEnv(t *testing.T) {
	t.Setenv("NOVA_MASTER_KEY_V1", "deadbeef") // 4 bytes, too short
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	_, err := envelope.NewKeystoreFromEnv(nil)
	require.Error(t, err)
}

func TestKeystoreRefusesMissingActive(t *testing.T) {
	t.Setenv("NOVA_MASTER_KEY_V1", mustHexKey())
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "")
	_, err := envelope.NewKeystoreFromEnv(nil)
	require.Error(t, err)
}

func TestKeystoreRefusesActiveWithoutLoadedKey(t *testing.T) {
	t.Setenv("NOVA_MASTER_KEY_V1", mustHexKey())
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v2") // active references a version we did not load
	_, err := envelope.NewKeystoreFromEnv(nil)
	require.Error(t, err)
}
```

- [ ] **Step 6.3: Run test, verify it fails**

```bash
go test ./internal/envelope/... -run Keystore
```

Expected: FAIL — `undefined: envelope.NewKeystoreFromEnv`.

- [ ] **Step 6.4: Add the google/uuid dep**

```bash
go get github.com/google/uuid@latest
go mod tidy
```

Expected: pulls `github.com/google/uuid` (already indirectly required by pgx).

- [ ] **Step 6.5: Implement keystore.go**

```go
package envelope

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Keystore holds the operator's master keys (one per version label)
// in process memory and exposes Wrap/Unwrap that record and resolve
// the master_key_versions.id for each wrapped per-blob key.
//
// Lifecycle:
//   1. NewKeystoreFromEnv reads NOVA_MASTER_KEY_<LABEL> + NOVA_MASTER_KEY_ACTIVE.
//   2. Bootstrap(ctx) inserts the active version's master_key_versions
//      row if it does not already exist (idempotent), and caches the
//      label → id mapping for every version it finds in the DB.
//   3. Wrap(perBlobKey) wraps under the active version and returns the
//      wrapped bytes + the active version's id.
//   4. Unwrap(wrapped, id) looks up the version by id and unwraps with
//      the matching in-memory master key.
type Keystore struct {
	pool         *pgxpool.Pool
	masters      map[string][]byte    // label → 32-byte master key
	versionByID  map[uuid.UUID]string // master_key_versions.id → label
	idByLabel    map[string]uuid.UUID // label → master_key_versions.id
	activeLabel  string
}

// NewKeystoreFromEnv parses NOVA_MASTER_KEY_<LABEL> entries from the
// process environment (where <LABEL> is uppercased — V1, V2, V2026Q2)
// and NOVA_MASTER_KEY_ACTIVE selects the default. Labels are stored
// lowercase in the keystore and in master_key_versions.version_label.
//
// At least one NOVA_MASTER_KEY_<LABEL> matching ACTIVE must be set, or
// the constructor returns an error.
func NewKeystoreFromEnv(pool *pgxpool.Pool) (*Keystore, error) {
	active := strings.TrimSpace(os.Getenv("NOVA_MASTER_KEY_ACTIVE"))
	if active == "" {
		return nil, errors.New("keystore: NOVA_MASTER_KEY_ACTIVE is required")
	}
	active = strings.ToLower(active)

	masters := make(map[string][]byte)
	for _, e := range os.Environ() {
		const prefix = "NOVA_MASTER_KEY_"
		if !strings.HasPrefix(e, prefix) {
			continue
		}
		eq := strings.IndexByte(e, '=')
		if eq < 0 {
			continue
		}
		key := e[:eq]
		val := strings.TrimSpace(e[eq+1:])
		label := strings.ToLower(strings.TrimPrefix(key, prefix))
		if label == "active" || label == "file" || strings.HasSuffix(label, "_file") {
			continue
		}
		if val == "" {
			continue
		}
		raw, err := hex.DecodeString(val)
		if err != nil {
			return nil, fmt.Errorf("keystore: NOVA_MASTER_KEY_%s is not valid hex: %w", strings.ToUpper(label), err)
		}
		if len(raw) != KeySize {
			return nil, fmt.Errorf("keystore: NOVA_MASTER_KEY_%s must be %d bytes (got %d)", strings.ToUpper(label), KeySize, len(raw))
		}
		masters[label] = raw
	}

	if _, ok := masters[active]; !ok {
		return nil, fmt.Errorf("keystore: NOVA_MASTER_KEY_ACTIVE=%s but NOVA_MASTER_KEY_%s is not set", active, strings.ToUpper(active))
	}

	return &Keystore{
		pool:        pool,
		masters:     masters,
		versionByID: make(map[uuid.UUID]string),
		idByLabel:   make(map[string]uuid.UUID),
		activeLabel: active,
	}, nil
}

// ActiveLabel returns the active version label (lowercase).
func (k *Keystore) ActiveLabel() string { return k.activeLabel }

// Bootstrap ensures master_key_versions has a row for every label in
// k.masters that is not yet recorded. The active row is created with
// state='active'; non-active labels are not auto-inserted (operators
// rotate via novactl in M10 which sets state explicitly). Returns the
// active version's id.
//
// Idempotent. Safe to call from multiple processes concurrently.
func (k *Keystore) Bootstrap(ctx context.Context) (uuid.UUID, error) {
	// 1. Load every existing row.
	if err := k.loadVersions(ctx); err != nil {
		return uuid.Nil, err
	}
	// 2. Insert the active label if absent.
	if id, ok := k.idByLabel[k.activeLabel]; ok {
		return id, nil
	}
	var id uuid.UUID
	err := k.pool.QueryRow(ctx, `
		INSERT INTO master_key_versions (version_label, state)
		VALUES ($1, 'active')
		ON CONFLICT (version_label) DO UPDATE SET version_label = EXCLUDED.version_label
		RETURNING id
	`, k.activeLabel).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("keystore: insert master_key_versions: %w", err)
	}
	k.idByLabel[k.activeLabel] = id
	k.versionByID[id] = k.activeLabel
	return id, nil
}

func (k *Keystore) loadVersions(ctx context.Context) error {
	rows, err := k.pool.Query(ctx, `SELECT id, version_label FROM master_key_versions`)
	if err != nil {
		return fmt.Errorf("keystore: load versions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var label string
		if err := rows.Scan(&id, &label); err != nil {
			return fmt.Errorf("keystore: scan version: %w", err)
		}
		label = strings.ToLower(label)
		k.idByLabel[label] = id
		k.versionByID[id] = label
	}
	return rows.Err()
}

// Wrap encrypts perBlobKey under the active master key and returns the
// 72-byte wrapped payload plus the active master_key_versions.id.
// Bootstrap must have been called at least once.
func (k *Keystore) Wrap(perBlobKey []byte) ([]byte, uuid.UUID, error) {
	mk, ok := k.masters[k.activeLabel]
	if !ok {
		return nil, uuid.Nil, fmt.Errorf("keystore: active master key %q not loaded", k.activeLabel)
	}
	id, ok := k.idByLabel[k.activeLabel]
	if !ok {
		return nil, uuid.Nil, errors.New("keystore: Bootstrap must be called before Wrap")
	}
	wrapped, err := WrapKey(mk, perBlobKey)
	if err != nil {
		return nil, uuid.Nil, err
	}
	return wrapped, id, nil
}

// Unwrap decrypts wrapped under the master key recorded by versionID.
// Returns ErrEnvelopeAuthFailed if the master key has been rotated
// away and is no longer loaded (operators must keep prior versions in
// env until rotation completes for every wrapped key).
func (k *Keystore) Unwrap(wrapped []byte, versionID uuid.UUID) ([]byte, error) {
	label, ok := k.versionByID[versionID]
	if !ok {
		// Not in cache; re-load and try again.
		if err := k.loadVersions(context.Background()); err != nil {
			return nil, err
		}
		label, ok = k.versionByID[versionID]
		if !ok {
			return nil, fmt.Errorf("keystore: master_key_versions.id %s not found", versionID)
		}
	}
	mk, ok := k.masters[label]
	if !ok {
		return nil, fmt.Errorf("keystore: master key for version %q is not loaded (env missing?)", label)
	}
	return UnwrapKey(mk, wrapped)
}

// Force the import to be referenced even if we move queries to a sqlc
// build later. Keep the import quiet by referencing pgx through the
// pgxpool, but expose a no-op alias for static analysers.
var _ = pgx.ErrNoRows
```

- [ ] **Step 6.6: Run tests, verify they pass**

```bash
go test ./internal/envelope/... -count=1
```

Expected: PASS (integration tests run; takes ~60-90s on first container pull, subsequent runs cached).

- [ ] **Step 6.7: Commit**

```bash
git add internal/dbtest/dbtest.go internal/envelope/keystore.go internal/envelope/keystore_test.go go.mod go.sum
git commit -s -m "feat(envelope): keystore with multi-version master keys + DB bootstrap

Reads NOVA_MASTER_KEY_V1/V2/.../ACTIVE from the environment, ensures
master_key_versions row for the active label, and exposes Wrap/Unwrap
keyed by the master_key_versions.id. Integration-tested against postgres
via testcontainers + internal/dbtest helper."
```

---

### Task 7: IPFS Backend interface + errors + Mode enum

**Files:**
- Create: `internal/ipfs/errors.go`
- Create: `internal/ipfs/backend.go`

This task lays the interface that M3+ consumers (`pkg/coordinator/storage`, nova-image) program against. No implementation yet — that arrives in Task 10 once the validator (Task 8) and import rules (Task 9) are in place.

- [ ] **Step 7.1: Create errors.go**

```go
// Package ipfs is Nova's IPFS abstraction layer. The Backend interface
// defines the operations the coordinator and product layers perform
// against an IPFS daemon; the EmbeddedBackend implementation runs an
// in-process Kubo node configured per docs/specs/KUBO_HARDENING.md and
// docs/specs/IPFS_IMPORT_RULES.md.
//
// The interface is small on purpose: most call sites only need
// Add/Get/Has, and the audit subsystem additionally needs
// BlockstoreHas/BlockGet. Splitting these into multiple interfaces was
// considered and rejected — there is exactly one implementation in
// Phase 1, and the surface is stable enough that future product layers
// can mock it via testify/mock.
package ipfs

import "errors"

var (
	// ErrCIDNotPinned: blob is not present in the local Kubo blockstore
	// (Phase 1 single-node) or has been unpinned. Surfaces at the
	// integrity audit kubo_pin_present check.
	ErrCIDNotPinned = errors.New("ipfs: CID not pinned locally")

	// ErrBlockNotFound: a specific block CID referenced by blob_blocks
	// is missing. Surfaces at the integrity audit block_hash_valid
	// check.
	ErrBlockNotFound = errors.New("ipfs: block not found")

	// ErrConfigViolation: a KUBO_HARDENING.md rule was violated during
	// ValidateConfig. The wrapped error names the specific key.
	ErrConfigViolation = errors.New("ipfs: kubo config violates hardening rule")

	// ErrSwarmKeyMissing: private-mode requires IPFS_SWARM_KEY at the
	// repo path; Backend.Run refuses to start without it.
	ErrSwarmKeyMissing = errors.New("ipfs: swarm key missing (required in private mode)")
)
```

- [ ] **Step 7.2: Create backend.go**

```go
package ipfs

import (
	"context"
	"io"

	"github.com/ipfs/go-cid"
)

// Mode determines which set of hardening rules ValidateConfig enforces.
// Private (default) refuses public-DHT-shaped configs entirely;
// PublicArchivalDHT relaxes Routing/Provider/Bootstrap rules per the
// opt-in mode in docs/specs/KUBO_HARDENING.md § "Public IPFS DHT mode".
type Mode int

const (
	// ModePrivate is the default — every Phase 1 deployment that holds
	// personal or potentially-infringing content uses this.
	ModePrivate Mode = iota

	// ModePublicArchivalDHT is the opt-in for `nova-archive`-style
	// deployments hosting open data. Operator must explicitly set
	// `coordinator.public_ipfs_dht: true` in operator.yaml.
	ModePublicArchivalDHT
)

// String returns the mode name for log messages.
func (m Mode) String() string {
	switch m {
	case ModePrivate:
		return "private"
	case ModePublicArchivalDHT:
		return "public_archival_dht"
	default:
		return "unknown"
	}
}

// AddResult is what AddDeterministic returns. The Blocks slice is in
// DAG-traversal order matching the blob_blocks.block_index sequence in
// the database.
type AddResult struct {
	CID         cid.Cid
	EnvelopeSize int64
	Codec       string  // "raw" for single-block, "dag-pb" for multi-block
	Blocks      []Block
	MerkleRoot  cid.Cid // root CID; for single-block this equals CID
}

// Block is one row's worth of blob_blocks information.
type Block struct {
	CID   cid.Cid
	Index int
	Size  int
}

// Backend is Nova's IPFS abstraction. EmbeddedBackend implements it via
// in-process Kubo; future Phase 2 work may add a remote backend that
// talks to an external Kubo daemon over the loopback HTTP API.
type Backend interface {
	// AddDeterministic imports the bytes per IPFS_IMPORT_RULES.md. The
	// returned CID is bit-identical across implementations conforming
	// to the spec. The bytes are pinned (the implementation MAY use the
	// raw-codec shortcut path for bytes ≤ RawCodecThresholdBytes).
	AddDeterministic(ctx context.Context, envelope []byte) (AddResult, error)

	// Get retrieves the previously-Add'd bytes for a CID. The returned
	// ReadCloser MUST be closed by the caller.
	Get(ctx context.Context, c cid.Cid) (io.ReadCloser, error)

	// Has reports whether the local blockstore has the CID pinned.
	Has(ctx context.Context, c cid.Cid) (bool, error)

	// Pin pins an already-stored CID (useful when re-pinning after an
	// audit detects a missing pin).
	Pin(ctx context.Context, c cid.Cid) error

	// Unpin removes the local pin so Kubo's GC can reclaim. Does not
	// remove the blocks immediately.
	Unpin(ctx context.Context, c cid.Cid) error

	// BlockstoreHas reports whether a specific block CID exists in the
	// blockstore. Used by the block_hash_valid integrity audit.
	BlockstoreHas(ctx context.Context, c cid.Cid) (bool, error)

	// BlockGet returns the raw block bytes for a CID, bypassing UnixFS
	// reassembly. Used by the block_hash_valid integrity audit.
	BlockGet(ctx context.Context, c cid.Cid) ([]byte, error)

	// Close releases the backend's resources. After Close, all methods
	// return errors.
	Close(ctx context.Context) error
}
```

- [ ] **Step 7.3: Verify build**

```bash
go build ./internal/ipfs/...
```

Expected: builds. (No tests for the interface alone; tests land with the implementation in Task 10.)

- [ ] **Step 7.4: Commit**

```bash
git add internal/ipfs/errors.go internal/ipfs/backend.go
git commit -s -m "feat(ipfs): Backend interface + Mode enum + sentinel errors

Defines the surface the coordinator and product layers consume. M2
ships the EmbeddedBackend implementation; Phase 2 may add a remote
backend that talks to an external Kubo daemon over loopback HTTP."
```

---

### Task 8: KuboConfig + ValidateConfig hardening rules (TDD)

**Files:**
- Create: `internal/ipfs/validate.go`
- Create: `internal/ipfs/validate_test.go`

This is the refuse-to-start validator per `KUBO_HARDENING.md`. Pure logic — no Kubo node needed.

- [ ] **Step 8.1: Write the failing tests**

Create `internal/ipfs/validate_test.go`:

```go
package ipfs_test

import (
	"path/filepath"
	"testing"

	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/stretchr/testify/require"
)

// goodPrivate returns a KuboConfig that passes Private-mode validation.
// Each violation test starts from this and mutates one field.
func goodPrivate() ipfs.KuboConfig {
	return ipfs.KuboConfig{
		Bootstrap: []string{
			"/ip4/127.0.0.1/tcp/4001/p2p/12D3KooWExampleA",
			"/ip4/10.0.0.1/tcp/4001/p2p/12D3KooWExampleB",
		},
		Routing: ipfs.RoutingConfig{Type: "none"},
		Discovery: ipfs.DiscoveryConfig{
			MDNS: ipfs.MDNSConfig{Enabled: false},
		},
		Provider:   ipfs.ProviderConfig{Strategy: ""},
		Reprovider: ipfs.ReproviderConfig{Strategy: ""},
		Addresses: ipfs.AddressesConfig{
			API:     "/ip4/127.0.0.1/tcp/5001",
			Gateway: "/ip4/127.0.0.1/tcp/8080",
		},
		Swarm: ipfs.SwarmConfig{DisableNatPortMap: true},
	}
}

func writeSwarmKey(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "swarm.key")
	const content = "/key/swarm/psk/1.0.0/\n/base16/\n" +
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"
	require.NoError(t, writeFile(t, path, content))
	return path
}

func writeFile(t *testing.T, path, content string) error {
	t.Helper()
	return ipfs.WriteFileForTest(path, []byte(content))
}

func TestValidatePrivateAcceptsGoodConfig(t *testing.T) {
	t.Parallel()
	swarm := writeSwarmKey(t)
	require.NoError(t, ipfs.ValidateConfig(goodPrivate(), ipfs.ModePrivate, swarm))
}

func TestValidatePrivateRejectsMDNSEnabled(t *testing.T) {
	t.Parallel()
	swarm := writeSwarmKey(t)
	cfg := goodPrivate()
	cfg.Discovery.MDNS.Enabled = true
	err := ipfs.ValidateConfig(cfg, ipfs.ModePrivate, swarm)
	require.ErrorIs(t, err, ipfs.ErrConfigViolation)
	require.Contains(t, err.Error(), "Discovery.MDNS.Enabled")
}

func TestValidatePrivateRejectsProviderStrategy(t *testing.T) {
	t.Parallel()
	swarm := writeSwarmKey(t)
	cfg := goodPrivate()
	cfg.Provider.Strategy = "all"
	err := ipfs.ValidateConfig(cfg, ipfs.ModePrivate, swarm)
	require.ErrorIs(t, err, ipfs.ErrConfigViolation)
	require.Contains(t, err.Error(), "Provider.Strategy")
}

func TestValidatePrivateRejectsReproviderStrategy(t *testing.T) {
	t.Parallel()
	swarm := writeSwarmKey(t)
	cfg := goodPrivate()
	cfg.Reprovider.Strategy = "all"
	err := ipfs.ValidateConfig(cfg, ipfs.ModePrivate, swarm)
	require.ErrorIs(t, err, ipfs.ErrConfigViolation)
	require.Contains(t, err.Error(), "Reprovider.Strategy")
}

func TestValidatePrivateRejectsRoutingType(t *testing.T) {
	t.Parallel()
	swarm := writeSwarmKey(t)
	for _, bad := range []string{"dht", "dhtserver", "auto", "autoclient"} {
		bad := bad
		t.Run(bad, func(t *testing.T) {
			cfg := goodPrivate()
			cfg.Routing.Type = bad
			err := ipfs.ValidateConfig(cfg, ipfs.ModePrivate, writeSwarmKey(t))
			require.ErrorIs(t, err, ipfs.ErrConfigViolation)
			require.Contains(t, err.Error(), "Routing.Type")
		})
	}
}

func TestValidatePrivateAcceptsRoutingNoneOrDhtClient(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"none", "dhtclient"} {
		ok := ok
		t.Run(ok, func(t *testing.T) {
			cfg := goodPrivate()
			cfg.Routing.Type = ok
			require.NoError(t, ipfs.ValidateConfig(cfg, ipfs.ModePrivate, writeSwarmKey(t)))
		})
	}
}

func TestValidatePrivateRejectsAddressesAPINonLoopback(t *testing.T) {
	t.Parallel()
	cfg := goodPrivate()
	cfg.Addresses.API = "/ip4/0.0.0.0/tcp/5001"
	err := ipfs.ValidateConfig(cfg, ipfs.ModePrivate, writeSwarmKey(t))
	require.ErrorIs(t, err, ipfs.ErrConfigViolation)
	require.Contains(t, err.Error(), "Addresses.API")
}

func TestValidatePrivateRejectsAddressesGatewayNonLoopback(t *testing.T) {
	t.Parallel()
	cfg := goodPrivate()
	cfg.Addresses.Gateway = "/ip4/0.0.0.0/tcp/8080"
	err := ipfs.ValidateConfig(cfg, ipfs.ModePrivate, writeSwarmKey(t))
	require.ErrorIs(t, err, ipfs.ErrConfigViolation)
	require.Contains(t, err.Error(), "Addresses.Gateway")
}

func TestValidatePrivateRejectsNatPortMapEnabled(t *testing.T) {
	t.Parallel()
	cfg := goodPrivate()
	cfg.Swarm.DisableNatPortMap = false
	err := ipfs.ValidateConfig(cfg, ipfs.ModePrivate, writeSwarmKey(t))
	require.ErrorIs(t, err, ipfs.ErrConfigViolation)
	require.Contains(t, err.Error(), "Swarm.DisableNatPortMap")
}

func TestValidatePrivateRejectsPublicBootstrap(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{
		"/dnsaddr/bootstrap.libp2p.io/p2p/QmNnoo",
		"/ip4/8.8.8.8/tcp/4001/p2p/12D3Koo",
		"/ip6/2001:db8::1/tcp/4001/p2p/12D3Koo",
		"/dns4/node.ipfs.io/tcp/4001/p2p/12D3Koo",
	} {
		addr := addr
		t.Run(addr, func(t *testing.T) {
			cfg := goodPrivate()
			cfg.Bootstrap = append(cfg.Bootstrap, addr)
			err := ipfs.ValidateConfig(cfg, ipfs.ModePrivate, writeSwarmKey(t))
			require.ErrorIs(t, err, ipfs.ErrConfigViolation)
			require.Contains(t, err.Error(), "Bootstrap")
		})
	}
}

func TestValidatePrivateAcceptsRFC1918Bootstrap(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{
		"/ip4/10.0.0.5/tcp/4001/p2p/12D3Koo",
		"/ip4/172.16.0.1/tcp/4001/p2p/12D3Koo",
		"/ip4/192.168.1.1/tcp/4001/p2p/12D3Koo",
		"/ip4/127.0.0.1/tcp/4001/p2p/12D3Koo",
		"/ip6/::1/tcp/4001/p2p/12D3Koo",
	} {
		addr := addr
		t.Run(addr, func(t *testing.T) {
			cfg := goodPrivate()
			cfg.Bootstrap = []string{addr}
			require.NoError(t, ipfs.ValidateConfig(cfg, ipfs.ModePrivate, writeSwarmKey(t)))
		})
	}
}

func TestValidatePrivateRefusesMissingSwarmKey(t *testing.T) {
	t.Parallel()
	cfg := goodPrivate()
	err := ipfs.ValidateConfig(cfg, ipfs.ModePrivate, filepath.Join(t.TempDir(), "no-such-file"))
	require.ErrorIs(t, err, ipfs.ErrSwarmKeyMissing)
}

func TestValidatePrivateRefusesEmptySwarmKeyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	empty := filepath.Join(dir, "swarm.key")
	require.NoError(t, ipfs.WriteFileForTest(empty, []byte{}))
	cfg := goodPrivate()
	err := ipfs.ValidateConfig(cfg, ipfs.ModePrivate, empty)
	require.ErrorIs(t, err, ipfs.ErrSwarmKeyMissing)
}

func TestValidatePublicArchivalDHTRelaxesMost(t *testing.T) {
	t.Parallel()
	cfg := goodPrivate()
	cfg.Routing.Type = "dht"
	cfg.Provider.Strategy = "all"
	cfg.Reprovider.Strategy = "all"
	cfg.Bootstrap = append(cfg.Bootstrap, "/dnsaddr/bootstrap.libp2p.io/p2p/QmAny")
	cfg.Swarm.DisableNatPortMap = false
	// Loopback API and Gateway remain mandatory even in public mode.
	require.NoError(t, ipfs.ValidateConfig(cfg, ipfs.ModePublicArchivalDHT, ""))
}

func TestValidatePublicArchivalStillRequiresLoopbackAPI(t *testing.T) {
	t.Parallel()
	cfg := goodPrivate()
	cfg.Addresses.API = "/ip4/0.0.0.0/tcp/5001"
	err := ipfs.ValidateConfig(cfg, ipfs.ModePublicArchivalDHT, "")
	require.ErrorIs(t, err, ipfs.ErrConfigViolation)
}
```

- [ ] **Step 8.2: Run tests, verify they fail**

```bash
go test ./internal/ipfs/...
```

Expected: FAIL — `undefined: ipfs.KuboConfig`.

- [ ] **Step 8.3: Implement validate.go**

```go
package ipfs

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
)

// KuboConfig mirrors the fields of Kubo's on-disk config.json that the
// hardening validator inspects. We intentionally do not import Kubo's
// config struct here — the validator is a pure-Go check we can run
// before any Kubo code loads, and decoupling from Kubo's internals
// limits the blast radius of Kubo version upgrades.
type KuboConfig struct {
	Bootstrap  []string         `json:"Bootstrap"`
	Routing    RoutingConfig    `json:"Routing"`
	Discovery  DiscoveryConfig  `json:"Discovery"`
	Provider   ProviderConfig   `json:"Provider"`
	Reprovider ReproviderConfig `json:"Reprovider"`
	Addresses  AddressesConfig  `json:"Addresses"`
	Swarm      SwarmConfig      `json:"Swarm"`
}

type RoutingConfig struct {
	Type string `json:"Type"`
}

type DiscoveryConfig struct {
	MDNS MDNSConfig `json:"MDNS"`
}

type MDNSConfig struct {
	Enabled bool `json:"Enabled"`
}

type ProviderConfig struct {
	Strategy string `json:"Strategy"`
}

type ReproviderConfig struct {
	Strategy string `json:"Strategy"`
}

type AddressesConfig struct {
	API     string `json:"API"`
	Gateway string `json:"Gateway"`
}

type SwarmConfig struct {
	DisableNatPortMap bool `json:"DisableNatPortMap"`
}

// ValidateConfig walks the KUBO_HARDENING.md validator table against
// the given Kubo config + mode. Returns the first violation as a
// wrapped ErrConfigViolation, naming the offending key. The validator
// stops at the first violation rather than collecting all of them so
// the operator's first restart sees the most upstream root cause.
//
// In ModePrivate, swarmKeyPath must point to a non-empty file
// containing the IPFS_SWARM_KEY format (per KUBO_HARDENING.md
// § "Private swarm key"). In ModePublicArchivalDHT, swarmKeyPath is
// ignored (pass "").
func ValidateConfig(cfg KuboConfig, mode Mode, swarmKeyPath string) error {
	// Rules that apply in BOTH modes.
	if err := requireLoopback("Addresses.API", cfg.Addresses.API); err != nil {
		return err
	}
	if err := requireLoopback("Addresses.Gateway", cfg.Addresses.Gateway); err != nil {
		return err
	}

	if mode == ModePublicArchivalDHT {
		// Public archival mode: loopback API/Gateway are all that's required.
		return nil
	}

	// Private mode rules.
	if cfg.Discovery.MDNS.Enabled {
		return wrapViolation("Discovery.MDNS.Enabled must be false in private mode")
	}
	if cfg.Provider.Strategy != "" {
		return wrapViolation("Provider.Strategy must be empty in private mode (got %q)", cfg.Provider.Strategy)
	}
	if cfg.Reprovider.Strategy != "" {
		return wrapViolation("Reprovider.Strategy must be empty in private mode (got %q)", cfg.Reprovider.Strategy)
	}
	switch cfg.Routing.Type {
	case "none", "dhtclient":
		// ok
	default:
		return wrapViolation("Routing.Type must be 'none' or 'dhtclient' in private mode (got %q)", cfg.Routing.Type)
	}
	if !cfg.Swarm.DisableNatPortMap {
		return wrapViolation("Swarm.DisableNatPortMap must be true in private mode")
	}
	for _, addr := range cfg.Bootstrap {
		if err := requirePrivateBootstrap(addr); err != nil {
			return err
		}
	}

	// Swarm key must exist and be non-empty.
	if swarmKeyPath == "" {
		return fmt.Errorf("validate: swarm key path is empty: %w", ErrSwarmKeyMissing)
	}
	info, err := os.Stat(swarmKeyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("validate: %s: %w", swarmKeyPath, ErrSwarmKeyMissing)
		}
		return fmt.Errorf("validate: stat %s: %w", swarmKeyPath, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("validate: %s is empty: %w", swarmKeyPath, ErrSwarmKeyMissing)
	}

	return nil
}

func requireLoopback(field, ma string) error {
	// Multiaddr strings start with /ip4/<ip>/tcp/... or /ip6/<ip>/tcp/...
	parts := strings.Split(ma, "/")
	if len(parts) < 3 {
		return wrapViolation("%s is not a valid multiaddr: %q", field, ma)
	}
	switch parts[1] {
	case "ip4":
		if parts[2] != "127.0.0.1" {
			return wrapViolation("%s must bind to 127.0.0.1 (got %q)", field, ma)
		}
	case "ip6":
		if parts[2] != "::1" {
			return wrapViolation("%s must bind to ::1 (got %q)", field, ma)
		}
	default:
		return wrapViolation("%s must be /ip4/ or /ip6/ (got %q)", field, ma)
	}
	return nil
}

// requirePrivateBootstrap accepts loopback, RFC 1918, or Nova-overlay
// addresses. (Nebula overlay support is recognised as RFC 1918 by
// default; operators who use non-RFC-1918 overlay subnets are out of
// scope for the table-driven test but the rule still applies — they
// pass an additional allow-list via a future config option, not in M2.)
func requirePrivateBootstrap(ma string) error {
	parts := strings.Split(ma, "/")
	if len(parts) < 3 {
		return wrapViolation("Bootstrap entry not a valid multiaddr: %q", ma)
	}
	switch parts[1] {
	case "ip4":
		ip := net.ParseIP(parts[2])
		if ip == nil {
			return wrapViolation("Bootstrap entry has bad IPv4 %q", parts[2])
		}
		if isLoopback4(ip) || isRFC1918(ip) {
			return nil
		}
		return wrapViolation("Bootstrap entry %q is not loopback/RFC1918 (private mode requires private addresses)", ma)
	case "ip6":
		ip := net.ParseIP(parts[2])
		if ip == nil {
			return wrapViolation("Bootstrap entry has bad IPv6 %q", parts[2])
		}
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return nil
		}
		return wrapViolation("Bootstrap entry %q is not loopback IPv6", ma)
	case "dnsaddr", "dns4", "dns6", "dns":
		// DNS-bootstrap addresses cannot be resolved at config-validate
		// time without leaking the federation to the resolver. We refuse
		// them in private mode; operators who genuinely need DNS-based
		// private bootstraps run an internal resolver and use the ip4/ip6
		// form against its result.
		return wrapViolation("Bootstrap entry uses DNS resolution (%q); private mode requires literal IPs", ma)
	default:
		return wrapViolation("Bootstrap entry has unknown protocol %q", ma)
	}
}

func isLoopback4(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	return v4[0] == 127
}

func isRFC1918(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	switch {
	case v4[0] == 10:
		return true
	case v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31:
		return true
	case v4[0] == 192 && v4[1] == 168:
		return true
	default:
		return false
	}
}

func wrapViolation(format string, args ...any) error {
	return errors.Join(ErrConfigViolation, fmt.Errorf(format, args...))
}

// WriteFileForTest is a small re-export of os.WriteFile so the
// validate_test.go file can sit in the _test package. (Tests live in
// internal/ipfs_test to enforce the public surface; we expose a helper
// to keep the test code free of build-tag tricks.)
func WriteFileForTest(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
```

- [ ] **Step 8.4: Run tests, verify they pass**

```bash
go test ./internal/ipfs/...
```

Expected: PASS — all validation cases (~15 subtests).

- [ ] **Step 8.5: Commit**

```bash
git add internal/ipfs/validate.go internal/ipfs/validate_test.go
git commit -s -m "feat(ipfs): KuboConfig + ValidateConfig refuse-to-start hardening

Implements every row of the KUBO_HARDENING.md validator table for
Private and PublicArchivalDHT modes. Returns errors.Join(ErrConfigViolation, ...)
naming the offending key so the coordinator startup logs identify the
violation precisely."
```

---

### Task 9: IPFS import rules constants + threshold helper

**Files:**
- Create: `internal/ipfs/importrules.go`
- Create: `internal/ipfs/importrules_test.go`

This task captures the IPFS_IMPORT_RULES.md numeric constants in one named place so they cannot drift away from the spec. The threshold helper picks the import path (raw codec vs UnixFS dag-pb) based on envelope size.

- [ ] **Step 9.1: Write the failing test**

Create `internal/ipfs/importrules_test.go`:

```go
package ipfs_test

import (
	"testing"

	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/stretchr/testify/require"
)

func TestRawCodecThresholdIsOneMiB(t *testing.T) {
	t.Parallel()
	require.Equal(t, int64(1<<20), ipfs.RawCodecThresholdBytes,
		"IPFS_IMPORT_RULES.md fixes the threshold at 1 MiB; do not change without spec amendment")
}

func TestChunkerSizeIsTwoFiftySixKiB(t *testing.T) {
	t.Parallel()
	require.Equal(t, int64(262144), ipfs.ChunkerSizeBytes,
		"IPFS_IMPORT_RULES.md fixes the chunker at 256 KiB; do not change without spec amendment")
	require.Equal(t, "size-262144", ipfs.ChunkerSpec,
		"ChunkerSpec must match the Kubo chunker name exactly")
}

func TestShouldUseRawCodecAtAndBelowThreshold(t *testing.T) {
	t.Parallel()
	require.True(t, ipfs.ShouldUseRawCodec(0))
	require.True(t, ipfs.ShouldUseRawCodec(1))
	require.True(t, ipfs.ShouldUseRawCodec(262144))   // exactly one chunk
	require.True(t, ipfs.ShouldUseRawCodec(1<<20))    // at threshold
	require.False(t, ipfs.ShouldUseRawCodec(1<<20+1)) // one byte over
	require.False(t, ipfs.ShouldUseRawCodec(5*1024*1024))
}
```

- [ ] **Step 9.2: Run test, verify it fails**

```bash
go test ./internal/ipfs/... -run "Threshold|Chunker|Raw"
```

Expected: FAIL — undefined symbols.

- [ ] **Step 9.3: Implement importrules.go**

```go
package ipfs

// IPFS import rules per docs/specs/IPFS_IMPORT_RULES.md. Every value
// here is Tier 1 — protocol-enforced, refused-at-startup if mutated
// across a federation. Operators cannot tune any of these.
const (
	// RawCodecThresholdBytes is the upper bound (inclusive) for the
	// single-block raw-codec import shortcut. Envelopes at or below
	// this size are stored as a single raw block; above this they are
	// chunked under a dag-pb UnixFS file node.
	RawCodecThresholdBytes int64 = 1 << 20 // 1 MiB

	// ChunkerSizeBytes is the fixed chunk size for multi-block imports.
	// Numerically equal to ChunkerSpec's suffix.
	ChunkerSizeBytes int64 = 262144 // 256 KiB

	// ChunkerSpec is the Kubo chunker-name string that produces the
	// ChunkerSizeBytes chunks. Used verbatim as the
	// options.Unixfs.Chunker(...) argument.
	ChunkerSpec = "size-262144"

	// HashAlg is the multihash algorithm name used for every block in
	// every Nova-imported DAG. Matches IPFS_IMPORT_RULES.md.
	HashAlg = "sha2-256"

	// CodecRaw is the multicodec name for the single-block path.
	CodecRaw = "raw"

	// CodecDagPB is the multicodec name for the multi-block path.
	CodecDagPB = "dag-pb"

	// MaxLinkCount is Kubo's UnixFS-1 default. Recorded here to make
	// drift obvious if a future Kubo upgrade changes the default.
	MaxLinkCount = 174
)

// ShouldUseRawCodec returns true when an envelope of envelopeSize bytes
// should be imported via the raw-codec shortcut (single block, no
// UnixFS wrapping). Returns false for sizes above RawCodecThresholdBytes,
// indicating the dag-pb chunked path is required for spec-correct CIDs.
func ShouldUseRawCodec(envelopeSize int64) bool {
	return envelopeSize <= RawCodecThresholdBytes
}
```

- [ ] **Step 9.4: Run tests, verify they pass**

```bash
go test ./internal/ipfs/...
```

Expected: PASS — all validator and importrules tests.

- [ ] **Step 9.5: Commit**

```bash
git add internal/ipfs/importrules.go internal/ipfs/importrules_test.go
git commit -s -m "feat(ipfs): import-rule constants and threshold helper

Numeric constants from IPFS_IMPORT_RULES.md (1 MiB raw threshold, 256
KiB chunker, sha2-256, raw/dag-pb codecs) live in one named place so
drift is impossible without a deliberate code change."
```

---

### Task 10: Embedded Kubo backend (TDD with offline node)

**Files:**
- Create: `internal/ipfs/embedded.go`
- Create: `internal/ipfs/embedded_test.go`

This task wires the embedded Kubo node to the `Backend` interface. Tests run an offline (no swarm) Kubo node per test in `t.TempDir()`. The implementation has two import paths: raw-codec for envelopes ≤ 1 MiB, dag-pb UnixFS for larger.

> **Implementer note.** Kubo's `core/coreapi` surface changes between major versions. The code below targets the API shape as of Kubo v0.30+. If the resolved version from Task 1 has a divergent type for `api.Unixfs().Add(...)` return values or `path.New(...)` (vs `path.NewPath`), adjust call sites — the structure, options, and ordering are what matter. Surface the version-specific tweak in the commit message rather than making it silent.

- [ ] **Step 10.1: Write the failing integration test**

Create `internal/ipfs/embedded_test.go`:

```go
package ipfs_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/stretchr/testify/require"
)

func newOfflineBackend(t *testing.T) (*ipfs.EmbeddedBackend, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	repo := t.TempDir()
	swarmKey := filepath.Join(t.TempDir(), "swarm.key")
	require.NoError(t, ipfs.WriteFileForTest(swarmKey,
		[]byte("/key/swarm/psk/1.0.0/\n/base16/\n"+
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n")))

	be, err := ipfs.NewEmbedded(ctx, ipfs.EmbeddedOptions{
		RepoPath:     repo,
		Mode:         ipfs.ModePrivate,
		SwarmKeyPath: swarmKey,
		Online:       false, // offline = no libp2p swarm in tests
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = be.Close(shutdownCtx)
	})
	return be, ctx
}

func TestIntegrationEmbeddedRoundTripSmall(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	be, ctx := newOfflineBackend(t)

	envelope := bytes.Repeat([]byte{0xAB}, 1024) // 1 KiB, raw-codec path
	res, err := be.AddDeterministic(ctx, envelope)
	require.NoError(t, err)
	require.Equal(t, ipfs.CodecRaw, res.Codec, "≤1MiB envelope must use raw codec")
	require.Equal(t, int64(len(envelope)), res.EnvelopeSize)
	require.Equal(t, 1, len(res.Blocks), "single-block raw must yield exactly one block row")
	require.Equal(t, res.CID, res.Blocks[0].CID, "raw block CID equals envelope CID")
	require.Equal(t, res.CID, res.MerkleRoot)

	rc, err := be.Get(ctx, res.CID)
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, envelope, got)

	has, err := be.Has(ctx, res.CID)
	require.NoError(t, err)
	require.True(t, has)
}

func TestIntegrationEmbeddedRoundTripLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	be, ctx := newOfflineBackend(t)

	envelope := make([]byte, 4*1024*1024) // 4 MiB, dag-pb path
	_, _ = rand.Read(envelope)
	res, err := be.AddDeterministic(ctx, envelope)
	require.NoError(t, err)
	require.Equal(t, ipfs.CodecDagPB, res.Codec, ">1MiB envelope must use dag-pb codec")
	require.GreaterOrEqual(t, len(res.Blocks), 2, "multi-block result")
	// Block index ordering MUST be deterministic.
	for i, b := range res.Blocks {
		require.Equal(t, i, b.Index)
		require.Greater(t, b.Size, 0)
	}

	rc, err := be.Get(ctx, res.CID)
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	require.Equal(t, envelope, got)
}

func TestIntegrationEmbeddedSameBytesSameCID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	be1, ctx1 := newOfflineBackend(t)
	be2, ctx2 := newOfflineBackend(t)

	envelope := []byte("deterministic bytes → identical CID")
	res1, err := be1.AddDeterministic(ctx1, envelope)
	require.NoError(t, err)
	res2, err := be2.AddDeterministic(ctx2, envelope)
	require.NoError(t, err)

	require.Equal(t, res1.CID.String(), res2.CID.String(),
		"identical bytes MUST produce identical CIDs across independently-initialised Kubo nodes")
}

func TestIntegrationEmbeddedUnpin(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	be, ctx := newOfflineBackend(t)

	envelope := []byte("to be unpinned")
	res, err := be.AddDeterministic(ctx, envelope)
	require.NoError(t, err)

	require.NoError(t, be.Unpin(ctx, res.CID))

	// After unpin, the local Kubo can garbage-collect; we don't run GC
	// in the test, so the bytes may still be in the blockstore, but Has
	// reports "not pinned" via the absence of the pin record. We assert
	// the weaker invariant: Unpin succeeded without error and re-pin
	// works.
	require.NoError(t, be.Pin(ctx, res.CID))
}

func TestIntegrationEmbeddedBlockGet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	be, ctx := newOfflineBackend(t)

	envelope := make([]byte, 600*1024) // 600 KiB; under threshold so raw
	_, _ = rand.Read(envelope)
	res, err := be.AddDeterministic(ctx, envelope)
	require.NoError(t, err)

	has, err := be.BlockstoreHas(ctx, res.CID)
	require.NoError(t, err)
	require.True(t, has)

	bytesGot, err := be.BlockGet(ctx, res.CID)
	require.NoError(t, err)
	require.Equal(t, envelope, bytesGot,
		"raw block bytes == envelope bytes (no UnixFS wrapping)")
}

func TestIntegrationEmbeddedRefusesOpenOnHardeningViolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	repo := t.TempDir()
	// No swarm key — ValidateConfig must refuse before the node starts.
	_, err := ipfs.NewEmbedded(ctx, ipfs.EmbeddedOptions{
		RepoPath:     repo,
		Mode:         ipfs.ModePrivate,
		SwarmKeyPath: filepath.Join(t.TempDir(), "missing.key"),
		Online:       false,
	})
	require.ErrorIs(t, err, ipfs.ErrSwarmKeyMissing)
}
```

- [ ] **Step 10.2: Run tests, verify they fail**

```bash
go test ./internal/ipfs/... -run Integration
```

Expected: FAIL — `undefined: ipfs.NewEmbedded`.

- [ ] **Step 10.3: Implement embedded.go**

Create `internal/ipfs/embedded.go`. The skeleton below shows the structure; concrete Kubo API calls may need minor tweaks against the resolved Kubo version.

```go
package ipfs

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/ipfs/boxo/files"
	"github.com/ipfs/boxo/path"
	"github.com/ipfs/go-cid"
	kuboconfig "github.com/ipfs/kubo/config"
	"github.com/ipfs/kubo/core"
	"github.com/ipfs/kubo/core/coreapi"
	"github.com/ipfs/kubo/core/coreiface/options"
	"github.com/ipfs/kubo/plugin/loader"
	"github.com/ipfs/kubo/repo/fsrepo"
	mh "github.com/multiformats/go-multihash"
)

// EmbeddedOptions configures the in-process Kubo node.
type EmbeddedOptions struct {
	// RepoPath is the directory holding the Kubo fsrepo. Must be
	// writable; will be initialised if empty.
	RepoPath string

	// Mode selects the hardening profile.
	Mode Mode

	// SwarmKeyPath is the file containing /key/swarm/psk/1.0.0/.
	// Required in ModePrivate; ignored in ModePublicArchivalDHT.
	SwarmKeyPath string

	// Online controls whether libp2p starts. Tests typically pass false.
	// Production passes true.
	Online bool
}

// EmbeddedBackend is the in-process Kubo implementation of Backend.
type EmbeddedBackend struct {
	node    *core.IpfsNode
	api     coreapi.CoreAPI
	repoDir string
}

// NewEmbedded initialises (if necessary), opens, validates, and boots
// the embedded Kubo node. The returned backend MUST be Closed on
// shutdown.
//
// NewEmbedded refuses to return a backend until ValidateConfig has
// passed against the loaded Kubo config — this is the refuse-to-start
// floor.
func NewEmbedded(ctx context.Context, opts EmbeddedOptions) (*EmbeddedBackend, error) {
	// Ensure plugins (datastore, etc.) are loaded exactly once per
	// process. NewPluginLoader is safe to call multiple times against
	// the same path — it short-circuits if already initialised.
	plugins, err := loader.NewPluginLoader(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("ipfs embedded: plugin loader: %w", err)
	}
	if err := plugins.Initialize(); err != nil {
		return nil, fmt.Errorf("ipfs embedded: plugin init: %w", err)
	}
	if err := plugins.Inject(); err != nil {
		return nil, fmt.Errorf("ipfs embedded: plugin inject: %w", err)
	}

	if !fsrepo.IsInitialized(opts.RepoPath) {
		cfg, err := kuboconfig.Init(io.Discard, 2048)
		if err != nil {
			return nil, fmt.Errorf("ipfs embedded: config init: %w", err)
		}
		applyHardeningDefaults(cfg, opts.Mode)
		if err := fsrepo.Init(opts.RepoPath, cfg); err != nil {
			return nil, fmt.Errorf("ipfs embedded: fsrepo init: %w", err)
		}
	}

	repo, err := fsrepo.Open(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("ipfs embedded: fsrepo open: %w", err)
	}

	loadedCfg, err := repo.Config()
	if err != nil {
		_ = repo.Close()
		return nil, fmt.Errorf("ipfs embedded: load config: %w", err)
	}
	ourCfg := translateKuboConfig(loadedCfg)
	if err := ValidateConfig(ourCfg, opts.Mode, opts.SwarmKeyPath); err != nil {
		_ = repo.Close()
		return nil, err
	}

	node, err := core.NewNode(ctx, &core.BuildCfg{
		Repo:   repo,
		Online: opts.Online,
	})
	if err != nil {
		_ = repo.Close()
		return nil, fmt.Errorf("ipfs embedded: new node: %w", err)
	}

	api, err := coreapi.NewCoreAPI(node)
	if err != nil {
		_ = node.Close()
		return nil, fmt.Errorf("ipfs embedded: coreapi: %w", err)
	}

	return &EmbeddedBackend{node: node, api: api, repoDir: opts.RepoPath}, nil
}

// applyHardeningDefaults mutates a fresh Kubo config to satisfy
// KUBO_HARDENING.md in the requested mode. ValidateConfig is the
// authoritative gate; this function exists so first-time init produces
// a config that already passes the validator.
func applyHardeningDefaults(cfg *kuboconfig.Config, mode Mode) {
	cfg.Discovery.MDNS.Enabled = false
	cfg.Bootstrap = []string{}
	cfg.Addresses.API = kuboconfig.Strings{"/ip4/127.0.0.1/tcp/5001"}
	cfg.Addresses.Gateway = kuboconfig.Strings{"/ip4/127.0.0.1/tcp/8080"}
	cfg.Swarm.DisableNatPortMap = true

	if mode == ModePrivate {
		// Routing.Type and Provider/Reprovider are pointer-y in modern
		// Kubo configs; set via the spec-mandated values.
		cfg.Routing.Type = kuboconfig.NewOptionalString("none")
		cfg.Provider.Strategy = ""
		cfg.Reprovider.Strategy = ""
	} else {
		// ModePublicArchivalDHT: leave Kubo defaults for routing.
	}
}

// translateKuboConfig converts the loaded Kubo config into our pure-Go
// KuboConfig struct (the type ValidateConfig accepts). We translate
// rather than import Kubo's types into the validator to keep the
// validator stable across Kubo upgrades.
func translateKuboConfig(c *kuboconfig.Config) KuboConfig {
	out := KuboConfig{
		Discovery: DiscoveryConfig{MDNS: MDNSConfig{Enabled: c.Discovery.MDNS.Enabled}},
		Provider:  ProviderConfig{Strategy: c.Provider.Strategy},
		Reprovider: ReproviderConfig{
			Strategy: c.Reprovider.Strategy,
		},
		Swarm: SwarmConfig{DisableNatPortMap: c.Swarm.DisableNatPortMap},
	}
	if c.Routing.Type != nil {
		out.Routing.Type = c.Routing.Type.String()
	}
	if len(c.Addresses.API) > 0 {
		out.Addresses.API = c.Addresses.API[0]
	}
	if len(c.Addresses.Gateway) > 0 {
		out.Addresses.Gateway = c.Addresses.Gateway[0]
	}
	out.Bootstrap = append(out.Bootstrap, c.Bootstrap...)
	return out
}

// AddDeterministic dispatches between the raw-codec shortcut and the
// dag-pb UnixFS pipeline based on envelope size, per IPFS_IMPORT_RULES.md.
func (b *EmbeddedBackend) AddDeterministic(ctx context.Context, envelope []byte) (AddResult, error) {
	size := int64(len(envelope))
	if ShouldUseRawCodec(size) {
		return b.addRaw(ctx, envelope)
	}
	return b.addDagPB(ctx, envelope)
}

func (b *EmbeddedBackend) addRaw(ctx context.Context, envelope []byte) (AddResult, error) {
	stat, err := b.api.Block().Put(ctx, bytes.NewReader(envelope),
		options.Block.Format(CodecRaw),
		options.Block.Hash(mh.SHA2_256, -1),
		options.Block.Pin(true),
	)
	if err != nil {
		return AddResult{}, fmt.Errorf("ipfs embedded: block put: %w", err)
	}
	c := stat.Path().RootCid()
	return AddResult{
		CID:          c,
		EnvelopeSize: int64(len(envelope)),
		Codec:        CodecRaw,
		Blocks:       []Block{{CID: c, Index: 0, Size: len(envelope)}},
		MerkleRoot:   c,
	}, nil
}

func (b *EmbeddedBackend) addDagPB(ctx context.Context, envelope []byte) (AddResult, error) {
	res, err := b.api.Unixfs().Add(ctx,
		files.NewBytesFile(envelope),
		options.Unixfs.CidVersion(1),
		options.Unixfs.Hash(mh.SHA2_256),
		options.Unixfs.RawLeaves(true),
		options.Unixfs.Chunker(ChunkerSpec),
		options.Unixfs.Layout(options.BalancedLayout),
		options.Unixfs.Pin(true),
	)
	if err != nil {
		return AddResult{}, fmt.Errorf("ipfs embedded: unixfs add: %w", err)
	}
	rootCid := res.RootCid()

	blocks, err := b.enumerateBlocks(ctx, rootCid)
	if err != nil {
		return AddResult{}, fmt.Errorf("ipfs embedded: enumerate blocks: %w", err)
	}
	return AddResult{
		CID:          rootCid,
		EnvelopeSize: int64(len(envelope)),
		Codec:        CodecDagPB,
		Blocks:       blocks,
		MerkleRoot:   rootCid,
	}, nil
}

// enumerateBlocks walks the DAG rooted at root and emits a Block per
// leaf in DAG-traversal order. Used by AddDeterministic to populate
// blob_blocks rows.
func (b *EmbeddedBackend) enumerateBlocks(ctx context.Context, root cid.Cid) ([]Block, error) {
	// Walk the DAG using the Object().Get(...) -> Links() pattern.
	// For the M2 deliverable we only need leaf-level enumeration; the
	// integrity audit recomputes hashes per-leaf via BlockGet.
	out := []Block{}
	if err := b.walkLeaves(ctx, root, 0, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (b *EmbeddedBackend) walkLeaves(ctx context.Context, c cid.Cid, idx int, out *[]Block) error {
	node, err := b.api.Object().Get(ctx, path.FromCid(c))
	if err != nil {
		// If c is a raw leaf, Object().Get returns an error specific to
		// raw codec; we treat that as "this is a leaf" and record its
		// block.
		raw, errBlock := b.api.Block().Get(ctx, path.FromCid(c))
		if errBlock != nil {
			return fmt.Errorf("walk: %w (object) / %w (block)", err, errBlock)
		}
		bytesData, errRead := io.ReadAll(raw)
		if errRead != nil {
			return errRead
		}
		*out = append(*out, Block{CID: c, Index: len(*out), Size: len(bytesData)})
		return nil
	}
	links := node.Links()
	if len(links) == 0 {
		// Internal node with no links (rare); record as leaf.
		*out = append(*out, Block{CID: c, Index: len(*out), Size: int(node.Size())})
		return nil
	}
	for _, l := range links {
		if err := b.walkLeaves(ctx, l.Cid, len(*out), out); err != nil {
			return err
		}
	}
	return nil
}

// Get returns the (reassembled) bytes for c. For raw-codec CIDs this
// is the single block; for dag-pb CIDs this is the UnixFS file content.
func (b *EmbeddedBackend) Get(ctx context.Context, c cid.Cid) (io.ReadCloser, error) {
	node, err := b.api.Unixfs().Get(ctx, path.FromCid(c))
	if err != nil {
		return nil, fmt.Errorf("ipfs embedded: unixfs get: %w", err)
	}
	file, ok := node.(files.File)
	if !ok {
		_ = node.Close()
		return nil, fmt.Errorf("ipfs embedded: get returned non-file node for cid %s", c)
	}
	return file, nil
}

func (b *EmbeddedBackend) Has(ctx context.Context, c cid.Cid) (bool, error) {
	pins, err := b.api.Pin().Ls(ctx, options.Pin.Ls.Recursive())
	if err != nil {
		return false, fmt.Errorf("ipfs embedded: pin ls: %w", err)
	}
	defer func() {
		// Drain the channel even if we found our pin early.
		for range pins {
		}
	}()
	for p := range pins {
		if p.Err() != nil {
			return false, p.Err()
		}
		if p.Path().RootCid().Equals(c) {
			return true, nil
		}
	}
	return false, nil
}

func (b *EmbeddedBackend) Pin(ctx context.Context, c cid.Cid) error {
	return b.api.Pin().Add(ctx, path.FromCid(c))
}

func (b *EmbeddedBackend) Unpin(ctx context.Context, c cid.Cid) error {
	return b.api.Pin().Rm(ctx, path.FromCid(c))
}

func (b *EmbeddedBackend) BlockstoreHas(ctx context.Context, c cid.Cid) (bool, error) {
	_, err := b.api.Block().Stat(ctx, path.FromCid(c))
	if err != nil {
		// Distinguish "not found" from real errors; Kubo's Block().Stat
		// returns an err whose .Error() contains "not found" — we treat
		// any error as not-found and let real I/O errors surface up the
		// stack via subsequent calls.
		return false, nil
	}
	return true, nil
}

func (b *EmbeddedBackend) BlockGet(ctx context.Context, c cid.Cid) ([]byte, error) {
	r, err := b.api.Block().Get(ctx, path.FromCid(c))
	if err != nil {
		return nil, fmt.Errorf("ipfs embedded: block get %s: %w", c, err)
	}
	return io.ReadAll(r)
}

func (b *EmbeddedBackend) Close(ctx context.Context) error {
	if b.node == nil {
		return nil
	}
	err := b.node.Close()
	b.node = nil
	b.api = nil
	return err
}
```

- [ ] **Step 10.4: Build and run integration tests**

```bash
go mod tidy
go build ./internal/ipfs/...
go test ./internal/ipfs/... -run Integration -count=1
```

Expected: PASS. First run pulls Kubo deps and may take 1-3 minutes. Each test then takes ~3-8 s to spin up an offline node. The validator-violation test (`RefusesOpenOnHardeningViolation`) fails fast (~1 s).

If the build fails with `cannot find module github.com/ipfs/boxo/path` or similar: the package may have moved between Kubo versions; consult `go doc github.com/ipfs/boxo/path` against the resolved version and adjust imports. Surface the import-path shift in the commit message.

- [ ] **Step 10.5: Commit**

```bash
git add internal/ipfs/embedded.go internal/ipfs/embedded_test.go go.mod go.sum
git commit -s -m "feat(ipfs): EmbeddedBackend with deterministic Add and refuse-to-start

In-process Kubo node configured per KUBO_HARDENING.md + IPFS_IMPORT_RULES.md.
Two import paths (raw codec ≤1MiB, dag-pb UnixFS above). ValidateConfig
runs before core.NewNode so hardening violations refuse cleanly without
ever opening the libp2p stack."
```

---

### Task 11: Jobs types + queue (TDD with testcontainers)

**Files:**
- Create: `internal/jobs/errors.go`
- Create: `internal/jobs/types.go`
- Create: `internal/jobs/backoff.go`
- Create: `internal/jobs/backoff_test.go`
- Create: `internal/jobs/queue.go`
- Create: `internal/jobs/queue_test.go`

- [ ] **Step 11.1: Write the backoff unit test**

Create `internal/jobs/backoff_test.go`:

```go
package jobs_test

import (
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/jobs"
	"github.com/stretchr/testify/require"
)

func TestBackoffGrowsExponentiallyUntilCap(t *testing.T) {
	t.Parallel()
	// Attempts here are post-increment: attempts=1 means the first retry.
	tests := []struct {
		attempts int
		expect   time.Duration
	}{
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{4, 40 * time.Second},
		{5, 80 * time.Second},
		{6, 160 * time.Second},
		{7, 300 * time.Second}, // cap
		{8, 300 * time.Second},
		{20, 300 * time.Second},
	}
	for _, tc := range tests {
		require.Equal(t, tc.expect, jobs.Backoff(tc.attempts), "attempts=%d", tc.attempts)
	}
}

func TestBackoffZeroAttemptsReturnsBase(t *testing.T) {
	t.Parallel()
	require.Equal(t, 5*time.Second, jobs.Backoff(0))
}
```

- [ ] **Step 11.2: Run, verify fails**

```bash
go test ./internal/jobs/...
```

Expected: FAIL — undefined `jobs.Backoff`.

- [ ] **Step 11.3: Implement backoff.go**

```go
// Package jobs is Nova's Postgres-backed job queue. Workers across the
// coordinator's milestones (integrity audits, scheduled tombstones,
// derivative prewarming, master-key rotation, webhook emission) all
// consume from this queue. The queue is partitioned by created_at
// monthly; see migration 0002_jobs.sql.
package jobs

import "time"

// Backoff returns the delay to apply before a job becomes eligible
// after a retryable failure. Exponential growth (5s, 10s, 20s, ...)
// capped at 5 minutes. The shape of the curve is documented in
// docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md
// § "Job lifecycle".
//
// Attempts is the count after the current failure (i.e., the row's
// attempts column post-increment). Attempts=0 returns the base delay
// so first-retry latency is consistent with subsequent retries.
func Backoff(attempts int) time.Duration {
	const base = 5 * time.Second
	const cap = 5 * time.Minute

	delay := base
	for i := 0; i < attempts-1; i++ {
		delay *= 2
		if delay > cap {
			return cap
		}
	}
	if delay > cap {
		return cap
	}
	return delay
}
```

- [ ] **Step 11.4: Verify backoff test passes**

```bash
go test ./internal/jobs/... -run Backoff
```

Expected: PASS.

- [ ] **Step 11.5: Create errors.go**

```go
package jobs

import "errors"

var (
	// ErrNoJobsAvailable: Lease found no pending jobs. Callers should
	// poll again after a short sleep.
	ErrNoJobsAvailable = errors.New("jobs: no jobs available")

	// ErrJobNotFound: the job id does not match any row, or matches a
	// row in a terminal state.
	ErrJobNotFound = errors.New("jobs: job not found")

	// ErrUnknownKind: WorkerPool.Run found a job with no registered
	// handler. The job is failed with this error so operators can see
	// it in the admin UI and either register the handler or delete
	// the dead row.
	ErrUnknownKind = errors.New("jobs: unknown kind")
)
```

- [ ] **Step 11.6: Create types.go**

```go
package jobs

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Job is the leased work-item view returned by Queue.Lease.
type Job struct {
	ID          uuid.UUID
	Kind        string
	Payload     []byte
	Attempts    int
	MaxAttempts int
	CreatedAt   time.Time
}

// Handler is the user-supplied function that processes a leased job.
// Returning nil marks the job completed; returning an error transitions
// it to pending (retryable, if attempts < max_attempts) or dead.
type Handler func(ctx context.Context, payload []byte) error

// State is the lifecycle state of a row in the jobs table.
type State string

const (
	StatePending   State = "pending"
	StateLeased    State = "leased"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
	StateDead      State = "dead"
)

// EnqueueOpt mutates the row inserted by Queue.Enqueue. Currently only
// MaxAttempts is exposed; add NotBefore and PartitionHint in later
// milestones if needed.
type EnqueueOpt func(*enqueueParams)

type enqueueParams struct {
	maxAttempts int
	notBefore   time.Time
}

// WithMaxAttempts overrides the default max_attempts (5) for a specific
// job. Use higher counts for idempotent kinds; lower counts for kinds
// where failure indicates a non-transient problem.
func WithMaxAttempts(n int) EnqueueOpt {
	return func(p *enqueueParams) { p.maxAttempts = n }
}

// WithNotBefore schedules the job for later execution. Pass time.Now()
// for "asap" (the default behavior).
func WithNotBefore(t time.Time) EnqueueOpt {
	return func(p *enqueueParams) { p.notBefore = t }
}
```

- [ ] **Step 11.7: Write the failing queue integration test**

Create `internal/jobs/queue_test.go`:

```go
package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/jobs"
	"github.com/stretchr/testify/require"
)

func TestIntegrationQueueEnqueueAndLease(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	q := jobs.NewQueue(pool)

	id, err := q.Enqueue(ctx, "test.echo", []byte(`{"msg":"hi"}`))
	require.NoError(t, err)
	require.NotEmpty(t, id)

	job, err := q.Lease(ctx, 30*time.Second)
	require.NoError(t, err)
	require.Equal(t, id, job.ID.String())
	require.Equal(t, "test.echo", job.Kind)
	require.Equal(t, []byte(`{"msg":"hi"}`), job.Payload)
	require.Equal(t, 0, job.Attempts)
}

func TestIntegrationQueueLeaseEmptyReturnsErr(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	q := jobs.NewQueue(pool)

	_, err := q.Lease(ctx, 30*time.Second)
	require.ErrorIs(t, err, jobs.ErrNoJobsAvailable)
}

func TestIntegrationQueueLeaseSkipLocked(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	q := jobs.NewQueue(pool)

	_, err := q.Enqueue(ctx, "test.echo", []byte("a"))
	require.NoError(t, err)
	_, err = q.Enqueue(ctx, "test.echo", []byte("b"))
	require.NoError(t, err)

	j1, err := q.Lease(ctx, 30*time.Second)
	require.NoError(t, err)
	j2, err := q.Lease(ctx, 30*time.Second)
	require.NoError(t, err)
	require.NotEqual(t, j1.ID, j2.ID, "two concurrent leases must yield distinct jobs")

	_, err = q.Lease(ctx, 30*time.Second)
	require.ErrorIs(t, err, jobs.ErrNoJobsAvailable, "third lease finds nothing")
}

func TestIntegrationQueueCompleteRemovesFromLeased(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	q := jobs.NewQueue(pool)

	_, err := q.Enqueue(ctx, "test.echo", []byte("c"))
	require.NoError(t, err)
	job, err := q.Lease(ctx, 30*time.Second)
	require.NoError(t, err)
	require.NoError(t, q.Complete(ctx, job.ID.String()))

	_, err = q.Lease(ctx, 30*time.Second)
	require.ErrorIs(t, err, jobs.ErrNoJobsAvailable)
}

func TestIntegrationQueueFailRetryableSchedulesBackoff(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	q := jobs.NewQueue(pool)

	_, err := q.Enqueue(ctx, "test.echo", []byte("d"))
	require.NoError(t, err)
	job, err := q.Lease(ctx, 30*time.Second)
	require.NoError(t, err)

	require.NoError(t, q.Fail(ctx, job.ID.String(), errors.New("transient")))

	// The row should be pending again, but not_before is in the future.
	_, err = q.Lease(ctx, 30*time.Second)
	require.ErrorIs(t, err, jobs.ErrNoJobsAvailable,
		"row is pending but not_before guards against immediate re-lease")
}

func TestIntegrationQueueFailExhaustsToDead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	q := jobs.NewQueue(pool)

	id, err := q.Enqueue(ctx, "test.echo", []byte("e"), jobs.WithMaxAttempts(2))
	require.NoError(t, err)

	for i := 0; i < 2; i++ {
		// Push not_before to now so the next Lease succeeds.
		_, err = pool.Exec(ctx, `UPDATE jobs SET not_before = now() WHERE id = $1`, id)
		require.NoError(t, err)

		j, err := q.Lease(ctx, 30*time.Second)
		require.NoError(t, err)
		require.NoError(t, q.Fail(ctx, j.ID.String(), errors.New("still failing")))
	}

	// After max_attempts failures, the row is dead.
	var state string
	err = pool.QueryRow(ctx, `SELECT state::text FROM jobs WHERE id = $1`, id).Scan(&state)
	require.NoError(t, err)
	require.Equal(t, "dead", state)
}

func TestIntegrationQueueReclaimExpiredLeases(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	q := jobs.NewQueue(pool)

	_, err := q.Enqueue(ctx, "test.echo", []byte("f"))
	require.NoError(t, err)
	_, err = q.Lease(ctx, 30*time.Second)
	require.NoError(t, err)

	// Force the lease into the past.
	_, err = pool.Exec(ctx, `UPDATE jobs SET lease_until = now() - interval '1 minute' WHERE state = 'leased'`)
	require.NoError(t, err)

	n, err := q.ReclaimExpiredLeases(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	// Now Lease can pick it up again.
	j, err := q.Lease(ctx, 30*time.Second)
	require.NoError(t, err)
	require.NotEmpty(t, j.ID)
}
```

- [ ] **Step 11.8: Run tests, verify they fail**

```bash
go test ./internal/jobs/... -run Integration
```

Expected: FAIL — `undefined: jobs.NewQueue`.

- [ ] **Step 11.9: Implement queue.go**

```go
package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Queue is a Postgres-backed FIFO job queue. The queue does not pool
// across processes — there is one Queue per coordinator process —
// but it is safe to share across goroutines within a process.
//
// All public methods take a context; honor it for cancellation and
// timeouts.
type Queue struct {
	pool *pgxpool.Pool
}

// NewQueue returns a Queue backed by the given pool.
func NewQueue(pool *pgxpool.Pool) *Queue {
	return &Queue{pool: pool}
}

// Enqueue inserts a pending row and returns the new job id (string form
// of the row's UUID).
//
// Default max_attempts is 5; override via WithMaxAttempts.
// Default not_before is now(); override via WithNotBefore.
func (q *Queue) Enqueue(ctx context.Context, kind string, payload []byte, opts ...EnqueueOpt) (string, error) {
	if kind == "" {
		return "", errors.New("jobs: enqueue: kind is required")
	}
	if payload == nil {
		payload = []byte(`{}`)
	}

	p := enqueueParams{maxAttempts: 5, notBefore: time.Now()}
	for _, o := range opts {
		o(&p)
	}

	var id uuid.UUID
	err := q.pool.QueryRow(ctx, `
		INSERT INTO jobs (kind, payload, max_attempts, not_before)
		VALUES ($1, $2::jsonb, $3, $4)
		RETURNING id
	`, kind, string(payload), p.maxAttempts, p.notBefore).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("jobs: enqueue: %w", err)
	}
	return id.String(), nil
}

// Lease atomically claims one pending job whose not_before is in the
// past. Returns ErrNoJobsAvailable if no row matches. Sets lease_until
// to now() + leaseDuration; the worker MUST complete or fail before
// the lease expires, otherwise the reclaim ticker will return the row
// to pending.
//
// Lease uses SELECT … FOR UPDATE SKIP LOCKED so concurrent leasers
// each receive distinct rows without coordination.
func (q *Queue) Lease(ctx context.Context, leaseDuration time.Duration) (*Job, error) {
	tx, err := q.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("jobs: lease: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		id        uuid.UUID
		kind      string
		payload   []byte
		attempts  int
		maxAttn   int
		createdAt time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT id, kind, payload::text::bytea, attempts, max_attempts, created_at
		FROM jobs
		WHERE state = 'pending' AND not_before <= now()
		ORDER BY created_at ASC, id ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED
	`).Scan(&id, &kind, &payload, &attempts, &maxAttn, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoJobsAvailable
	}
	if err != nil {
		return nil, fmt.Errorf("jobs: lease: select: %w", err)
	}

	_, err = tx.Exec(ctx, `
		UPDATE jobs
		   SET state = 'leased', lease_until = now() + ($2 || ' seconds')::interval
		 WHERE id = $1 AND created_at = $3
	`, id, fmt.Sprintf("%d", int(leaseDuration.Seconds())), createdAt)
	if err != nil {
		return nil, fmt.Errorf("jobs: lease: update: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("jobs: lease: commit: %w", err)
	}

	return &Job{
		ID:          id,
		Kind:        kind,
		Payload:     payload,
		Attempts:    attempts,
		MaxAttempts: maxAttn,
		CreatedAt:   createdAt,
	}, nil
}

// Complete transitions a leased job to 'completed'. No-op if the row
// is already in a terminal state.
func (q *Queue) Complete(ctx context.Context, jobID string) error {
	id, err := uuid.Parse(jobID)
	if err != nil {
		return fmt.Errorf("jobs: complete: bad uuid: %w", err)
	}
	tag, err := q.pool.Exec(ctx, `
		UPDATE jobs
		   SET state = 'completed', lease_until = NULL
		 WHERE id = $1 AND state = 'leased'
	`, id)
	if err != nil {
		return fmt.Errorf("jobs: complete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrJobNotFound
	}
	return nil
}

// Fail records a handler error. If attempts+1 < max_attempts the row
// returns to 'pending' with not_before set per Backoff(attempts+1).
// Otherwise the row transitions to 'dead' for operator inspection.
func (q *Queue) Fail(ctx context.Context, jobID string, cause error) error {
	id, err := uuid.Parse(jobID)
	if err != nil {
		return fmt.Errorf("jobs: fail: bad uuid: %w", err)
	}
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}

	tx, err := q.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("jobs: fail: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var attempts, maxAttempts int
	err = tx.QueryRow(ctx, `
		SELECT attempts, max_attempts FROM jobs WHERE id = $1 AND state = 'leased'
	`, id).Scan(&attempts, &maxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrJobNotFound
	}
	if err != nil {
		return fmt.Errorf("jobs: fail: select: %w", err)
	}
	attempts++

	if attempts >= maxAttempts {
		_, err = tx.Exec(ctx, `
			UPDATE jobs
			   SET state = 'dead', attempts = $2, last_error = $3, lease_until = NULL
			 WHERE id = $1
		`, id, attempts, msg)
	} else {
		delay := Backoff(attempts)
		_, err = tx.Exec(ctx, `
			UPDATE jobs
			   SET state = 'pending', attempts = $2, last_error = $3,
			       lease_until = NULL,
			       not_before = now() + ($4 || ' seconds')::interval
			 WHERE id = $1
		`, id, attempts, msg, fmt.Sprintf("%d", int(delay.Seconds())))
	}
	if err != nil {
		return fmt.Errorf("jobs: fail: update: %w", err)
	}
	return tx.Commit(ctx)
}

// ReclaimExpiredLeases returns rows whose lease has expired back to
// 'pending'. Called by WorkerPool.Run on a 10-second ticker.
func (q *Queue) ReclaimExpiredLeases(ctx context.Context) (int, error) {
	tag, err := q.pool.Exec(ctx, `
		UPDATE jobs
		   SET state = 'pending', lease_until = NULL
		 WHERE state = 'leased' AND lease_until < now()
	`)
	if err != nil {
		return 0, fmt.Errorf("jobs: reclaim: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
```

- [ ] **Step 11.10: Run tests, verify they pass**

```bash
go test ./internal/jobs/... -count=1
```

Expected: PASS — all 7 integration tests + the backoff unit test.

- [ ] **Step 11.11: Commit**

```bash
git add internal/jobs/errors.go internal/jobs/types.go internal/jobs/backoff.go internal/jobs/backoff_test.go internal/jobs/queue.go internal/jobs/queue_test.go
git commit -s -m "feat(jobs): Postgres-backed job queue with SKIP LOCKED leasing

Enqueue/Lease/Complete/Fail/ReclaimExpiredLeases against the partitioned
jobs table from migration 0002. Exponential backoff with 5-minute cap.
Integration tests via testcontainers cover concurrent leases, retry
semantics, and lease reclaim."
```

---

### Task 12: Jobs worker pool (TDD with testcontainers)

**Files:**
- Create: `internal/jobs/worker.go`
- Create: `internal/jobs/worker_test.go`

- [ ] **Step 12.1: Write the failing test**

Create `internal/jobs/worker_test.go`:

```go
package jobs_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/jobs"
	"github.com/stretchr/testify/require"
)

func TestIntegrationWorkerProcessesEnqueuedJob(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	q := jobs.NewQueue(pool)

	var calls atomic.Int32
	done := make(chan struct{})
	wp := jobs.NewWorkerPool(q, jobs.WorkerOptions{
		Concurrency:   2,
		PollInterval:  50 * time.Millisecond,
		LeaseDuration: 30 * time.Second,
	})
	wp.RegisterHandler("test.count", func(ctx context.Context, payload []byte) error {
		if calls.Add(1) == 3 {
			close(done)
		}
		return nil
	})

	for i := 0; i < 3; i++ {
		_, err := q.Enqueue(ctx, "test.count", nil)
		require.NoError(t, err)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	go wp.Run(runCtx)

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("worker did not process 3 jobs within 15s")
	}
	runCancel()
}

func TestIntegrationWorkerRetriesRetryableFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	q := jobs.NewQueue(pool)

	var attempts atomic.Int32
	done := make(chan struct{})

	wp := jobs.NewWorkerPool(q, jobs.WorkerOptions{
		Concurrency:   1,
		PollInterval:  50 * time.Millisecond,
		LeaseDuration: 30 * time.Second,
	})
	wp.RegisterHandler("test.fail-twice", func(ctx context.Context, payload []byte) error {
		n := attempts.Add(1)
		if n < 3 {
			return errors.New("flaky")
		}
		close(done)
		return nil
	})

	_, err := q.Enqueue(ctx, "test.fail-twice", nil, jobs.WithMaxAttempts(5))
	require.NoError(t, err)

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go wp.Run(runCtx)

	// Worker uses Backoff() so retries wait 5s, 10s, ... To make the
	// test fast, advance not_before manually after each failure.
	go func() {
		for {
			select {
			case <-runCtx.Done():
				return
			case <-time.After(200 * time.Millisecond):
				_, _ = pool.Exec(runCtx,
					`UPDATE jobs SET not_before = now() WHERE state = 'pending' AND not_before > now()`)
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("expected 3 attempts, got %d", attempts.Load())
	}
}

func TestIntegrationWorkerHandlesUnknownKindAsDead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	q := jobs.NewQueue(pool)
	wp := jobs.NewWorkerPool(q, jobs.WorkerOptions{
		Concurrency:   1,
		PollInterval:  50 * time.Millisecond,
		LeaseDuration: 5 * time.Second,
	})
	// Intentionally no handler registered for "test.unknown".

	id, err := q.Enqueue(ctx, "test.unknown", nil, jobs.WithMaxAttempts(1))
	require.NoError(t, err)

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go wp.Run(runCtx)

	require.Eventually(t, func() bool {
		var state string
		_ = pool.QueryRow(runCtx, `SELECT state::text FROM jobs WHERE id = $1`, id).Scan(&state)
		return state == "dead"
	}, 10*time.Second, 100*time.Millisecond)
}

func TestIntegrationWorkerStopsOnContextCancel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := dbtest.New(t, ctx)
	q := jobs.NewQueue(pool)
	wp := jobs.NewWorkerPool(q, jobs.WorkerOptions{
		Concurrency:   1,
		PollInterval:  50 * time.Millisecond,
		LeaseDuration: 5 * time.Second,
	})

	runCtx, runCancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		wp.Run(runCtx)
	}()
	runCancel()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop within 5s of context cancel")
	}
}
```

- [ ] **Step 12.2: Run tests, verify they fail**

```bash
go test ./internal/jobs/... -run Worker
```

Expected: FAIL — `undefined: jobs.NewWorkerPool`.

- [ ] **Step 12.3: Implement worker.go**

```go
package jobs

import (
	"context"
	"errors"
	"sync"
	"time"
)

// WorkerOptions tunes a WorkerPool. Defaults: 4 concurrent workers,
// 250ms poll interval, 30s lease duration. Choose lease duration so
// the slowest handler comfortably finishes within it; on lease expiry
// another worker re-leases and runs the handler again (handlers MUST
// be idempotent).
type WorkerOptions struct {
	Concurrency   int
	PollInterval  time.Duration
	LeaseDuration time.Duration
}

// WorkerPool consumes jobs from a Queue and dispatches them to
// registered handlers. Register all handlers BEFORE calling Run; the
// pool does not support adding handlers after Run starts.
type WorkerPool struct {
	q        *Queue
	opts     WorkerOptions
	mu       sync.RWMutex
	handlers map[string]Handler
}

// NewWorkerPool returns a pool over q with the given options. Defaults
// are applied to zero-valued option fields.
func NewWorkerPool(q *Queue, opts WorkerOptions) *WorkerPool {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 250 * time.Millisecond
	}
	if opts.LeaseDuration <= 0 {
		opts.LeaseDuration = 30 * time.Second
	}
	return &WorkerPool{
		q:        q,
		opts:     opts,
		handlers: make(map[string]Handler),
	}
}

// RegisterHandler associates kind with h. Re-registration overwrites.
// Call before Run.
func (wp *WorkerPool) RegisterHandler(kind string, h Handler) {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	wp.handlers[kind] = h
}

// Run blocks until ctx is cancelled. It spawns Concurrency leaser
// goroutines plus one reclaim ticker. Each leaser polls Lease at
// PollInterval; when Lease returns a job, the leaser dispatches to
// the registered handler.
//
// Run is safe to invoke once per WorkerPool. To restart a stopped
// pool, construct a new WorkerPool.
func (wp *WorkerPool) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < wp.opts.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wp.leaserLoop(ctx)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		wp.reclaimLoop(ctx)
	}()
	wg.Wait()
}

func (wp *WorkerPool) leaserLoop(ctx context.Context) {
	t := time.NewTicker(wp.opts.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			wp.tryOne(ctx)
		}
	}
}

func (wp *WorkerPool) tryOne(ctx context.Context) {
	job, err := wp.q.Lease(ctx, wp.opts.LeaseDuration)
	if errors.Is(err, ErrNoJobsAvailable) {
		return
	}
	if err != nil {
		// Lease errors are transient (DB blips); log and continue. We
		// don't have a structured logger plumbed in M2; later milestones
		// inject one via WorkerOptions.
		return
	}

	wp.mu.RLock()
	h, ok := wp.handlers[job.Kind]
	wp.mu.RUnlock()

	if !ok {
		_ = wp.q.Fail(ctx, job.ID.String(), ErrUnknownKind)
		return
	}

	if err := h(ctx, job.Payload); err != nil {
		_ = wp.q.Fail(ctx, job.ID.String(), err)
		return
	}
	_ = wp.q.Complete(ctx, job.ID.String())
}

func (wp *WorkerPool) reclaimLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = wp.q.ReclaimExpiredLeases(ctx)
		}
	}
}
```

- [ ] **Step 12.4: Run tests, verify they pass**

```bash
go test ./internal/jobs/... -count=1
```

Expected: PASS — all worker tests + queue tests + backoff tests.

- [ ] **Step 12.5: Commit**

```bash
git add internal/jobs/worker.go internal/jobs/worker_test.go
git commit -s -m "feat(jobs): WorkerPool with concurrent leasers and reclaim ticker

N goroutines poll Lease at PollInterval, dispatch to registered
handlers, and Complete/Fail per result. One additional goroutine runs
ReclaimExpiredLeases every 10s. Honor context cancellation in all
loops. Unknown-kind jobs Fail with ErrUnknownKind so operators can see
them in the admin UI (introduced in M11)."
```

---

### Task 13: End-to-end M2 integration test

**Files:**
- Create: `internal/integration/m2_roundtrip_test.go`

This is the exit-criterion test for M2. It composes envelope + ipfs + jobs by hand to prove the three subsystems fit together. Later milestones replace the manual wiring with the real coordinator pipeline.

- [ ] **Step 13.1: Write the failing exit test**

Create `internal/integration/m2_roundtrip_test.go`:

```go
// Package integration holds cross-package integration tests that
// exercise multiple internal subsystems together. Each test in this
// package is the exit criterion for a specific milestone; M2's test
// is m2_roundtrip_test.go.
//
// Tests use the Integration name prefix for selection in the Makefile's
// test-integration target.
package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/internal/jobs"
	"github.com/stretchr/testify/require"
)

// TestIntegrationM2EncryptImportFetchDecrypt is the M2 exit test.
//
// Setup:
//   - postgres + migrations (via dbtest)
//   - envelope keystore bootstrapped against NOVA_MASTER_KEY_V1
//   - embedded Kubo node (offline)
//   - jobs queue + worker pool, with a synthetic "m2.roundtrip" kind
//     that performs the full encrypt/import/fetch/decrypt path
//
// Exercise:
//   - generate random plaintext (one ≤ raw threshold, one above)
//   - enqueue an m2.roundtrip job carrying the plaintext
//   - worker leases, runs encrypt → wrap → AddDeterministic → Get →
//     decrypt → bytes match
//   - test observes the result via a channel
//
// Verifies:
//   - envelope.Keystore.Wrap returns a wrapped key tied to the active
//     master_key_versions.id
//   - ipfs.EmbeddedBackend.AddDeterministic round-trips bytes for both
//     the raw (≤1MiB envelope) and dag-pb (>1MiB envelope) paths
//   - jobs.WorkerPool consumes and completes the work
//   - the three subsystems compose without coupling assumptions
func TestIntegrationM2EncryptImportFetchDecrypt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping M2 integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// --- DB + keystore ---
	pool := dbtest.New(t, ctx)
	masterHex := randHex(t, envelope.KeySize)
	t.Setenv("NOVA_MASTER_KEY_V1", masterHex)
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")

	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)

	masterBytes, err := hex.DecodeString(masterHex)
	require.NoError(t, err)

	// --- IPFS embedded ---
	swarmPath := filepath.Join(t.TempDir(), "swarm.key")
	require.NoError(t, ipfs.WriteFileForTest(swarmPath,
		[]byte("/key/swarm/psk/1.0.0/\n/base16/\n"+
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n")))
	be, err := ipfs.NewEmbedded(ctx, ipfs.EmbeddedOptions{
		RepoPath:     t.TempDir(),
		Mode:         ipfs.ModePrivate,
		SwarmKeyPath: swarmPath,
		Online:       false,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = be.Close(shutdownCtx)
	})

	// --- Jobs ---
	q := jobs.NewQueue(pool)
	wp := jobs.NewWorkerPool(q, jobs.WorkerOptions{
		Concurrency:   1,
		PollInterval:  50 * time.Millisecond,
		LeaseDuration: 60 * time.Second,
	})

	// Job payload IS the plaintext. Handler:
	//   1. generates a per-blob key
	//   2. wraps it with the keystore (active master-key version)
	//   3. sanity-unwraps with the raw master to confirm composition
	//   4. v1.Encrypt(plaintext, perBlobKey)
	//   5. AddDeterministic(envelope) → CID
	//   6. Get(CID) → envelope bytes
	//   7. v1.Decrypt(envelope, perBlobKey) → recovered plaintext
	//   8. bytes-equal the input; report success on resultCh
	var handlerDone atomic.Int32
	type result struct{ ok bool }
	resultCh := make(chan result, 2)

	wp.RegisterHandler("m2.roundtrip", func(ctx context.Context, payload []byte) error {
		pbk := make([]byte, envelope.KeySize)
		if _, err := rand.Read(pbk); err != nil {
			resultCh <- result{ok: false}
			return err
		}

		wrapped, _, err := ks.Wrap(pbk)
		if err != nil {
			resultCh <- result{ok: false}
			return err
		}

		unwrapped, err := envelope.UnwrapKey(masterBytes, wrapped)
		if err != nil || !bytes.Equal(unwrapped, pbk) {
			resultCh <- result{ok: false}
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return err
		}

		env, err := envelope.V1().Encrypt(payload, pbk)
		if err != nil {
			resultCh <- result{ok: false}
			return err
		}

		addRes, err := be.AddDeterministic(ctx, env)
		if err != nil {
			resultCh <- result{ok: false}
			return err
		}

		rc, err := be.Get(ctx, addRes.CID)
		if err != nil {
			resultCh <- result{ok: false}
			return err
		}
		gotEnv, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			resultCh <- result{ok: false}
			return err
		}

		gotPlain, err := envelope.V1().Decrypt(gotEnv, pbk)
		if err != nil {
			resultCh <- result{ok: false}
			return err
		}

		if !bytes.Equal(gotPlain, payload) {
			resultCh <- result{ok: false}
			return nil
		}
		handlerDone.Add(1)
		resultCh <- result{ok: true}
		return nil
	})

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go wp.Run(runCtx)

	// Raw-codec path: 768 KiB plaintext + 48 envelope overhead ≤ 1 MiB.
	plaintext := randomBytes(t, 768*1024)
	_, err = q.Enqueue(ctx, "m2.roundtrip", plaintext)
	require.NoError(t, err)

	select {
	case r := <-resultCh:
		require.True(t, r.ok, "raw-codec round-trip handler reported failure")
	case <-time.After(2 * time.Minute):
		t.Fatal("M2 roundtrip (raw) did not complete within 2 minutes")
	}
	require.Equal(t, int32(1), handlerDone.Load())

	// dag-pb path: 3 MiB plaintext drives the chunked import.
	largePlaintext := randomBytes(t, 3*1024*1024)
	_, err = q.Enqueue(ctx, "m2.roundtrip", largePlaintext)
	require.NoError(t, err)

	select {
	case r := <-resultCh:
		require.True(t, r.ok, "dag-pb round-trip handler reported failure")
	case <-time.After(3 * time.Minute):
		t.Fatal("M2 roundtrip (dag-pb) did not complete within 3 minutes")
	}
	require.Equal(t, int32(2), handlerDone.Load())
}

// --- helpers ---

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	return hex.EncodeToString(randomBytes(t, n))
}

// Silence the unused-import warning for os when the test compiles with
// no os.Getenv lookups left; t.Setenv handles env mutation. This stub
// is removed once any other test in the package imports os directly.
var _ = os.Environ
```

- [ ] **Step 13.2: Run the M2 exit test**

```bash
go test ./internal/integration/... -run TestIntegrationM2 -count=1 -v
```

Expected: PASS — two round-trips (small + large), the handler reports success twice, `handlerDone == 2`. First run takes ~2-3 minutes (testcontainer pull + Kubo deps); subsequent runs ~60-90 s.

- [ ] **Step 13.3: Add a make target for M2 verification**

Edit `Makefile`. After the `smoke` target add:

```makefile
m2-exit:
	$(GOTESTV) ./internal/integration/... -run TestIntegrationM2 -count=1
```

Update the `help` block:

```
@echo "  m2-exit           Run the M2 exit-criterion test (env → ipfs → decrypt round-trip)"
```

- [ ] **Step 13.4: Commit**

```bash
git add internal/integration/m2_roundtrip_test.go Makefile
git commit -s -m "test(integration): M2 exit test — encrypt → import → fetch → decrypt

Composes envelope + ipfs + jobs through a synthetic m2.roundtrip job
kind. Exercises both the raw-codec (≤1MiB) and dag-pb (>1MiB) import
paths. make m2-exit runs the test in isolation."
```

---

### Task 14: M2 completion — full test pass, update master plan, tag

- [ ] **Step 14.1: Run the full M2 test surface**

```bash
make tidy
make test
make m2-exit
make smoke
```

Expected: every step PASS. Total runtime budget ~4-6 minutes on commodity hardware.

- [ ] **Step 14.2: Run the linter**

```bash
make lint
```

Expected: no warnings. Address any `golangci-lint` complaints inline; preserve the public surface (no renames of `Backend`, `Codec`, `Queue`, `WorkerPool`).

- [ ] **Step 14.3: Verify CI passes on push**

```bash
git push origin main
```

Watch GitHub Actions. Expected: `test`, `lint`, `schema-drift` are green within ~10 minutes (longer than M1 because of Kubo build).

If `test-integration` times out on the CI runner: the Kubo build is genuinely heavy. The mitigation is to split the M2 integration tests into a separate matrix job with a longer timeout. Surface this to Bug if it happens.

- [ ] **Step 14.4: Tag M2**

```bash
git tag -s m2-envelope-ipfs -m "Phase 1 M2: envelope + IPFS round-trip + jobs queue"
git push origin m2-envelope-ipfs
```

- [ ] **Step 14.5: Update the master plan to mark M2 completed**

Edit `docs/superpowers/plans/2026-05-25-phase1-single-node-mvp.md`. In the milestone table:

```
| M2 | Envelope + IPFS round-trip | **completed** | docs/superpowers/plans/2026-05-25-phase1-m2-envelope-ipfs.md |
```

- [ ] **Step 14.6: Commit and push the status update**

```bash
git add docs/superpowers/plans/2026-05-25-phase1-single-node-mvp.md
git commit -s -m "docs(plans): mark M2 completed; link M2 detailed plan"
git push origin main
```

- [ ] **Step 14.7: Open M3 planning**

The next milestone is M3 — Storage core API (read path). Open a new conversation in this repo and ask for the M3 plan to be drafted under the writing-plans skill, referencing this completed M2 as the foundation.

---

## Self-review

### Scope coverage against the design's M2 line items

| Design item | Covered by |
|---|---|
| `internal/envelope` v1 codec + Codec interface (v2 slot reserved) | Tasks 2, 3 |
| Golden test vectors | Task 5 |
| `internal/ipfs.Backend` interface | Task 7 |
| Embedded implementation via Kubo coreapi | Task 10 |
| `ValidateConfig` refuse-to-start hardening rules | Task 8 |
| Master-key wrap/unwrap | Tasks 4 (pure), 6 (DB-aware keystore) |
| `internal/jobs` queue + worker pool | Tasks 11, 12 |
| Exit: encrypt → import → fetch → decrypt → byte-equal | Task 13 |
| Exit: `Backend.ValidateConfig` rejects every hardening violation | Task 8 (table-driven covers every row of the KUBO_HARDENING.md table) |
| Deferred item from M1: sqlc | **deferred again to M3** — documented in "Out of scope" |
| Deferred item from M1: cmd/migrate master-key bootstrap | **moved to coordinator startup via envelope.Keystore.Bootstrap** — documented in "Risks and notes for the implementer" |

### Placeholder scan

No `TBD`, no `implement later`, no "add appropriate error handling" — every step contains the actual code or command. The one acknowledged hand-wavy section is the Kubo coreapi specifics in Task 10, which is gated by an explicit "Implementer note" admitting that minor API tweaks may be needed against the resolved Kubo version. This is honest scope, not a placeholder.

### Type consistency

- `Codec.Version` returns `byte`; `Decode` returns `(byte, Codec, error)`. Consistent across `envelope.go`, `v1.go`, and `envelope_test.go`.
- `Keystore.Wrap` returns `([]byte, uuid.UUID, error)`; `Keystore.Unwrap` accepts `([]byte, uuid.UUID)`. Consistent across `keystore.go` and `keystore_test.go`.
- `Backend.AddDeterministic` returns `AddResult`; the same struct is consumed by the M2 exit test. `cid.Cid` is the canonical type for CIDs throughout (not `string`).
- `Queue` method names match between `queue.go` and `worker.go`: `Lease`, `Complete`, `Fail`, `ReclaimExpiredLeases`. `WorkerPool` uses them verbatim.
- `Job.ID` is `uuid.UUID`; `Queue.Complete`/`Queue.Fail` accept `string` (the caller passes `job.ID.String()`). This is intentional — string is what flows over HTTP later, and the conversion sits at the API surface.

### Things that might surprise the implementer

1. **`internal/dbtest` package.** Introduced in Task 6 to avoid making `internal/db` import testcontainers. If the implementer is tempted to put the helper in `internal/db/dbtest.go`, push back — that would balloon production binary size by pulling docker-client libs.

2. **Vector regeneration via `-update`.** Standard Go golden-file pattern, but the `internal/envelope/test_helpers.go` file exists in the production package so the test can call into nonce-overridden Wrap/Encrypt without build tags. The functions are exported as `*ForTest`; do not call them from non-test code.

3. **`addRaw` vs `addDagPB`.** The threshold is checked against `int64(len(envelope))`, not the plaintext. The envelope is 32+plaintext+16 bytes, so a 1 MiB plaintext produces a ~1 MiB + 48 envelope — which is *above* the 1 MiB threshold and uses the dag-pb path. This is intentional and matches the spec (which thresholds on the imported bytes, not the plaintext).

4. **Embedded Kubo offline mode in tests.** `Online: false` means no libp2p swarm starts; `AddDeterministic` works because it only writes to the local blockstore. This is much faster than running with the swarm. Production sets `Online: true`.

Plan complete.

---

## Execution handoff

**Plan complete and saved to `docs/superpowers/plans/2026-05-25-phase1-m2-envelope-ipfs.md`. Two execution options:**

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration on the Kubo coreapi specifics that may need version-by-version adjustment.

**2. Inline Execution** — Execute tasks in this session using `superpowers:executing-plans`, batch execution with checkpoints. Better when Bug wants to follow each crypto decision in real time.

**Which approach?**

