# P2-M6 Possession Audits & Reputation — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make donor acks continuously testable — the coordinator spot-checks that an acked donor still holds the bytes it claimed, then moves `reputation_score`, corrects `pin_assignments` durability on failure, graduates/demotes `trust_state`, and biases source selection.

**Architecture:** A standalone in-process coordinator scheduler (`internal/audit/possession`, mirroring `internal/audit/integrity`) selects targets (two-stage weighted sampling), dispatches a synchronous mTLS `block_hash` challenge to the donor's existing inbound source server (`internal/node/source`), and verifies the **returned block bytes** by CID reconstruction (`stored.Prefix().Sum(bytes)` — works after M4.1 pruned the coordinator's local copy). Outcomes are recorded in `pin_audits` and drive reputation/trust/durability in one transaction. No new healing path — failures reuse M5's `FailPinAssignment` + `blob_replication_reconcile_queue`.

**Tech Stack:** Go 1.26 (`go.mod` 1.26.2), pgx v5 + sqlc, Kubo HTTP sidecar (`/api/v0/block/get`), Ed25519/mTLS federation transport, `go-cid` (coordinator only).

**Design doc:** `docs/superpowers/specs/phase2/2026-06-29-phase2-m6-possession-audits-design.md` (decisions D-M6-1 … D-M6-15).

## Global Constraints

- **Donor dependency boundary:** `cmd/node`'s graph must pass `scripts/check_node_deps.sh` (deny-by-default over non-stdlib deps). M6 adds **no** new donor allowlist entry — the audit endpoint reuses `internal/node/{source,ipfsclient,state,bandwidth}`, `internal/federation/{wire,transport,replay}`. The donor stays **`go-cid`-free**: CID reconstruction is coordinator-only.
- **Migrations frozen:** never modify an existing `internal/db/migrations/00NN_*.sql`. M6 adds exactly one new file `0015_possession_audits.sql`, appended to `internal/db/migrations/MANIFEST.sha256`. `scripts/check-migrations-frozen.sh` must stay green.
- **sqlc:** schema/query changes regenerate via `make sqlc-generate` (never hand-edit `internal/db/gen/*`).
- **gofmt:** only files you touch. `golangci-lint` is CI-only.
- **Naming:** the canonical webhook event is `federation.node_suspect` (spec alias `node.suspect`). The capability id `audit-block-hash/v1` already exists as `wire.CapAuditBlockHash`.
- **Receive-time authority (D10):** the pass/fail deadline uses the coordinator's `received_at`, never any donor-supplied timestamp.
- **No auto-suspension:** M6 never sets `trust_state='suspended'` automatically.

---

## File Structure

**New files:**
- `internal/db/migrations/0015_possession_audits.sql` — ALTER pin_audits/nodes + indexes + backfill.
- `internal/db/queries/possession.sql` — all M6 sqlc queries.
- `internal/federation/wire/audit.go` — challenge message + domain-separated transcript-hash helper.
- `internal/node/source/audit.go` — donor `POST /fed/v1/audit/challenge` handler.
- `internal/node/ipfsclient/blockget.go` — `BlockGetLocal` primitive (or extend `client.go`).
- `internal/audit/possession/dispatch.go` — coordinator→donor mTLS challenge client + CID verify.
- `internal/audit/possession/verify.go` — outcome classification + recording + reputation/trust/durability transaction.
- `internal/audit/possession/trust.go` — graduation/demotion policy.
- `internal/audit/possession/scheduler.go` — in-process scheduler (sampling, cadence, fast lane, startup reconcile).

**Modified files:**
- `internal/federation/wire/messages.go` — (cap already present; nothing to add unless a response code constant is needed).
- `internal/node/source/server.go` — extend `Deps`/`Server` + register the audit route.
- `internal/node/agent/*.go` — advertise `wire.CapAuditBlockHash`.
- `internal/notify/emitter.go` — `federation.node_suspect` suppression window.
- `internal/config/types.go` — `PossessionAudit` block + defaults + validation.
- `internal/api/handlers/config_admin.go` — `/settings` field metadata for the two first-class knobs.
- `cmd/coordinator/main.go` — wire the possession scheduler.
- `cmd/node/main.go` — wire the audit deps (audit-budget bucket, block reader) into `source.NewServer`.
- `cmd/novactl/*.go` — `novactl node trust clear-review|suspend|unsuspend`.
- Spec docs + `docs/ROADMAP.md` (Task 14).

---

## Task 1: Migration 0015 — schema

**Files:**
- Create: `internal/db/migrations/0015_possession_audits.sql`
- Modify: `internal/db/migrations/MANIFEST.sha256`
- Test: `internal/db/migrations/possession_state_test.go`

**Interfaces:**
- Produces: columns `pin_audits.received_at|decided_at|transcript_hash`; `nodes.trust_epoch_started_at|trust_review_required_at|trust_review_reason`; indexes `pin_assignments_acked_at_idx`, `pin_audits_recent_pass_node_blob_idx`, `pin_audits_recent_fail_node_idx`.

- [ ] **Step 1: Write the migration**

Create `internal/db/migrations/0015_possession_audits.sql` (model the header on `0014_liveness_healing.sql`):

```sql
-- 0015_possession_audits.sql
-- P2-M6: possession audits & reputation. Forward-only ALTER/reconciliation —
-- pin_audits + audit_result already exist in frozen 0001_init; this adds only the
-- D10 receive-time column, an always-set decision time, the transcript digest, the
-- trust-epoch/review columns, and scheduler-support indexes. No Phase-1 migration
-- is modified (migrations-frozen stays green).

ALTER TABLE pin_audits
    ADD COLUMN received_at     timestamptz,   -- coordinator response receive-time; NULL on timeout (D10)
    ADD COLUMN decided_at      timestamptz,   -- when the coordinator decided the outcome (always set)
    ADD COLUMN transcript_hash bytea;         -- domain-separated audit transcript digest (D-M6-3a)

ALTER TABLE nodes
    ADD COLUMN trust_epoch_started_at   timestamptz NOT NULL DEFAULT now(), -- graduation-evidence anchor
    ADD COLUMN trust_review_required_at timestamptz,                        -- set on hash-mismatch; gates graduation
    ADD COLUMN trust_review_reason      text;

-- Existing donors keep their tenure: anchor the epoch at registration, not deploy.
UPDATE nodes SET trust_epoch_started_at = joined_at;

-- New-ack fast lane: "freshly acked within 15 min".
CREATE INDEX pin_assignments_acked_at_idx
    ON pin_assignments (acked_at) WHERE state = 'acked';

-- Audit recency: most-recent pass per (node, blob).
CREATE INDEX pin_audits_recent_pass_node_blob_idx
    ON pin_audits (node_id, blob_cid, received_at DESC) WHERE result = 'pass';

-- Failure history: decided_at is set even when received_at is NULL (timeouts).
CREATE INDEX pin_audits_recent_fail_node_idx
    ON pin_audits (node_id, decided_at DESC) WHERE result = 'fail';
```

- [ ] **Step 2: Append the migration hash to the frozen manifest**

Run (regenerates the manifest line for the new file only — inspect the script first to match its format):

Run: `bash scripts/check-migrations-frozen.sh --update 2>/dev/null || sha256sum internal/db/migrations/0015_possession_audits.sql`
Then add/confirm the `0015_possession_audits.sql` line in `internal/db/migrations/MANIFEST.sha256` exactly as the other entries are formatted.

- [ ] **Step 3: Write the schema test**

Create `internal/db/migrations/possession_state_test.go` mirroring `replication_state_test.go`'s harness (spin up the test DB, run all migrations up):

```go
func TestMigration0015PossessionColumns(t *testing.T) {
    db := newMigratedTestDB(t) // same helper the sibling *_state_test.go files use
    for _, col := range []string{"received_at", "decided_at", "transcript_hash"} {
        assertColumnExists(t, db, "pin_audits", col)
    }
    for _, col := range []string{"trust_epoch_started_at", "trust_review_required_at", "trust_review_reason"} {
        assertColumnExists(t, db, "nodes", col)
    }
    // Backfill: a node's epoch equals its joined_at.
    assertTrustEpochEqualsJoinedAt(t, db)
}
```

(Reuse/extend the `assertColumnExists`-style helpers already present in the sibling tests; if none exists, query `information_schema.columns`.)

- [ ] **Step 4: Run the test + frozen check**

Run: `go test ./internal/db/migrations/... -run TestMigration0015 -v && bash scripts/check-migrations-frozen.sh`
Expected: PASS; frozen check green.

- [ ] **Step 5: Commit**

```bash
git add internal/db/migrations/0015_possession_audits.sql internal/db/migrations/MANIFEST.sha256 internal/db/migrations/possession_state_test.go
git commit -m "feat(p2-m6): migration 0015 — pin_audits receive/decide/transcript + nodes trust epoch (P2-M6)"
```

---

## Task 2: Donor `BlockGetLocal` primitive

**Files:**
- Create: `internal/node/ipfsclient/blockget.go`
- Test: `internal/node/ipfsclient/blockget_test.go`

**Interfaces:**
- Produces: `func (c *Client) BlockGetLocal(ctx context.Context, blockCID string) ([]byte, error)`; `var ErrBlockNotLocal = errors.New(...)`.

- [ ] **Step 1: Write the failing test**

Create `internal/node/ipfsclient/blockget_test.go` with a fake Kubo HTTP server:

```go
func TestBlockGetLocalReturnsBytesAndForcesOffline(t *testing.T) {
    var gotOffline string
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path != "/api/v0/block/get" { t.Fatalf("path %s", r.URL.Path) }
        gotOffline = r.URL.Query().Get("offline")
        w.Write([]byte("BLOCKBYTES"))
    }))
    defer srv.Close()
    c := New(srv.URL)
    got, err := c.BlockGetLocal(context.Background(), "bafkreiabc")
    if err != nil { t.Fatal(err) }
    if string(got) != "BLOCKBYTES" { t.Fatalf("got %q", got) }
    if gotOffline != "true" { t.Fatalf("offline=%q, want true (no Bitswap)", gotOffline) }
}

func TestBlockGetLocalNotPresent(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(http.StatusInternalServerError) // Kubo returns 500 for a missing block
    }))
    defer srv.Close()
    if _, err := New(srv.URL).BlockGetLocal(context.Background(), "bafkreimissing"); !errors.Is(err, ErrBlockNotLocal) {
        t.Fatalf("want ErrBlockNotLocal, got %v", err)
    }
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/node/ipfsclient/ -run TestBlockGetLocal -v`
Expected: FAIL (undefined: BlockGetLocal / ErrBlockNotLocal).

- [ ] **Step 3: Implement**

Create `internal/node/ipfsclient/blockget.go`:

```go
package ipfsclient

import (
    "context"
    "errors"
    "io"
    "net/http"
    "net/url"
)

// ErrBlockNotLocal signals the local Kubo blockstore does not hold the block.
// The audit handler maps this to a clean 404 (the lying-donor indication).
var ErrBlockNotLocal = errors.New("ipfsclient: block not present locally")

// BlockGetLocal returns the raw bytes of a single block from the LOCAL Kubo
// blockstore ONLY. It passes offline=true so Kubo never triggers a Bitswap
// network fetch (D-M6-4a); a missing block yields ErrBlockNotLocal, never a
// remote read. Used by the possession-audit responder.
func (c *Client) BlockGetLocal(ctx context.Context, blockCID string) ([]byte, error) {
    q := url.Values{"arg": {blockCID}, "offline": {"true"}}
    resp, err := c.post(ctx, "/api/v0/block/get", q, nil, "")
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        io.Copy(io.Discard, resp.Body)
        return nil, ErrBlockNotLocal
    }
    return io.ReadAll(resp.Body)
}
```

> **Implementation note (caution 1):** confirm against the pinned Kubo version that `block/get?offline=true` cannot reach peers; if a Kubo build ignores the per-request flag, fall back to the daemon's offline posture documented in `KUBO_HARDENING.md`. The no-network behavior is asserted end-to-end in Task 13.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/node/ipfsclient/ -run TestBlockGetLocal -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/node/ipfsclient/blockget.go internal/node/ipfsclient/blockget_test.go
git commit -m "feat(p2-m6): donor BlockGetLocal — local-only raw block read, no Bitswap (P2-M6)"
```

---

## Task 3: wire — audit challenge message + transcript-hash helper

**Files:**
- Create: `internal/federation/wire/audit.go`
- Test: `internal/federation/wire/audit_test.go`

**Interfaces:**
- Produces: `type AuditChallenge struct{...}`; `func AuditTranscriptHash(c AuditChallenge, blockBytes []byte) []byte`.

- [ ] **Step 1: Write the failing test (golden vector + length-prefix unambiguity)**

Create `internal/federation/wire/audit_test.go`:

```go
func TestAuditTranscriptHashLengthPrefixed(t *testing.T) {
    // Length-prefixing must make ("ab","c") != ("a","bc") even though raw concat is equal.
    base := AuditChallenge{ChallengeID: "id", BlobCID: "blob", BlockCID: "blk", BlockIndex: 3, Nonce: "n"}
    h1 := AuditTranscriptHash(AuditChallenge{ChallengeID: "ab", BlobCID: base.BlobCID, BlockCID: base.BlockCID, Nonce: base.Nonce}, []byte("x"))
    h2 := AuditTranscriptHash(AuditChallenge{ChallengeID: "a", BlobCID: "b" + base.BlobCID, BlockCID: base.BlockCID, Nonce: base.Nonce}, []byte("x"))
    if bytes.Equal(h1, h2) { t.Fatal("length-prefixing failed: ambiguous concat") }
    if len(AuditTranscriptHash(base, []byte("x"))) != 32 { t.Fatal("want sha256 length") }
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/federation/wire/ -run TestAuditTranscript -v`
Expected: FAIL (undefined).

- [ ] **Step 3: Implement**

Create `internal/federation/wire/audit.go`:

```go
package wire

import (
    "crypto/sha256"
    "encoding/binary"
)

// AuditChallenge is the coordinator→donor possession-audit request body
// (challenge_kind == "block_hash"). assignment_id/generation make it
// assignment-bound (D-M6-4-BIND); block_size lets the donor length-check.
type AuditChallenge struct {
    ChallengeID  string `json:"challenge_id"`
    ChallengeKind string `json:"challenge_kind"`
    BlobCID      string `json:"blob_cid"`
    AssignmentID string `json:"assignment_id"`
    Generation   int64  `json:"generation"`
    BlockIndex   int64  `json:"block_index"`
    BlockCID     string `json:"block_cid"`
    BlockSize    int64  `json:"block_size"`
    Nonce        string `json:"nonce"`
}

// AuditChallengeKindBlockHash is the only kind shipped in M6.
const AuditChallengeKindBlockHash = "block_hash"

const auditTranscriptDomain = "NOVA-POSSESSION-AUDIT-v1"

// AuditTranscriptHash is the domain-separated, length-prefixed audit transcript
// digest (D-M6-3a). CID reconstruction is the primary verifier; this digest is
// the durable transcript / test-vector artifact stored in pin_audits.transcript_hash.
func AuditTranscriptHash(c AuditChallenge, blockBytes []byte) []byte {
    h := sha256.New()
    h.Write([]byte(auditTranscriptDomain))
    h.Write([]byte{0x00})
    lp := func(b []byte) {
        var n [4]byte
        binary.BigEndian.PutUint32(n[:], uint32(len(b)))
        h.Write(n[:])
        h.Write(b)
    }
    be64 := func(v int64) { var b [8]byte; binary.BigEndian.PutUint64(b[:], uint64(v)); h.Write(b[:]) }
    lp([]byte(c.ChallengeID))
    lp([]byte(c.BlobCID))
    lp([]byte(c.AssignmentID))
    be64(c.Generation)
    lp([]byte(c.BlockCID))
    be64(c.BlockIndex)
    be64(c.BlockSize)
    lp([]byte(c.Nonce))
    lp(blockBytes)
    return h.Sum(nil)
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/federation/wire/ -run TestAuditTranscript -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/federation/wire/audit.go internal/federation/wire/audit_test.go
git commit -m "feat(p2-m6): wire AuditChallenge + domain-separated transcript hash (P2-M6)"
```

---

## Task 4: Donor audit endpoint

**Files:**
- Create: `internal/node/source/audit.go`
- Modify: `internal/node/source/server.go` (extend `Deps`/`Server`, register route)
- Test: `internal/node/source/audit_test.go`

**Interfaces:**
- Consumes: `wire.AuditChallenge`, `state.Progress`, `ipfsclient.BlockGetLocal` (via `AuditBlockReader`), `bandwidth.Bucket` (via `Budget`).
- Produces: route `POST /fed/v1/audit/challenge`; `Deps.AuditBlocks AuditBlockReader`, `Deps.AuditBudget Budget`.

- [ ] **Step 1: Extend `Deps`/`Server` in `server.go`**

In `internal/node/source/server.go` add the interface + deps + route (no behavior change to `handleBlob`):

```go
// AuditBlockReader reads a single block from LOCAL storage only (no Bitswap).
// Satisfied by *ipfsclient.Client (BlockGetLocal).
type AuditBlockReader interface {
    BlockGetLocal(ctx context.Context, blockCID string) ([]byte, error)
}
```

Add to `Deps`: `AuditBlocks AuditBlockReader`, `AuditBudget Budget`, and `MaxAuditBlockBytes int64` (default 262144 in `NewServer` when ≤ 0).
Add to `Server`: `auditBlocks AuditBlockReader`, `auditBudget Budget`, `maxAuditBlockBytes int64`; set them in `NewServer`; and register:

```go
s.mux.HandleFunc("POST /fed/v1/audit/challenge", s.handleAuditChallenge)
```

- [ ] **Step 2: Write the failing test**

Create `internal/node/source/audit_test.go` (mirror `server_test.go`'s fakes: a fake `Pinner`, a fake `ProgressLookup`, a fake `AuditBlockReader`, an always-true/false `Budget`, and a TLS request helper that sets a coordinator/node peer cert):

```go
func TestAuditChallengePassReturnsBlockBytes(t *testing.T) {
    s := newAuditTestServer(t, auditFakes{
        progress: map[string]state.Progress{"blob": {State: state.ProgressAckDelivered, AssignmentID: "a1", Generation: 7, ByteSize: 100}},
        pinned:   map[string]bool{"blob": true},
        blocks:   map[string][]byte{"blk": bytes.Repeat([]byte{0xAB}, 64)},
        budgetOK: true,
    })
    body := wire.AuditChallenge{ChallengeID: "c1", ChallengeKind: "block_hash", BlobCID: "blob",
        AssignmentID: "a1", Generation: 7, BlockIndex: 0, BlockCID: "blk", BlockSize: 64, Nonce: "n"}
    rec := doAudit(t, s, coordinatorPeer(t), body)
    if rec.Code != 200 { t.Fatalf("code %d", rec.Code) }
    if !bytes.Equal(rec.Body.Bytes(), bytes.Repeat([]byte{0xAB}, 64)) { t.Fatal("wrong bytes") }
}

func TestAuditChallengeRejectsRoleNode(t *testing.T) {
    s := newAuditTestServer(t, auditFakes{})
    rec := doAudit(t, s, nodePeer(t), wire.AuditChallenge{BlobCID: "blob", BlockCID: "blk"})
    if rec.Code != http.StatusForbidden { t.Fatalf("RoleNode must be 403, got %d", rec.Code) }
}

func TestAuditChallenge404WhenBlockNotLocal(t *testing.T) {
    s := newAuditTestServer(t, auditFakes{
        progress: map[string]state.Progress{"blob": {State: state.ProgressAckDelivered, AssignmentID: "a1", Generation: 7, ByteSize: 100}},
        pinned:   map[string]bool{"blob": true},
        blocks:   map[string][]byte{}, // BlockGetLocal -> ErrBlockNotLocal
        budgetOK: true,
    })
    body := wire.AuditChallenge{ChallengeID: "c", ChallengeKind: "block_hash", BlobCID: "blob", AssignmentID: "a1", Generation: 7, BlockCID: "blk", BlockSize: 64, Nonce: "n"}
    if doAudit(t, s, coordinatorPeer(t), body).Code != 404 { t.Fatal("missing block must be 404") }
}

func TestAuditChallengeAssignmentMismatchFails(t *testing.T) {
    s := newAuditTestServer(t, auditFakes{
        progress: map[string]state.Progress{"blob": {State: state.ProgressAckDelivered, AssignmentID: "OTHER", Generation: 7, ByteSize: 100}},
        pinned:   map[string]bool{"blob": true}, blocks: map[string][]byte{"blk": {1}}, budgetOK: true,
    })
    body := wire.AuditChallenge{ChallengeID: "c", ChallengeKind: "block_hash", BlobCID: "blob", AssignmentID: "a1", Generation: 7, BlockCID: "blk", BlockSize: 1, Nonce: "n"}
    if doAudit(t, s, coordinatorPeer(t), body).Code != 404 { t.Fatal("assignment mismatch must fail (404)") }
}

func TestAuditChallengeBudgetExhaustedSignalsSkip(t *testing.T) {
    s := newAuditTestServer(t, auditFakes{
        progress: map[string]state.Progress{"blob": {State: state.ProgressAckDelivered, AssignmentID: "a1", Generation: 7, ByteSize: 100}},
        pinned:   map[string]bool{"blob": true}, blocks: map[string][]byte{"blk": {1}}, budgetOK: false,
    })
    body := wire.AuditChallenge{ChallengeID: "c", ChallengeKind: "block_hash", BlobCID: "blob", AssignmentID: "a1", Generation: 7, BlockCID: "blk", BlockSize: 1, Nonce: "n"}
    if doAudit(t, s, coordinatorPeer(t), body).Code != http.StatusTooManyRequests { t.Fatal("budget exhausted must be 429 (->skip)") }
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/node/source/ -run TestAuditChallenge -v`
Expected: FAIL (handler undefined).

- [ ] **Step 4: Implement the handler**

Create `internal/node/source/audit.go`:

```go
package source

import (
    "encoding/json"
    "io"
    "log/slog"
    "net/http"

    "github.com/nova-archive/nova/internal/federation/transport"
    "github.com/nova-archive/nova/internal/federation/wire"
    "github.com/nova-archive/nova/internal/node/state"
)

const maxAuditBody = 4 << 10 // challenge JSON is ~300 bytes

// handleAuditChallenge serves a synchronous block_hash possession challenge.
// Stricter than handleBlob: COORDINATOR ONLY, NO repair token; assignment-bound;
// returns the challenged block's bytes (the coordinator verifies by CID
// reconstruction). 404 is the clean "I do not hold it / stale assignment" signal;
// 429 means the audit governor is exhausted (the coordinator records a skip).
func (s *Server) handleAuditChallenge(w http.ResponseWriter, r *http.Request) {
    const codeUnauthorized, codeBlobUnavail, codeBudget, codeBad = "audit_unauthorized", "blob_unavailable", "budget_exceeded", "bad_request"
    now := s.now()

    // 1) Coordinator ONLY (audits are control traffic; reject RoleNode).
    if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
        s.refuse(w, http.StatusForbidden, codeUnauthorized, "no_peer_cert", "")
        return
    }
    peer, err := transport.IdentityFromCert(r.TLS.PeerCertificates[0])
    if err != nil || peer.Role != transport.RoleCoordinator {
        s.refuse(w, http.StatusForbidden, codeUnauthorized, "wrong_role", "")
        return
    }

    // 2) Parse the bounded challenge body.
    var ch wire.AuditChallenge
    if err := json.NewDecoder(io.LimitReader(r.Body, maxAuditBody)).Decode(&ch); err != nil ||
        ch.ChallengeKind != wire.AuditChallengeKindBlockHash || ch.BlobCID == "" || ch.BlockCID == "" {
        s.refuse(w, http.StatusBadRequest, codeBad, "bad_challenge", ch.BlobCID)
        return
    }
    // 2a) Local block-size ceiling (defense against a buggy/malicious coordinator
    // requesting a huge read): reject BEFORE any budget debit or block read.
    if ch.BlockSize <= 0 || ch.BlockSize > s.maxAuditBlockBytes {
        s.refuse(w, http.StatusBadRequest, codeBad, "block_size_out_of_range", ch.BlobCID)
        return
    }

    // 3) Assignment-bound: acked-delivered + assignment/generation match (D-M6-4-BIND).
    prog, ok := s.progress.Get(ch.BlobCID)
    if !ok || prog.State != state.ProgressAckDelivered ||
        prog.AssignmentID != ch.AssignmentID || prog.Generation != ch.Generation {
        s.refuse(w, http.StatusNotFound, codeBlobUnavail, "progress_mismatch", ch.BlobCID)
        return
    }

    // 4) Recursive PIN (not stray block residue).
    has, err := s.pinner.Has(r.Context(), ch.BlobCID)
    if err != nil || !has {
        s.refuse(w, http.StatusNotFound, codeBlobUnavail, "not_pinned", ch.BlobCID)
        return
    }

    // 5) Audit governor (separate from the M5 source/repair bucket, D-M6-6).
    if !s.auditBudget.Take(ch.BlockSize, now) {
        s.refuse(w, http.StatusTooManyRequests, codeBudget, "audit_budget", ch.BlobCID)
        return
    }

    // 6) Local-only block read; ANY error (incl. not-present) is a clean 404. We do
    // not import the concrete ipfsclient here — the package stays interface-only.
    block, err := s.auditBlocks.BlockGetLocal(r.Context(), ch.BlockCID)
    if err != nil {
        s.refuse(w, http.StatusNotFound, codeBlobUnavail, "block_unavailable", ch.BlobCID)
        return
    }
    if int64(len(block)) != ch.BlockSize {
        s.refuse(w, http.StatusNotFound, codeBlobUnavail, "block_size_mismatch", ch.BlobCID)
        return
    }

    w.Header().Set("Content-Type", "application/octet-stream")
    w.WriteHeader(http.StatusOK)
    _, _ = w.Write(block)
    slog.Info("node.audit.served", "blob", ch.BlobCID, "block", ch.BlockCID, "bytes", len(block))
}
```

> The handler imports no concrete `ipfsclient` — `source` stays interface-only (cleaner boundary). Any `BlockGetLocal` error (including not-present) maps to a single `404 block_unavailable`.

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./internal/node/source/ -run TestAuditChallenge -v`
Expected: PASS.

- [ ] **Step 6: Verify the donor boundary still holds**

Run: `bash scripts/check_node_deps.sh`
Expected: PASS (no new non-stdlib dep outside the allowlist).

- [ ] **Step 7: Commit**

```bash
git add internal/node/source/server.go internal/node/source/audit.go internal/node/source/audit_test.go
git commit -m "feat(p2-m6): donor POST /fed/v1/audit/challenge — assignment-bound, coordinator-only (P2-M6)"
```

---

## Task 5: Donor wiring — advertise capability + audit budget

**Files:**
- Modify: `internal/node/agent/*.go` (capability advertisement)
- Modify: `cmd/node/main.go` (construct the audit-budget bucket + pass `AuditBlocks`/`AuditBudget` to `source.NewServer`)
- Test: `internal/node/agent/*_test.go`

**Interfaces:**
- Consumes: `wire.CapAuditBlockHash` (already declared), `bandwidth.Bucket`, `*ipfsclient.Client`.

- [ ] **Step 1: Write the failing test (capability advertised)**

In the agent's capability test, assert the advertised set now contains `wire.CapAuditBlockHash`:

```go
func TestAgentAdvertisesAuditCapability(t *testing.T) {
    caps := advertisedCapabilities() // the function/literal the agent uses for register/heartbeat
    if !slices.Contains(caps, wire.CapAuditBlockHash) {
        t.Fatalf("agent must advertise %s; got %v", wire.CapAuditBlockHash, caps)
    }
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/node/agent/ -run TestAgentAdvertisesAuditCapability -v`
Expected: FAIL.

- [ ] **Step 3: Add the capability**

Add `wire.CapAuditBlockHash` to the donor's advertised capability slice (where `CapReadSource`/`CapRepairStream` are listed in the agent).

- [ ] **Step 4: Wire the audit budget + block reader in `cmd/node/main.go`**

Where `source.NewServer(source.Deps{...})` is constructed, add a **separate** audit-budget bucket sized at `audit_budget_fraction × daily_budget` and the block reader:

```go
// Separate from the D11 source/repair bucket. Constructor is NewDailyBucket(bytesPerDay, now).
auditBucket := bandwidth.NewDailyBucket(int64(float64(dailyBudgetBytes)*auditBudgetFraction), time.Now())
srv := source.NewServer(source.Deps{
    // ... existing fields ...
    AuditBlocks:        ipfsClient,
    AuditBudget:        auditBucket,
    MaxAuditBlockBytes: 262144,
})
```

(`auditBudgetFraction` comes from donor config, default 0.01; `dailyBudgetBytes` is the same figure the D11 bucket is sized from.)

- [ ] **Step 5: Run + boundary check**

Run: `go test ./internal/node/... && go build ./cmd/node/... && bash scripts/check_node_deps.sh`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/node/agent cmd/node
git commit -m "feat(p2-m6): donor advertises audit-block-hash/v1 + separate audit-budget governor (P2-M6)"
```

---

## Task 6: sqlc queries for possession audits

**Files:**
- Create: `internal/db/queries/possession.sql`
- Modify: `internal/db/gen/*` (via `make sqlc-generate` — do not hand-edit)
- Test: covered by Tasks 8–9 (DB-backed); this task's deliverable is "sqlc generates and compiles".

**Interfaces:**
- Produces (generated method names): `InsertAuditChallenge`, `RevalidateAuditPin`, `FailAckedPinAssignmentForAudit`, `GetNodeTrustForUpdate`, `SelectLastAuditPerNode`, `RecordAuditOutcome`, `MoveReputation`, `GetNodeTrust`, `CountPassedAuditsSince`, `CountAckedTransfersSince`, `SetTrustState`, `SetTrustReview`, `ClearTrustReview`, `ReconcileStaleAudits`, `SelectDueAuditNodes`, `SelectAckedPinForAudit`, `SelectNewlyAckedPins`, `SelectRandomBlockForCID`, `GetNodeSourceAddr`.

- [ ] **Step 1: Write the queries**

Create `internal/db/queries/possession.sql` (match the annotation style of `replication.sql`):

```sql
-- name: InsertAuditChallenge :exec
-- D-M6-3b: insert BEFORE dispatch with result NULL so a crash mid-flight is recoverable.
INSERT INTO pin_audits (id, blob_cid, node_id, challenge_kind, nonce, deadline, challenged_at)
VALUES ($1, $2, $3, $4, $5, $6, now());

-- name: RevalidateAuditPin :one
-- D-M6-7a: confirm the audited assignment is still the live acked one before scoring.
SELECT EXISTS (
  SELECT 1 FROM pin_assignments
  WHERE cid = $1 AND node_id = $2 AND state = 'acked'
    AND assignment_id = $3 AND generation = $4
) AS still_current;

-- name: FailAckedPinAssignmentForAudit :execrows
-- D-M6-7 hard-fail: invalidate the ACKED row for this exact assignment/generation.
-- (M5's FailPinAssignment only fails 'pending' rows — wrong state for audits.)
UPDATE pin_assignments
SET state = 'failed'
WHERE cid = $1 AND node_id = $2 AND assignment_id = $3 AND generation = $4 AND state = 'acked';

-- name: GetNodeTrustForUpdate :one
-- Row-locked read for the reputation/trust transaction (Blocker 5: lost-update safe).
SELECT trust_state, reputation_score, trust_epoch_started_at, trust_review_required_at, joined_at
FROM nodes WHERE id = $1 FOR UPDATE;

-- name: SelectLastAuditPerNode :many
-- Startup cadence seed (D-M6-5): last resolved audit per node, so a restart does not
-- immediately re-audit every due node.
SELECT node_id, max(decided_at)::timestamptz AS last_decided_at
FROM pin_audits WHERE result IS NOT NULL
GROUP BY node_id;

-- name: RecordAuditOutcome :execrows
-- Resolve only the unresolved row, so a replayed challenge_id cannot overwrite a
-- decided audit (the caller asserts 1 row; 0 = replay/already-decided).
UPDATE pin_audits
SET result = $2, received_at = $3, decided_at = $4, latency_ms = $5,
    bytes_verified = $6, transcript_hash = $7, error = $8
WHERE id = $1 AND result IS NULL;

-- name: MoveReputation :one
-- Atomic, lost-update-safe (D-M6-7): clamp to [0,1].
UPDATE nodes
SET reputation_score = LEAST(1.0, GREATEST(0.0, $2::real))
WHERE id = $1
RETURNING reputation_score;

-- name: GetNodeTrust :one
SELECT trust_state, reputation_score, trust_epoch_started_at, trust_review_required_at, joined_at
FROM nodes WHERE id = $1;

-- name: CountPassedAuditsSince :one
SELECT count(*) FROM pin_audits
WHERE node_id = $1 AND result = 'pass' AND decided_at >= $2;

-- name: CountAckedTransfersSince :one
SELECT count(*) FROM pin_assignments
WHERE node_id = $1 AND state = 'acked' AND acked_at >= $2;

-- name: SetTrustState :exec
UPDATE nodes SET trust_state = $2 WHERE id = $1;

-- name: SetTrustReview :exec
-- Hash-mismatch path: reset epoch + mark for operator review (D-M6-2b).
UPDATE nodes
SET trust_epoch_started_at = now(), trust_review_required_at = now(), trust_review_reason = $2
WHERE id = $1;

-- name: ClearTrustReview :exec
-- Operator clear-review: restart the epoch, drop the marker (D-M6-8).
UPDATE nodes
SET trust_review_required_at = NULL, trust_review_reason = NULL, trust_epoch_started_at = now()
WHERE id = $1;

-- name: ReconcileStaleAudits :exec
-- Startup: crashed-mid-flight attempts -> skip (D-M6-3b step 4).
UPDATE pin_audits
SET result = 'skip', decided_at = now(), error = 'coordinator_crash_or_timeout'
WHERE result IS NULL AND deadline < now() - make_interval(secs => $1::float);

-- name: SelectDueAuditNodes :many
-- Stage 1 (D-M6-5a): live, current-synced nodes with at least one acked pin, ordered
-- by node-level pressure (stored bytes proxy + acked pin count) and audit staleness.
SELECT n.id AS node_id, n.trust_state, n.reputation_score,
       COALESCE(n.last_stored_bytes, 0) AS stored_bytes,
       count(pa.cid) AS acked_pins
FROM nodes n
JOIN pin_assignments pa ON pa.node_id = n.id AND pa.state = 'acked'
WHERE n.status IN ('active','suspect') AND n.assignment_sync_state = 'current'
  AND n.advertised_capabilities @> ARRAY['audit-block-hash/v1']  -- only challengeable donors
  AND n.source_nebula_addr IS NOT NULL AND n.source_nebula_addr <> ''
GROUP BY n.id
ORDER BY (COALESCE(n.last_stored_bytes,0)::float8 * count(pa.cid)) DESC, n.id
LIMIT $1;

-- name: SelectAckedPinForAudit :one
-- Stage 2: one acked pin for a node, WEIGHTED-random by envelope_size
-- (Efraimidis–Spirakis: key = random()^(1/weight), largest key wins) so big
-- custodians are sampled proportionally without ORDER BY random() over the corpus.
SELECT pa.cid, pa.assignment_id, pa.generation
FROM pin_assignments pa
JOIN blob_manifests m ON m.cid = pa.cid
WHERE pa.node_id = $1 AND pa.state = 'acked'
ORDER BY power(random(), 1.0 / GREATEST(m.envelope_size, 1)) DESC
LIMIT 1;

-- name: SelectNewlyAckedPins :many
-- Fast lane (D-M6-5b): pins acked within the window, not yet audited, bounded quota.
SELECT pa.node_id, pa.cid, pa.assignment_id, pa.generation
FROM pin_assignments pa
WHERE pa.state = 'acked' AND pa.acked_at >= $1
  AND NOT EXISTS (SELECT 1 FROM pin_audits a WHERE a.blob_cid = pa.cid AND a.node_id = pa.node_id)
ORDER BY pa.acked_at
LIMIT $2;

-- name: SelectRandomBlockForCID :one
-- Stage 3: a block to challenge (size <= max_block_bytes; never issue an over-cap block).
SELECT block_cid, block_index, block_size
FROM blob_blocks
WHERE blob_cid = $1 AND block_size <= $2
ORDER BY random()
LIMIT 1;

-- name: GetNodeSourceAddr :one
-- The donor's inbound source address to POST the challenge to.
SELECT source_nebula_addr FROM nodes WHERE id = $1;
```

- [ ] **Step 2: Regenerate sqlc**

Run: `make sqlc-generate && go build ./internal/db/...`
Expected: clean build; new methods present in `internal/db/gen/querier.go`.

- [ ] **Step 3: Commit**

```bash
git add internal/db/queries/possession.sql internal/db/gen
git commit -m "feat(p2-m6): sqlc queries for possession audits — challenge/outcome/trust/sampling (P2-M6)"
```

---

## Task 7: Coordinator audit dispatcher (mTLS client + CID verify)

**Files:**
- Create: `internal/audit/possession/dispatch.go`
- Test: `internal/audit/possession/dispatch_test.go`

**Interfaces:**
- Consumes: `wire.AuditChallenge`, a `*tls.Config` (CoordinatorClientTLS), `go-cid`.
- Produces: `type Dispatcher struct{...}`; `func (d *Dispatcher) Challenge(ctx, addr string, ch wire.AuditChallenge) (DispatchResult, error)`; `type DispatchResult struct { Outcome Outcome; Bytes []byte; ReceivedAt time.Time; LatencyMS int }`; `type Outcome int` with `OutcomePass, OutcomeFailNotPresent, OutcomeFailMismatch, OutcomeFailDeadline, OutcomeSkipBudget, OutcomeSkipUnreachable`.

- [ ] **Step 1: Write the failing test (against a fake donor)**

Create `internal/audit/possession/dispatch_test.go` using `go-cid` to build a real block + its CID, then a fake donor HTTP server returning those bytes; assert `OutcomePass`; another returning wrong bytes → `OutcomeFailMismatch`; a 404 → `OutcomeFailNotPresent`; a 429 → `OutcomeSkipBudget`.

```go
func TestDispatchVerifiesByCIDReconstruction(t *testing.T) {
    raw := []byte("hello-block")
    blkCID := rawLeafCID(t, raw) // cid.V1Builder{Codec: cid.Raw, MhType: mh.SHA2_256}.Sum(raw)
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(raw) }))
    defer srv.Close()
    d := NewDispatcher(plainClientForTest(srv)) // test injects an *http.Client
    res, err := d.Challenge(context.Background(), srv.URL, wire.AuditChallenge{BlockCID: blkCID, BlockSize: int64(len(raw))})
    if err != nil { t.Fatal(err) }
    if res.Outcome != OutcomePass { t.Fatalf("want pass, got %v", res.Outcome) }
}

func TestDispatchWrongBytesIsMismatch(t *testing.T) { /* server returns []byte("tampered"); expect OutcomeFailMismatch */ }
func TestDispatch404IsNotPresent(t *testing.T)      { /* 404 -> OutcomeFailNotPresent */ }
func TestDispatch429IsSkipBudget(t *testing.T)      { /* 429 -> OutcomeSkipBudget */ }
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/audit/possession/ -run TestDispatch -v`
Expected: FAIL (undefined).

- [ ] **Step 3: Implement**

Create `internal/audit/possession/dispatch.go`:

```go
package possession

import (
    "bytes"
    "context"
    "crypto/tls"
    "encoding/json"
    "errors"
    "io"
    "net/http"
    "time"

    "github.com/ipfs/go-cid"
    "github.com/nova-archive/nova/internal/federation/wire"
)

type Outcome int

const (
    OutcomePass Outcome = iota
    OutcomeFailNotPresent
    OutcomeFailMismatch
    OutcomeFailDeadline
    OutcomeSkipBudget
    OutcomeSkipUnreachable // pre-dispatch connection/TLS failure: donor may never have been challenged
)

type DispatchResult struct {
    Outcome    Outcome
    Bytes      []byte
    ReceivedAt time.Time
    LatencyMS  int
}

// Dispatcher POSTs a synchronous audit challenge to a donor's inbound source
// server over coordinator-identity mTLS and verifies the returned block bytes by
// reconstructing the CID from the stored prefix (D-M6-3). No repair token.
type Dispatcher struct {
    hc  *http.Client
    now func() time.Time
}

func NewDispatcher(clientTLS *tls.Config) *Dispatcher {
    return &Dispatcher{hc: &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}}, now: time.Now}
}

const maxAuditResp = 1 << 20 // 1 MiB ceiling on any returned block (importspec leaves <= 256 KiB)

func (d *Dispatcher) Challenge(ctx context.Context, addr string, ch wire.AuditChallenge) (DispatchResult, error) {
    if ch.BlockSize <= 0 || ch.BlockSize > maxAuditResp { // sanity ceiling; scheduler also filters over-cap blocks
        return DispatchResult{Outcome: OutcomeFailMismatch}, nil
    }
    ch.ChallengeKind = wire.AuditChallengeKindBlockHash
    body, _ := json.Marshal(ch)
    req, err := http.NewRequestWithContext(ctx, http.MethodPost, addr+"/fed/v1/audit/challenge", bytes.NewReader(body))
    if err != nil {
        return DispatchResult{}, err
    }
    req.Header.Set("Content-Type", "application/json")
    start := d.now()
    resp, err := d.hc.Do(req)
    if err != nil {
        // Distinguish "donor was challenged but missed the deadline" (fail) from
        // "could not reach the donor" (skip, no reputation movement): a deadline
        // exceedance after the request began is a fail; a dial/TLS/no-route error
        // before any response is unreachable.
        if errors.Is(err, context.DeadlineExceeded) {
            return DispatchResult{Outcome: OutcomeFailDeadline}, nil
        }
        return DispatchResult{Outcome: OutcomeSkipUnreachable}, nil
    }
    defer resp.Body.Close()
    switch resp.StatusCode {
    case http.StatusTooManyRequests:
        return DispatchResult{Outcome: OutcomeSkipBudget, ReceivedAt: d.now()}, nil
    case http.StatusNotFound:
        return DispatchResult{Outcome: OutcomeFailNotPresent, ReceivedAt: d.now()}, nil
    case http.StatusOK:
        // fall through
    default:
        return DispatchResult{Outcome: OutcomeFailNotPresent, ReceivedAt: d.now()}, nil
    }
    // Read EXACTLY the expected body (+1 to detect over-length), THEN stamp
    // received_at — so a slow-body donor is judged against the deadline after the
    // full read (D-M6-15 #4 late-body), and received_at reflects true completion.
    raw, err := io.ReadAll(io.LimitReader(resp.Body, ch.BlockSize+1))
    received := d.now()
    if err != nil {
        return DispatchResult{Outcome: OutcomeFailDeadline, ReceivedAt: received}, nil
    }
    if int64(len(raw)) != ch.BlockSize {
        return DispatchResult{Outcome: OutcomeFailMismatch, ReceivedAt: received}, nil
    }
    // Primary verifier: reconstruct the CID from the stored prefix and compare.
    stored, err := cid.Decode(ch.BlockCID)
    if err != nil {
        return DispatchResult{Outcome: OutcomeFailMismatch, ReceivedAt: received}, nil
    }
    recomputed, err := stored.Prefix().Sum(raw)
    if err != nil || !recomputed.Equals(stored) {
        return DispatchResult{Outcome: OutcomeFailMismatch, ReceivedAt: received}, nil
    }
    return DispatchResult{Outcome: OutcomePass, Bytes: raw, ReceivedAt: received,
        LatencyMS: int(received.Sub(start).Milliseconds())}, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/audit/possession/ -run TestDispatch -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/audit/possession/dispatch.go internal/audit/possession/dispatch_test.go
git commit -m "feat(p2-m6): coordinator audit dispatcher — mTLS challenge + CID-reconstruction verify (P2-M6)"
```

---

## Task 8: Outcome recording + reputation/trust/durability transaction

**Files:**
- Create: `internal/audit/possession/verify.go`, `internal/audit/possession/trust.go`
- Test: `internal/audit/possession/verify_test.go`, `internal/audit/possession/trust_test.go` (DB-backed)

**Interfaces:**
- Consumes: Task 6 queries (incl. `FailAckedPinAssignmentForAudit`, `GetNodeTrustForUpdate`); `DispatchResult` (Task 7); `notify.Notifier`; existing `gen.EnqueueReconcile`.
- Produces: `func (a *Auditor) Record(ctx, challenge AuditTarget, res DispatchResult) error`; `type AuditTarget struct { AuditID, NodeID, BlobCID, BlockCID, AssignmentID string; Generation int64; Nonce string; Deadline time.Time }`; `applyTrust(ctx, tx, nodeID, cfg)`.

- [ ] **Step 1: Write the failing tests (one per outcome, DB-backed)**

Create `internal/audit/possession/verify_test.go` using the repo's DB test harness (a fixture node + acked pin):

```go
func TestRecordPassDriftsReputationUp(t *testing.T)          { /* score 0.90 -> 0.91, pin stays acked, result=pass, received_at set */ }
func TestRecordDeadlineSoftFailKeepsPin(t *testing.T)        { /* score *=0.95, pin still acked, no reconcile enqueue */ }
func TestRecordNotPresentHardFailsPin(t *testing.T)          { /* score*=0.5, pin state='failed', reconcile enqueued (audit_not_present) */ }
func TestRecordMismatchZeroesAndSuspects(t *testing.T)       { /* score=0, pin failed, reconcile (audit_mismatch), trust_review set, federation.node_suspect emitted */ }
func TestRecordStaleChallengeSkips(t *testing.T)             { /* RevalidateAuditPin=false -> result=skip(stale_challenge), no rep move, pin untouched */ }
func TestRecordBelowFloorDoesNotBulkReplace(t *testing.T)    { /* a soft fail crossing 0.5: trusted->probationary demote, but NO reconcile enqueue for the node's other still-acked CIDs (narrowed, D-M6-7) */ }
func TestRecordUnreachableIsSkipNoMovement(t *testing.T)     { /* OutcomeSkipUnreachable -> result=skip(unreachable), reputation unchanged, pin acked */ }
func TestRecordOutcomeReplayDoesNotOverwrite(t *testing.T)   { /* RecordAuditOutcome WHERE result IS NULL: second call returns 0 rows, original outcome preserved */ }
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/audit/possession/ -run TestRecord -v`
Expected: FAIL (undefined `Record`).

- [ ] **Step 3: Implement `verify.go`**

```go
package possession

import (
    "context"
    "time"

    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/nova-archive/nova/internal/db/gen"
    "github.com/nova-archive/nova/internal/federation/wire"
    "github.com/nova-archive/nova/internal/notify"
)

type AuditTarget struct {
    AuditID, NodeID, BlobCID, BlockCID, AssignmentID, Nonce string
    Generation, BlockIndex, BlockSize int64
    Deadline                          time.Time
}

type Auditor struct {
    pool     *pgxpool.Pool
    notifier notify.Notifier
    trust    TrustConfig
}

func NewAuditor(pool *pgxpool.Pool, n notify.Notifier, tc TrustConfig) *Auditor {
    if n == nil { n = notify.NoopNotifier{} }
    return &Auditor{pool: pool, notifier: n, trust: tc}
}

// Record applies the audit outcome in one transaction: revalidate the pin, record
// pin_audits, move reputation (atomic), correct durability on a hard fail, and run
// the trust state machine. A post-commit federation.node_suspect fires on mismatch.
func (a *Auditor) Record(ctx context.Context, t AuditTarget, res DispatchResult, reputationFloor float64) error {
    var suspect bool
    err := pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
        q := gen.New(tx)

        // D-M6-7a: stale-challenge guard.
        still, err := q.RevalidateAuditPin(ctx, gen.RevalidateAuditPinParams{
            Cid: t.BlobCID, NodeID: pgNodeID(t.NodeID), AssignmentID: pgUUID(t.AssignmentID), Generation: t.Generation})
        if err != nil { return err }
        decided := time.Now()
        if !still {
            return q.RecordAuditOutcome(ctx, recordParams(t, "skip", nil, decided, res, "stale_challenge"))
        }

        result, repMul, repZero, hard, mismatch, errReason := classify(res.Outcome)
        var transcript []byte
        if res.Outcome == OutcomePass {
            transcript = wire.AuditTranscriptHash(wire.AuditChallenge{
                ChallengeID: t.AuditID, BlobCID: t.BlobCID, AssignmentID: t.AssignmentID, Generation: t.Generation,
                BlockCID: t.BlockCID, BlockIndex: t.BlockIndex, BlockSize: t.BlockSize, Nonce: t.Nonce}, res.Bytes)
        }
        // RecordAuditOutcome updates only the unresolved (result IS NULL) row, so a
        // replayed challenge_id cannot silently overwrite a decided audit.
        if _, err := q.RecordAuditOutcome(ctx, recordParams(t, result, transcript, decided, res, errReason)); err != nil {
            return err
        }
        if result == "skip" { return nil } // budget/unreachable skip: no movement

        // Row-lock the node so the read-compute-write reputation update cannot lose a
        // concurrent update (Blocker 5).
        cur, err := q.GetNodeTrustForUpdate(ctx, pgNodeID(t.NodeID))
        if err != nil { return err }
        newScore := cur.ReputationScore
        if repZero { newScore = 0 } else if repMul > 0 { newScore = cur.ReputationScore * float32(repMul) } else { newScore = minF32(1, cur.ReputationScore+0.01) }
        if _, err := q.MoveReputation(ctx, gen.MoveReputationParams{ID: pgNodeID(t.NodeID), Column2: newScore}); err != nil {
            return err
        }
        slog.Info("audit.reputation.moved", "node", t.NodeID, "from", cur.ReputationScore, "to", newScore, "outcome", result)
        if hard {
            // FailAckedPinAssignmentForAudit fails ONLY the acked row for this exact
            // assignment/generation (M5's FailPinAssignment fails pending rows only).
            n, err := q.FailAckedPinAssignmentForAudit(ctx, failAckedParams(t))
            if err != nil { return err }
            if n == 1 { // only enqueue if we actually invalidated the live acked pin
                if err := q.EnqueueReconcile(ctx, gen.EnqueueReconcileParams{Cid: t.BlobCID, Reason: pgText(reconcileReason(mismatch))}); err != nil { return err }
            }
        }
        if mismatch {
            if err := q.SetTrustReview(ctx, gen.SetTrustReviewParams{ID: pgNodeID(t.NodeID), TrustReviewReason: pgText("hash_mismatch")}); err != nil { return err }
            suspect = true
        }
        // Below-floor BULK re-replication is intentionally NOT done here (deferred to
        // P2-M7, D-M6-7): below-floor excludes new placement + deprioritizes source
        // ordering, but present acked pins stay countable unless a pin-specific hard
        // failure invalidated one above.
        return a.applyTrust(ctx, q, t.NodeID, float64(newScore), reputationFloor)
    })
    if err != nil { return err }
    if suspect {
        a.notifier.Emit(ctx, notify.Event{Type: "federation.node_suspect", ScopeKey: t.NodeID,
            Payload: map[string]any{"node_id": t.NodeID, "reason": "hash_mismatch", "affected_blob_cid": t.BlobCID, "audit_id": t.AuditID}})
    }
    return nil
}

// classify maps a dispatch outcome to (db result, repMultiplier, repZero, hardFail, mismatch, errReason).
func classify(o Outcome) (result string, mul float64, zero, hard, mismatch bool, reason string) {
    switch o {
    case OutcomePass:          return "pass", 0, false, false, false, ""
    case OutcomeFailDeadline:  return "fail", 0.95, false, false, false, "deadline"
    case OutcomeFailNotPresent:return "fail", 0.5, false, true, false, "not_present"
    case OutcomeFailMismatch:  return "fail", 0, true, true, true, "mismatch"
    case OutcomeSkipBudget:    return "skip", 0, false, false, false, "audit_budget_exhausted"
    case OutcomeSkipUnreachable: return "skip", 0, false, false, false, "unreachable"
    }
    return "skip", 0, false, false, false, "unknown"
}

func reconcileReason(mismatch bool) string { if mismatch { return "audit_mismatch" }; return "audit_not_present" }
```

(Implement the small `pg*`/`recordParams`/`failParams`/`minF32` adapters in the same file to match the exact generated param structs — names like `MoveReputationParams.Column2` depend on the sqlc output; adjust to the generated field names. The point of these tests is that the field mapping is exercised against a real DB.)

- [ ] **Step 4: Implement `trust.go`**

```go
package possession

import (
    "context"
    "time"

    "github.com/nova-archive/nova/internal/db/gen"
)

// TrustConfig holds the graduation thresholds (D-M6-8), all operator-tunable.
type TrustConfig struct {
    MinAge          time.Duration // default 7d
    MinPassedAudits int64         // default 10
    MinAckedXfers   int64         // default 5
    GraduateRep     float64       // default 0.95
}

// applyTrust runs the automatic probationary<->trusted transitions. suspended is
// never set here (operator-only). Demotion is symmetric: trusted -> probationary
// below the floor.
func (a *Auditor) applyTrust(ctx context.Context, q *gen.Queries, nodeID string, score, floor float64) error {
    n, err := q.GetNodeTrust(ctx, pgNodeID(nodeID))
    if err != nil { return err }
    switch n.TrustState {
    case "trusted":
        if score < floor {
            return q.SetTrustState(ctx, gen.SetTrustStateParams{ID: pgNodeID(nodeID), TrustState: "probationary"})
        }
    case "probationary":
        if n.TrustReviewRequiredAt.Valid { return nil } // review gate (D-M6-2b)
        if time.Since(n.TrustEpochStartedAt.Time) < a.trust.MinAge { return nil }
        if score < a.trust.GraduateRep { return nil }
        passed, err := q.CountPassedAuditsSince(ctx, gen.CountPassedAuditsSinceParams{NodeID: pgNodeID(nodeID), DecidedAt: n.TrustEpochStartedAt})
        if err != nil { return err }
        xfers, err := q.CountAckedTransfersSince(ctx, gen.CountAckedTransfersSinceParams{NodeID: pgNodeID(nodeID), AckedAt: n.TrustEpochStartedAt})
        if err != nil { return err }
        if passed >= a.trust.MinPassedAudits && xfers >= a.trust.MinAckedXfers {
            return q.SetTrustState(ctx, gen.SetTrustStateParams{ID: pgNodeID(nodeID), TrustState: "trusted"})
        }
    }
    return nil
}
```

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./internal/audit/possession/ -run 'TestRecord|TestTrust' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/audit/possession/verify.go internal/audit/possession/trust.go internal/audit/possession/verify_test.go internal/audit/possession/trust_test.go
git commit -m "feat(p2-m6): audit outcome txn — reputation + durability correction + trust state machine (P2-M6)"
```

---

## Task 9: Coordinator audit scheduler

**Files:**
- Create: `internal/audit/possession/scheduler.go`
- Test: `internal/audit/possession/scheduler_test.go`

**Interfaces:**
- Consumes: Task 6 queries, `Dispatcher` (Task 7), `Auditor` (Task 8), `PossessionAuditConfig` (Task 10).
- Produces: `type Scheduler struct{...}`; `func NewScheduler(...) *Scheduler`; `func (s *Scheduler) Run(ctx context.Context)`; `func (s *Scheduler) ReconcileOnStartup(ctx context.Context) error`.

- [ ] **Step 1: Write the failing tests**

Create `internal/audit/possession/scheduler_test.go`:

```go
func TestSchedulerStartupReconcilesStaleAudits(t *testing.T) { /* insert a result IS NULL row past deadline; ReconcileOnStartup -> result=skip */ }
func TestSchedulerFastLaneHasQuota(t *testing.T)             { /* N newly-acked pins, quota=2 -> at most 2 fast-lane challenges per tick */ }
func TestSchedulerSkipsOverCapBlocks(t *testing.T)           { /* SelectRandomBlockForCID excludes block_size > max_block_bytes; no challenge issued */ }
func TestSchedulerOneTickChallengesDueNode(t *testing.T)     { /* fake dispatcher records OutcomePass; assert a pin_audits pass row written */ }
```

(Use a fake `Dispatcher` interface so the scheduler test does not need a network donor — define `type challenger interface { Challenge(...) (DispatchResult, error) }` and have `Scheduler` depend on it.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/audit/possession/ -run TestScheduler -v`
Expected: FAIL.

- [ ] **Step 3: Implement `scheduler.go`**

Mirror `internal/audit/integrity/scheduler.go` (ticker loop, `WithClock`/`WithTick` options, per-run budget). One tick:

```go
func (s *Scheduler) runOnce(ctx context.Context) {
    q := gen.New(s.pool)
    // Fast lane (bounded quota).
    fast, _ := q.SelectNewlyAckedPins(ctx, gen.SelectNewlyAckedPinsParams{AckedAt: pgTime(s.now().Add(-s.cfg.NewAckWindow)), Limit: int32(s.cfg.FastLaneQuota)})
    for _, p := range fast { s.auditOne(ctx, q, p.NodeID, p.Cid, p.AssignmentID, p.Generation) }
    // Baseline: due nodes, one size-weighted pin each.
    nodes, _ := q.SelectDueAuditNodes(ctx, int32(s.cfg.NodesPerTick))
    for _, n := range nodes {
        if !s.due(n) { continue } // cadence modulation by trust/reputation (D-M6-5b)
        pin, err := q.SelectAckedPinForAudit(ctx, n.NodeID)
        if err != nil { continue }
        s.auditOne(ctx, q, n.NodeID, pin.Cid, pin.AssignmentID, pin.Generation)
    }
}

func (s *Scheduler) auditOne(ctx context.Context, q *gen.Queries, nodeID, cid, assignmentID string, gen_ int64) {
    blk, err := q.SelectRandomBlockForCID(ctx, gen.SelectRandomBlockForCIDParams{BlobCid: cid, BlockSize: int32(s.cfg.MaxBlockBytes)})
    if err != nil { slog.Info("audit.possession.skipped", "reason", "no_eligible_block", "cid", cid); return }
    addr, ok := s.donorAddr(ctx, nodeID)
    if !ok { return }
    target := AuditTarget{AuditID: uuidNew(), NodeID: nodeID, BlobCID: cid, BlockCID: blk.BlockCid,
        AssignmentID: assignmentID, Generation: gen_, Nonce: randNonce(), Deadline: s.now().Add(s.cfg.Deadline)}
    // Insert-before-dispatch (D-M6-3b).
    if err := q.InsertAuditChallenge(ctx, insertParams(target)); err != nil { return }
    cctx, cancel := context.WithTimeout(ctx, s.cfg.Deadline)
    defer cancel()
    res, _ := s.dispatch.Challenge(cctx, addr, wire.AuditChallenge{
        ChallengeID: target.AuditID, BlobCID: cid, AssignmentID: assignmentID, Generation: gen_,
        BlockIndex: blk.BlockIndex, BlockCID: blk.BlockCid, BlockSize: int64(blk.BlockSize), Nonce: target.Nonce})
    if err := s.auditor.Record(ctx, target, res, s.cfg.ReputationFloor); err != nil {
        slog.Warn("audit.possession.record_error", "cid", cid, "err", err)
    }
    s.logOutcome(cid, nodeID, res)
}
```

`donorAddr` reads `nodes.source_nebula_addr` via `GetNodeSourceAddr` (Task 6). `due(n)` modulates cadence: track `lastRun[nodeID]`; trusted & rep≥0.95 → 1.25× interval, probationary/rep<0.5 → 0.25× interval. `Run` is the integrity-style ticker calling `runOnce`, after `ReconcileOnStartup`.

`ReconcileOnStartup` does two things (D-M6-5 resume-from-natural-cadence + D-M6-3b crash recovery):

```go
func (s *Scheduler) ReconcileOnStartup(ctx context.Context) error {
    q := gen.New(s.pool)
    // 1) Crashed-mid-flight challenges -> skip.
    if err := q.ReconcileStaleAudits(ctx, s.cfg.StaleGraceSeconds); err != nil { return err }
    // 2) Seed per-node lastRun so a restart does not re-audit every due node at once.
    rows, err := q.SelectLastAuditPerNode(ctx)
    if err != nil { return err }
    s.mu.Lock()
    for _, r := range rows { s.lastRun[r.NodeID.String()] = r.LastDecidedAt.Time }
    s.mu.Unlock()
    return nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/audit/possession/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/audit/possession/scheduler.go internal/audit/possession/scheduler_test.go internal/db/queries/possession.sql internal/db/gen
git commit -m "feat(p2-m6): possession audit scheduler — two-stage sampling, fast-lane quota, startup reconcile (P2-M6)"
```

---

## Task 10: Config block + validation + /settings knobs

**Files:**
- Modify: `internal/config/types.go`
- Modify: `internal/api/handlers/config_admin.go` (field metadata for the two first-class knobs)
- Test: `internal/config/possession_test.go`

**Interfaces:**
- Produces: `type PossessionAudit struct{...}` with these exact fields/accessors (consumed by Task 13's wiring):
  - fields: `BaseIntervalSeconds int`, `DeadlineSeconds int`, `AuditBudgetFraction float64`, `MaxBlockBytes int64`, `MinAgeDays int`, `MinPassedAudits int64`, `MinAckedTransfers int64`, `GraduateReputation float64`.
  - accessors (all default the zero value): `EffectiveBaseInterval() time.Duration` (3600s), `EffectiveDeadline() time.Duration` (30s), `EffectiveAuditBudgetFraction() float64` (0.01), `EffectiveMaxBlockBytes() int64` (262144), `EffectiveMinAge() time.Duration` (7d), `EffectiveGraduateRep() float64` (0.95), `EffectiveMinPassedAudits() int64` (10), `EffectiveMinAckedTransfers() int64` (5).
  - `Validate() error`.

- [ ] **Step 1: Write the failing test**

```go
func TestPossessionAuditDefaults(t *testing.T) {
    var p PossessionAudit // all zero values
    if p.EffectiveDeadline() != 30*time.Second { t.Fatal("deadline default") }
    if p.EffectiveAuditBudgetFraction() != 0.01 { t.Fatal("budget default") }
    if err := p.Validate(); err != nil { t.Fatalf("zero value must validate (means unset): %v", err) }
}
func TestPossessionAuditValidationRejectsBadFraction(t *testing.T) {
    if err := (PossessionAudit{AuditBudgetFraction: 1.5}).Validate(); err == nil { t.Fatal("fraction > 1 must be rejected") }
    if err := (PossessionAudit{AuditBudgetFraction: -0.1}).Validate(); err == nil { t.Fatal("negative fraction must be rejected") }
    if err := (PossessionAudit{DeadlineSeconds: -1}).Validate(); err == nil { t.Fatal("negative deadline must be rejected") }
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/config/ -run TestPossessionAudit -v`
Expected: FAIL.

- [ ] **Step 3: Implement the block + validation**

Add `PossessionAudit PossessionAudit `yaml:"possession_audit,omitempty"`` to the operator `Config`, the struct with the fields above, and the `Effective*` accessors that supply defaults for zero values. **Zero means "unset" → defaulted**, so `Validate()` must *allow* zero and reject only invalid *explicit* values: `AuditBudgetFraction < 0 || > 1`, `DeadlineSeconds < 0`, `BaseIntervalSeconds < 0`, `MaxBlockBytes < 0`, `GraduateReputation < 0 || > 1`, `MinAgeDays < 0`, `MinPassedAudits < 0`, `MinAckedTransfers < 0`. Wire its `Validate()` into the existing config-validation aggregation.

- [ ] **Step 4: Add the two first-class /settings knobs**

In `internal/api/handlers/config_admin.go`, register field metadata (restart-effect) for `possession_audit.base_interval_seconds` and `possession_audit.deadline_seconds` (mirror an existing restart-effect orchestrator field). The rest stay advanced/effective-config-only.

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./internal/config/... ./internal/api/handlers/... -run 'Possession|Config' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/config/types.go internal/config/possession_test.go internal/api/handlers/config_admin.go
git commit -m "feat(p2-m6): possession_audit config block + validation + /settings knobs (P2-M6)"
```

---

## Task 11: `federation.node_suspect` webhook window + accept-list

**Files:**
- Modify: `internal/notify/emitter.go`
- Test: `internal/notify/emitter_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestNodeSuspectWindowIs24h(t *testing.T) {
    e := &BestEffortHTTP{massCasualtyWindow: time.Hour}
    if e.windowSeconds("federation.node_suspect") != int((24*time.Hour).Seconds()) {
        t.Fatal("node_suspect should suppress per 24h, scoped by node_id")
    }
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/notify/ -run TestNodeSuspectWindow -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Add to `windowSeconds`'s switch:

```go
case "federation.node_suspect":
    return int((24 * time.Hour).Seconds())
```

Confirm the webhook destination accept-list (config) permits `federation.node_suspect` (add it to the known/allowed event types alongside `federation.node_revoked` etc., so `d.accepts(ev.Type)` can match a destination subscribed to it).

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/notify/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/notify/emitter.go internal/notify/emitter_test.go
git commit -m "feat(p2-m6): federation.node_suspect webhook (24h node-scoped suppression) (P2-M6)"
```

---

## Task 12: `novactl node trust` subcommands

**Files:**
- Modify/Create: `cmd/novactl/node_trust.go` (follow the `novactl node` DB-direct pattern from M2/M5)
- Test: `cmd/novactl/node_trust_test.go`

**Interfaces:**
- Produces: `novactl node trust clear-review <node_id>`, `... suspend <node_id>`, `... unsuspend <node_id>` (DB-direct via `DATABASE_URL`, like `novactl setup`).

- [ ] **Step 1: Write the failing test**

```go
func TestNodeTrustClearReviewResetsMarkerAndEpoch(t *testing.T) {
    // seed a node with trust_review_required_at set; run clear-review; assert NULL marker + epoch bumped.
}
func TestNodeTrustSuspendUnsuspend(t *testing.T) {
    // suspend -> trust_state='suspended'; unsuspend -> 'probationary'.
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/novactl/ -run TestNodeTrust -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Add the `node trust` subcommand group. `clear-review` calls `ClearTrustReview` (Task 6). `suspend`/`unsuspend` call `SetTrustState` with `'suspended'`/`'probationary'`. Use the existing `novactl` DB-direct connection helper.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./cmd/novactl/ -run TestNodeTrust -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/novactl/node_trust.go cmd/novactl/node_trust_test.go
git commit -m "feat(p2-m6): novactl node trust clear-review|suspend|unsuspend (P2-M6)"
```

---

## Task 13: Coordinator wiring + loopback-mTLS E2E

**Files:**
- Modify: `cmd/coordinator/main.go`
- Test: `pkg/coordinator/m6_possession_e2e_test.go`

- [ ] **Step 1: Wire the scheduler in `cmd/coordinator/main.go`**

After the orchestrator block (~line 540), construct and run the possession scheduler, reusing the coordinator's federation client TLS (the same `*tls.Config` used for donor reads) and `opCfg.PossessionAudit`:

```go
if opCfg.Federation.Enabled() { // same guard the orchestrator/source path uses
    auditor := possession.NewAuditor(pool, notifier, possession.TrustConfig{
        MinAge: opCfg.PossessionAudit.EffectiveMinAge(), MinPassedAudits: opCfg.PossessionAudit.EffectiveMinPassedAudits(),
        MinAckedXfers: opCfg.PossessionAudit.EffectiveMinAckedTransfers(), GraduateRep: opCfg.PossessionAudit.EffectiveGraduateRep(),
    })
    psched := possession.NewScheduler(pool, possession.NewDispatcher(coordinatorClientTLS), auditor, possession.SchedulerConfig{
        Deadline: opCfg.PossessionAudit.EffectiveDeadline(), MaxBlockBytes: opCfg.PossessionAudit.EffectiveMaxBlockBytes(),
        ReputationFloor: opCfg.Orchestrator.EffectiveReputationFloor(),
        NewAckWindow: 15 * time.Minute, FastLaneQuota: 8, NodesPerTick: 32, BaseInterval: opCfg.PossessionAudit.EffectiveBaseInterval(),
    })
    if err := psched.ReconcileOnStartup(context.Background()); err != nil { slog.Warn("audit.possession.startup_reconcile_error", "err", err) }
    go psched.Run(ctx)
}
```

(`coordinatorClientTLS` is the client `*tls.Config` already built for `EnableDonorReadSource`; reuse that variable.)

- [ ] **Step 2: Write the E2E test**

Create `pkg/coordinator/m6_possession_e2e_test.go` modeled on `m41_readredirect_e2e_test.go` (real loopback mTLS, a real-ish donor `source.Server` over a fake/loopback Kubo): a donor acks a pin; the coordinator scheduler issues one challenge.

```go
func TestE2EPossessionPassThenLyingDonor(t *testing.T) {
    env := newFederationLoopbackEnv(t) // reuse the M4.1/M5 e2e harness
    donor := env.AddDonorHoldingBlock(t /* blob, block bytes */)
    // 1) Honest donor: a challenge passes, reputation drifts up, pin stays acked.
    env.RunOnePossessionTick(t)
    env.AssertAuditResult(t, donor, "pass")
    env.AssertPinState(t, donor, "acked")
    // 2) Coordinator's local copy ABSENT — pass must still verify (D-M6-15 #1).
    env.PruneCoordinatorLocal(t)
    env.RunOnePossessionTick(t)
    env.AssertAuditResult(t, donor, "pass")
    // 3) Lying donor: drop the block locally -> 404 -> hard fail.
    donor.DropBlockLocally(t)
    env.RunOnePossessionTick(t)
    env.AssertAuditResult(t, donor, "fail")
    env.AssertPinState(t, donor, "failed")
    env.AssertReconcileEnqueued(t /* cid */)
    env.AssertReputationBelow(t, donor, 0.6)
}

func TestE2EBlockGetLocalNoNetworkFetch(t *testing.T) {
    // donor lacks the block locally but a peer has it -> 404, no fetch (D-M6-15 #12).
}
```

- [ ] **Step 3: Run the E2E**

Run: `go test ./pkg/coordinator/ -run TestE2EPossession -v`
Expected: PASS.

- [ ] **Step 4: Full build + boundary + frozen gates**

Run: `go build ./... && go test ./internal/audit/possession/... ./internal/node/... && bash scripts/check_node_deps.sh && bash scripts/check-migrations-frozen.sh`
Expected: all PASS/green.

- [ ] **Step 5: Commit**

```bash
git add cmd/coordinator/main.go pkg/coordinator/m6_possession_e2e_test.go
git commit -m "feat(p2-m6): wire possession scheduler + loopback-mTLS E2E (no-origin verify, lying-donor) (P2-M6)"
```

---

## Task 14: Spec amendments + ROADMAP

**Files:**
- Modify: `docs/specs/POSSESSION_AUDIT.md`, `docs/specs/DATA_MODEL.sql`, `docs/specs/HEALING_PROTOCOL.md`, `docs/THREAT_MODEL.md`, `docs/specs/ARCHITECTURE_DECISIONS.md`, `docs/specs/FEDERATION_PROTOCOL.md`, `docs/ROADMAP.md`

- [ ] **Step 1: Amend `POSSESSION_AUDIT.md`** (D-M6-13)

Status → implemented. Response carries **block bytes** (not a digest); verification is CID-reconstruction (`stored.Prefix().Sum(bytes)`); the challenge carries `assignment_id`/`generation` (assignment-bound); the domain-separated, length-prefixed transcript digest; `received_at` vs `decided_at`; synchronous-only (two-call marked not-implemented); a failed audit invalidates the pin + corrects M5 durability; the donor-side audit governor; `federation.node_suspect` canonical (alias `node.suspect`).

- [ ] **Step 2: Amend the other specs**

`DATA_MODEL.sql` — annotate `pin_audits.received_at`/`decided_at`/`transcript_hash` and the `nodes` trust-epoch/review columns. `HEALING_PROTOCOL.md` — reputation now moves; audit recency is a bounded source preference, not a new countability. `THREAT_MODEL.md` — collusion/backdating mitigations as implemented. `ARCHITECTURE_DECISIONS.md` — a trust-graduation-policy row (auto graduate/demote; operator-only suspend). `FEDERATION_PROTOCOL.md` — the audit endpoint (coordinator→donor control traffic, no repair token, returns block bytes).

- [ ] **Step 3: Mark P2-M6 done in `docs/ROADMAP.md`**

Add the `**P2-M6** ✅` row to the Phase 2 progress table (mirror the M5 row's format), with the design/plan paths and the deferrals (`envelope_round_trip` + corpus-scale benchmark gate + Prometheus → M7).

- [ ] **Step 4: Commit**

```bash
git add docs/specs docs/THREAT_MODEL.md docs/ROADMAP.md
git commit -m "docs(p2-m6): mark P2-M6 done; POSSESSION/DATA_MODEL/HEALING/THREAT/ARCH/FED amendments (P2-M6)"
```

---

## Cross-cutting coverage notes

These design decisions are satisfied across tasks rather than by a dedicated task — verify them during review:

- **Observability slog set (D-M6-11).** Emit in the tasks that own each event: Task 9 emits `audit.possession.{challenged,passed,failed,skipped}` (fail/skip `reason`) and `audit.governor.exhausted`; Task 8 emits `audit.reputation.moved` and `audit.trust.{graduated,demoted}` (add a one-line `slog.Info` at each `MoveReputation`/`SetTrustState` call). USE/RED-named, the blueprint for the M7 Prometheus promotion.
- **Audit-recency source preference (D-M6-9).** The primary mechanism — reputation now moves (Task 8) and already flows into both `ListSourceableHolders` and `ListRepairSourceHolders` orderings — needs no query change. The `pin_audits_recent_pass_node_blob_idx` index (Task 1) provisions an explicit recent-pass tie-breaker; adding it to a source query is **optional within M6** and should only be done if EXPLAIN shows it earns its keep. M6 must **not** add a direct audit-recency term to the placement engine, and must **not** change either source query's `suspended` handling (the read/repair asymmetry stays as documented).
- **Anti-cheat tests (D-M6-14).** *Collusion* (a donor satisfying a challenge by fetching from a peer in-window) is exercised by Task 13's `TestE2EBlockGetLocalNoNetworkFetch` — a donor lacking the block locally returns `404` and performs no fetch. *Backdating* is structurally impossible in the synchronous design (the donor sends no timestamp) and is covered by Task 8 stamping `received_at` coordinator-side. *Replay* of a `challenge_id` is prevented by `challenge_id == pin_audits.id` (PK) + insert-before-dispatch (Task 6/9): a duplicate insert conflicts; add a unit assertion in Task 9's scheduler test that a re-used audit id cannot double-insert.

## Final verification (before merge)

- [ ] `go build ./...`
- [ ] `go test ./...` (or at least `./internal/audit/possession/... ./internal/node/... ./internal/notify/... ./internal/config/... ./pkg/coordinator/... ./cmd/...`)
- [ ] `bash scripts/check_node_deps.sh` (donor boundary — green; the donor stays `go-cid`-free)
- [ ] `bash scripts/check-migrations-frozen.sh` (green; only `0015` added)
- [ ] `gofmt -l` on touched files is empty
- [ ] Spot-check the design's failure-mode acceptance criteria (D-M6-15 #1–#12) each map to a passing test.

Then finish the branch per the milestone workflow (local fast-forward merge to `main` + annotated tag `p2-m6-possession-audits`; no remote push).
