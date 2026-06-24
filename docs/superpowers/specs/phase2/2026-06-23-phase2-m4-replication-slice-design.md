# P2-M4 — v1 opaque replication vertical slice

Status: **design**. Spec floor: the P2-M0-amended normative specs in `docs/specs/`
(`FEDERATION_PROTOCOL.md` §§ "Repair transport (Phase 2)" / "Donor inbound
endpoint" / `/fed/v1/pins/{cid}/{ack,fail}`, `IPFS_IMPORT_RULES.md`,
`ENCRYPTION_ENVELOPE.md` v1, the D1/D4/D11 reconciliations), the Phase-2 master
design ([`2026-06-11-phase2-federation-design.md`](2026-06-11-phase2-federation-design.md)
§§ "Component ownership", "Storage/read architecture (P2-M2.1 amendment)",
"Milestone breakdown" P2-M4), and the P2-M3 assignment-sync design
([`2026-06-22-phase2-m3-assignment-sync-design.md`](2026-06-22-phase2-m3-assignment-sync-design.md)).
Implementation plan:
[`../../plans/phase2/2026-06-23-phase2-m4-replication-slice.md`](../../plans/phase2/2026-06-23-phase2-m4-replication-slice.md).

Authors: Bug Plowman (operator), Claude (implementation partner).

## Context

Phase 2 makes Nova's data durable across volunteer-run **donor nodes** over a
private Nebula mesh. The **control plane** is complete and merged:

- **P2-M2** (`p2-m2-identity-registration`) — the live mTLS federation channel:
  `register`/`heartbeat`, identity derived from the verified federation cert,
  fail-closed capability negotiation, `trust_state`, and the `novactl node`
  CA/cert/revocation tooling.
- **P2-M3** (`p2-m3-assignment-sync`) — the durable assignment ledger:
  `pin_changes` log + retention, `pin_assignments` versioning
  (`assignment_id`/`generation`), snapshot/epoch recovery, the
  `AssignPin`/`UnpinPin` transaction seam + `novactl pin`, and the donor's
  durable desired-assignment set with crash/long-offline recovery. **M3
  deliberately moves no bytes and the donor never acks** — in the protocol an
  `ack` asserts *verified local storage* (D4), and a donor that has only observed
  an assignment holds no such evidence.

**P2-M4 is the first data plane: the v1 opaque replication vertical slice.** It
closes the evidence loop M3 left open, over the existing **v1 envelope** (donors
are envelope-agnostic, so no crypto change is needed to federate):

```
assignment  →  coordinator-as-source signed grant  →  donor fetches bounded
ciphertext  →  deterministic re-import + root-CID verify  →  local pin  →
persist verified state  →  production ack / fail
```

This is the milestone where an `ack` first means something real. Every later
subsystem trusts the acked-holder set as durability ground truth — **P2-M4.1**
read-source selection + commit quorum, **P2-M5** healing + origin pruning,
**P2-M6** possession audits — so M4's job is to make that signal trustworthy
*before* anything consumes it. The master design's milestone line:

> **P2-M4** — v1 opaque replication vertical slice. coordinator-as-source;
> streaming transfer; deterministic re-import + root-CID compare; donor storage
> limit; authoritative donor budget; Ed25519 repair grants — first real
> federation release over v1 envelopes.

## Decisions (ratified with Bug, 2026-06-23)

### D-M4-1 — Scope is the replication slice only; the storage/read redirect is a mandatory P2-M4.1 before M5.

M4 implements the replication mechanism and **nothing downstream of a verified
holder**. The P2-M2.1 storage/read redirect — donor-backed reads on a coordinator
cache miss, `require_replication_quorum_before_commit`, origin/staging pruning +
`prune_safety_floor`, `coordinator_storage_mode` / bounded cache, transform
re-fetch — is **not** in M4. It becomes **P2-M4.1**, which is **required before
P2-M5**.

This is a milestone split, **not** a relaxation. The P2-M2.1 storage/read target
is **not fulfilled until M4.1 lands**, and **M5 must be designed assuming the
coordinator origin copy is not guaranteed.** The split is correct because
donor-backed reads and origin pruning are only *safe* once M4 has proven that an
acked donor holder is real (fetched, verified, pinned). M4 proves the holder;
M4.1 lets reads and pruning trust it.

The correct read model M4.1 implements (recorded here so it is not lost): P2-M2.1
removes the requirement that the coordinator hold a **local-origin** copy — it
does **not** remove the coordinator from the read path. The coordinator stays the
sole decrypt/serve point for every byte (`T1.26`, donor-blind); on a cache miss it
sources ciphertext from a donor, decrypts, and serves. Donors **never** serve
plaintext to end users.

### D-M4-2 — Coordinator-as-source only; donor-as-source repair is M5.

For initial uploads the source is the coordinator itself (`FEDERATION_PROTOCOL.md`
§ "Donor inbound endpoint"). M4 builds exactly that: the **coordinator** serves
`GET /fed/v1/blob/{cid}`; the **donor is a fetch-only client**. The donor's own
inbound blob server and donor↔donor repair defer to **P2-M5**, because the
healing scheduler is their first real production trigger and building an exposed
serving surface with no scheduler driving it is premature.

M4 still builds the full *source-side* token discipline on the coordinator
endpoint — Ed25519 verify, `source_node_id`/`dest_node_id`/`cid` binding,
`max_bytes`, and a single-use `jti` replay defense — written as reusable seams so
M5's donor-as-source endpoint reuses the same logic rather than re-deriving it.

### D-M4-3 — Verify by deterministic re-import + root-CID comparison (D4 option 1); no CAR path.

The coordinator serves the reassembled envelope bytes (`Backend.Get`); the donor
re-imports them through `IPFS_IMPORT_RULES.md` and compares the **computed root
CID** to the assigned CID. This reuses Nova's shipped, golden-vector-tested
deterministic import and is fully correct per D4. A flat `hash(bytes) == cid` is
explicitly wrong — the CID is a UnixFS/DAG-PB root, not `sha256(bytes)`.

Transfer is **whole-buffer**, bounded by `max_blob_bytes` and a new
`max_transfer_bytes`; verify time + transferred bytes are instrumented so a CAR /
true-streaming `Importer(io.Reader)` path can be justified by measurement later
(it pairs naturally with the v2 streaming-envelope work, M8+). The M1 `transfer`
seam was already shaped around re-import + root-CID compare; M4 fills it in.

### D-M4-4 — No migration; the production ack uses the M3 state machine.

M4 adds **no schema**. The `pin_assignments` state machine (`pending` →
`acked`/`failed`, `acked_at`) and the conditional `AckPinAssignment` /
`FailPinAssignment` queries shipped in M3's migration `0012`; the production
donor `ack` simply drives the *existing* endpoints for real. Repair-signing key
material is a secret-path reference (never in the DB); the coordinator source
identity is a compile-time constant (not a `nodes` row). `migrations-frozen`
stays green — parity with P2-M2.1.

### D-M4-5 — Crash-safe donor write order: fetch → import/pin → persist → ack.

The donor must never ack before the evidence is durable. The order is:

```
fetch bounded ciphertext
  → ipfs add (deterministic re-import + pin) via the Kubo sidecar
  → compare computed root CID to the assignment CID
  → persist local state "verified, ack-pending"
  → POST /fed/v1/pins/{cid}/ack
  → on 204, mark "acked-delivered"
```

A crash after pin+persist but before the ack lands retries the **idempotent**
ack on restart (the coordinator's `ack` handler already treats a replay at the
same generation as a 204). The donor never re-transfers a blob it already holds
and never auto-acks without the verify+persist evidence. The progress record is
**keyed by `(assignment_id, generation)`**: a generation bump (an M3 reassign)
never inherits an older generation's `acked-delivered`, and stale-generation
progress is cleared so the new generation replicates. **On restart, before
retrying an ack the donor (a) drops progress that no longer matches the current
desired assignment's generation, then (b) re-checks that the Kubo sidecar still
holds the CID** (`Has`); if it does not (Kubo GC, disk loss, tampering), it
downgrades to `pending` and re-fetches — stale local JSON must never claim
durability the blockstore cannot back. An **`unpin`** change clears the donor's
progress **and** removes the sidecar pin, so `novactl pin unpin` does not leave
ciphertext pinned forever on the donor.

### D-M4-6 — M4 enforces storage + size limits donor-side; the D11 source bandwidth budget is exercised in M5.

The D11 token-bucket (`internal/node/bandwidth`) is the **authoritative budget of
the node that *sends* bytes** — its documented contract. In M4
coordinator-as-source the donor *receives* bytes, so M4 does **not** debit that
egress bucket; doing so would conflate ingress with the D11 source budget and
quietly change the bucket's meaning. M4 enforces two limits instead:

- a new donor `storage_max_bytes` — pinning that would exceed it is refused with
  `fail(out_of_space)`, tracked against live stored-bytes;
- the size bounds `max_blob_bytes` / `max_transfer_bytes` — the fetch is capped
  and oversize transfers are refused.

The D11 egress bucket lands its first real debit in **M5**, when the donor
becomes a *source* for donor↔donor repair. (If donor operators later want to cap
**inbound** replication traffic, that is a separately named
`replication_receive_budget` — never folded into the D11 source bucket.)

### D-M4-7 — Ed25519 repair-token mint lives in coordinator-only `internal/federation/tokens`; verify is the shared `wire.Verify`.

`wire.Verify`/`SigningInput`/`AssembleToken`/`Claims` already ship (M1) and hold
**no** private-key API — donors only ever verify. M4 adds the coordinator-only
mint in a new `internal/federation/tokens` package. The Ed25519 **repair-signing
key** loads via the existing stdlib `secret.ResolveSecret` chain
(`NOVA_FEDERATION_REPAIR_SIGNING_KEY` → `_FILE` → the operator-configured
`federation.repair_signing_key_path`, default
`/run/secrets/federation-repair-signing-key`) and is a **base64url/hex 32-byte
seed**; the coordinator derives the private+public key at startup. (`LoadSigner`
takes that configured path so the config field is not inert.) The public key
ships to donors as `HeartbeatResponse.RepairTokenPublicKey` =
**base64url(raw 32-byte public key)** (the field exists, empty since M2). Keys
never enter the DB.

### D-M4-8 — Tokens are minted dynamically when serving `/pins/changes`; never persisted.

`pin_changes` stays the durable record of assignment **intent** (M3). Short-lived
transfer tokens must not live in durable state or they expire in place. The
`/pins/changes` handler populates `PinChange.Source` for **pending `assign`** rows
**at serve time**, minting a fresh coordinator-as-source token per response.
`unpin` rows and already-acked assignments carry no source.

### D-M4-9 — Token replay defense is restart-safe without a migration.

The coordinator source endpoint keeps an **in-memory TTL `jti` cache** (single-use
within a token's short life). To close the restart replay window without adding
durable state, the endpoint **rejects any token whose `not_before` predates this
process's `source_boot_time`**. The per-serve mint (D-M4-8) correspondingly
**clamps `not_before` to be no earlier than `source_boot_time`**, so a token
minted immediately after startup is not rejected by its own floor. After a
coordinator restart a donor simply re-polls `/pins/changes` and receives a
freshly minted token. This keeps "no migration" true while leaving no usable
replay window for the short TTL.

### D-M4-10 — Donor storage is a hardened Kubo **sidecar** over the loopback HTTP API, behind a dependency-light shared import spec.

The donor needs a real blockstore to pin and (later) serve and be audited. It
uses a **hardened Kubo sidecar** reached over the loopback HTTP API, **not**
in-process embedded Kubo: embedding Kubo would pull its entire dependency tree
into the `cmd/node` build graph and blow past the M1 minimal-image / dependency
boundary.

Concretely:

- Extract the existing dependency-free import parameters
  (`internal/ipfs/importrules.go` — pure constants + `ShouldUseRawCodec`, no Kubo
  imports) into a new **`internal/ipfs/importspec`** sub-package so both the
  coordinator's `EmbeddedBackend` and the donor share **identical** import
  parameters and therefore produce **bit-identical root CIDs**. (They live in
  `package ipfs` today, which also contains the Kubo-importing `embedded.go`;
  Go compiles the whole package, so the donor cannot import them in place.)
- **Donor CID comparison is canonical-string equality, not go-cid semantic
  equality** (decided 2026-06-23 from the boundary-gate output). `transfer.Verify`
  compares the donor-computed root CID **string** to the assigned CID string
  (`root != cid` ⇒ `cid_mismatch`); it does **not** import `go-cid`. Wiring
  `go-cid` into the donor pulled ~15 third-party packages (the full `go-multihash`
  registration tree — blake3/sha3/murmur3/blake2 — plus multibase, base32/36/58,
  varint, cpuid, x/crypto, x/sys) into the `cmd/node` graph, contradicting the
  donor's deliberately-minimal deny-by-default boundary. This is sound because
  both sides compute roots from the **same shared `importspec`** (cid-version=1,
  sha2-256, raw-leaves, size-262144) and Kubo emits canonical CIDv1 base32, so a
  real match is byte-identical. Residual risk (a future heterogeneous importer or
  alternate multibase emitting a different string for the same CID) is acceptable
  for M4 and can be revisited behind a narrow normalization helper / conformance
  test if a later milestone needs it. The only donor-boundary allowlist addition
  is `internal/ipfs/importspec`.
- Implement a donor-local Kubo HTTP client under **`internal/node/ipfsclient`**
  that mirrors `EmbeddedBackend` **exactly**: `AddDeterministic` branches on
  `importspec.ShouldUseRawCodec(len)` — `/api/v0/block/put?cid-codec=raw&mhtype=sha2-256&pin=true`
  for the raw single-block path, else `/api/v0/add?chunker=size-262144&cid-version=1&raw-leaves=true&hash=sha2-256&pin=true`
  — so root CIDs match bit-for-bit (a single `/api/v0/add` path would mis-CID
  every ≤1 MiB blob). `Has` means **recursively pinned** (`/api/v0/pin/ls?type=recursive`),
  mirroring `EmbeddedBackend.Has`'s pinset check — not mere block presence — so an
  ack-retry's durability check is sound. `Unpin` (`/api/v0/pin/rm`) backs the
  unpin path; `/api/v0/repo/stat` backs stored-bytes. `cmd/node` **never imports
  `internal/ipfs`**.
- `deploy/donor/compose.yaml` gains a hardened Kubo sidecar (private swarm key,
  no public ports), reached on loopback.

The `donor-deps-boundary` allowlist (`scripts/check_node_deps.sh`) is extended by
the **reviewed minimum**: `internal/ipfs/importspec` and the CID/multiformats
deps the client needs for canonical root-CID equality (`go-cid` + its multihash /
multibase / varint transitive set), demonstrated-red against an injected
`internal/ipfs` (embedded) import. The design minimizes the third-party footprint
— if the multiformats tree proves heavier than the gate tolerates, the client
falls back to canonical-string CID comparison — with the gate output as the
deciding evidence.

### D-M4-11 — `blob-transfer/v1` capability; `repair-stream/v1` reserved for M5.

The donor advertises a new capability **`blob-transfer/v1`** — "I can fetch a
source-bearing assignment, verify it by deterministic re-import, pin it, and ack a
verified local pin." The coordinator requires it before it will populate
`PinChange.Source` or expect a real ack. **`repair-stream/v1`** stays reserved for
the M5 **donor-as-source** server (a donor can serve bytes to other donors). The
two are documented distinctly so a fetch-only M4 donor is never confused with an
M5 serving donor.

### D-M4-12 — The donor coordinator client and the source fetcher are separate seams.

The M3 `Client` interface is the **coordinator** API (`Register`, `Heartbeat`,
`GetChanges`, `GetSnapshot`); M4 adds `Ack` + `Fail` to it (M3 deliberately
omitted them). Fetching bytes is a **separate `SourceFetcher`** seam
(`Fetch(ctx, source, cid, token, maxBytes) → reader`). In M4 the `SourceFetcher`
always targets the coordinator-as-source; in M5 the same seam targets donor
sources for repair. Keeping fetch off the coordinator client avoids re-shaping
that interface in M5.

### D-M4-13 — Observability is evidence-shaped.

Per Nova's slog convention (Prometheus promotion deferred to M7), M4 emits signals
named for what they will become, measuring **evidence**, not activity:

| Seam | slog event (fields) | USE/RED |
|---|---|---|
| coordinator source | `fed.blob.served` (`cid`, `bytes`, `dur_ms`, `dest_node_id`, `jti`); `fed.blob.rejected` (`reason`) | RED rate/errors/duration |
| token mint/verify | `fed.token.minted` (`assignment_id`, `cid`, `dest_node_id`); `fed.token.rejected` (`reason`: `bad_signature`/`expired`/`pre_boot`/`replay`/`wrong_dest`) | RED + error class |
| ack | `fed.ack.applied` / `fed.ack.stale` (existing, M3) | RED |
| donor transfer | `node.transfer.fetched` (`cid`, `bytes`, `dur_ms`); `node.transfer.verified` (`cid`, `verify_ms`); `node.transfer.cid_mismatch`; `node.transfer.out_of_space`; `node.transfer.failed` (`reason`) | USE errors + duration |
| donor ack | `node.ack.delivered` (`cid`, `assignment_id`); `node.ack.retry` (`cid`) | RED |

Key measured quantities: **verified holders, ack latency, `cid_mismatch`,
`transfer_bytes`, `verify_ms`, token rejections (incl. replay), `out_of_space`** —
not "assignments completed." `novactl pin list`'s **verified holders** line
(empty since M3) populates for the first time.

## Scope

**In scope.** `internal/federation/tokens` (Ed25519 mint + reserved coordinator
source id + key load); the coordinator `GET /fed/v1/blob/{cid}` source endpoint
(token verify, source/dest/cid binding, `max_bytes`, preflight size check,
restart-safe `jti` replay defense, origin `Backend.Get` stream); dynamic
`PinChange.Source` population in `/pins/changes`; the real
`HeartbeatResponse.RepairTokenPublicKey`; `register` requiring `blob-transfer/v1`;
the `internal/ipfs/importspec` extraction; the donor `internal/node/ipfsclient`
Kubo-sidecar client; the real donor `transfer` (fetch + re-import + root-CID
verify); donor `storage_max_bytes` + size enforcement; durable donor
verified-ack-pending / acked-delivered state with crash-safe ordering and
ACK-time `Has` re-check; the donor `SourceFetcher` + `Client` `Ack`/`Fail` + the
agent fetch→verify→pin→persist→ack loop with fail classification; coordinator +
donor config additions; `cmd/coordinator` + `cmd/node` wiring; the
`donor-deps-boundary` allowlist extension; the e2e loopback-mTLS replication +
crash-recovery + fail-path tests; the observability signal set; doc amendments.

**Out of scope (owning milestone).**

- Donor-backed reads, `require_replication_quorum_before_commit`, origin/staging
  pruning + `prune_safety_floor`, `coordinator_storage_mode` / bounded cache,
  transform re-fetch — **P2-M4.1** (required before M5).
- Donor-as-source inbound `GET /fed/v1/blob/{cid}` server, donor↔donor repair,
  the D11 egress budget's first debit — **P2-M5** (with the healing scheduler).
- Placement / scheduler / replication-factor-driven assignment, failure-domain
  anti-affinity, `blob_replication_state` projection — **P2-M5**. M4 creates
  assignments via the **manual `novactl pin assign` seam + tests**, exactly as
  M3 established; the M5 scheduler will call the same `AssignPin` seam.
- Possession audits, reputation/trust graduation — **P2-M6**.
- Prometheus `/metrics` — **P2-M7** (the slog set above is its blueprint).
- Streaming-AEAD v2 / CAR transfer / `Importer(io.Reader)` — **P2-M8+**.

## Wire reconciliation

The M1–M3 forward-planning means M4 needs almost no new wire **types** — it fills
in behavior on fields that already exist:

- **Already present, now populated:** `PinChange.Source {NodeID, NebulaAddr,
  Token}` (was nil in M3); `HeartbeatResponse.RepairTokenPublicKey` (empty since
  M2); `Ack {AssignmentID, Generation, CID, ByteSize, IPFSPinStatus,
  FetchedFromNodeID}`; `Fail {…, Reason, Details}` with the full reason domain
  (`out_of_space`, `blob_unavailable`, `policy_filter`, `network_error`,
  `kubo_error`, `source_unauthorized`, `cid_mismatch`, `budget_exceeded`,
  `other`) + `NormalizeFailReason`.
- **New constant:** `CapBlobTransfer = "blob-transfer/v1"` (D-M4-11). `Claims`
  and `Verify` are unchanged; the coordinator-only mint reuses `SigningInput` +
  `AssembleToken`.
- **Encoding (D-M4-7):** `RepairTokenPublicKey` is base64url(raw 32-byte public
  key); the `token` in `ChangeSource` is the existing `signingInput.sig` wire
  form.

The package stays dependency-free (donor-safe; no operator-only imports).

## Architectural notes

### Coordinator-as-source endpoint

`internal/federation/coordinator/blob.go` registers `GET /fed/v1/blob/{cid}` on
the M2 federation mux (Nebula-bound, mTLS — never the public/admin mux). The
handler reuses `authNode` (verified-cert identity, revoked-node rejection), then:

1. reads `X-Nova-Repair-Token`; `wire.Verify(pub, token, now)` with the
   coordinator's own public key;
2. checks `claims.source_node_id == reservedCoordinatorSourceID`,
   `claims.dest_node_id == authenticated node`, `claims.cid == {cid}`,
   `claims.not_before ≥ source_boot_time` (D-M4-9), and the `jti` is unseen
   (insert into the TTL cache, else `fed.token.rejected reason=replay`);
3. **preflight size check (D-M4-3 size bounds):** loads `blobs.byte_size` for the
   CID **restricted to `state = 'active'`** — quarantined / tombstoned /
   soft-deleted blobs are **not sourceable** and yield a clean 404
   `blob_unavailable` — and, if the size exceeds `claims.max_bytes` or
   `max_transfer_bytes`, rejects **before** writing any body byte (no partial
   stream the donor would misclassify);
4. streams the origin envelope from `Backend.Get(cid)` (the coordinator keeps the
   origin copy through M4; pruning is M4.1), wrapped in an `io.LimitReader` at
   `max_bytes` as defense-in-depth;
5. emits `fed.blob.served`.

A 404 (origin missing the CID) is a clean `blob_unavailable` to the donor. The
**reserved coordinator source identity** is a fixed documented UUID constant
defined once in the dependency-free `wire` package (`wire.CoordinatorSourceID`,
aliased as `tokens.ReservedCoordinatorSourceID`) so the donor can echo it as
`Ack.FetchedFromNodeID` without importing operator-only code; it is explicitly
**not** a `nodes` row.

### Donor storage + transfer + verify

`internal/node/ipfsclient` (Kubo sidecar, D-M4-10) provides `AddDeterministic`
(POST `/api/v0/add` with `importspec` params + `pin=true`, returns root CID),
`Has`, `Get`, and `repoStoredBytes`. `internal/node/transfer` (replacing the M1
stub) fetches via the `SourceFetcher`, reads at most `max_bytes`
(`io.LimitReader`), calls `AddDeterministic`, and compares the returned root CID
to the assignment CID — equal ⇒ verified; unequal ⇒ `cid_mismatch`. Errors are
classified into the `Fail` reason domain (D-M4-8): source 404 → `blob_unavailable`,
sidecar error → `kubo_error`, token rejected → `source_unauthorized`, transport →
`network_error`, oversize → `cid_mismatch`/size refusal, storage cap →
`out_of_space`.

### Donor durable state + control loop

`internal/node/state` gains per-assignment progress beyond M3's desired set:
`desired (pending)` → `verified-ack-pending` → `acked-delivered`, persisted
**before** the ack (D-M4-5), atomic-JSON like the existing stores. The agent's
existing sync loop already converges the desired set; M4 adds, for each `pending`
assignment that now carries a `Source`, the
fetch→verify→pin→persist→ack/fail step, bounded by `max_pin_concurrency` and the
`storage_max_bytes` accounting. On startup the agent reconciles any
`verified-ack-pending` entry by re-checking `Has` (D-M4-5) then retrying the
idempotent ack. The `Client` interface gains `Ack`/`Fail`; fetch is the separate
`SourceFetcher` (D-M4-12).

### Coordinator/donor config additions

Operator `Federation` block: `repair_token_ttl_seconds` (default 300, range
60–1800 per spec), `federation_repair_signing_key_path` (or the env/secret-mount
chain), `max_transfer_bytes`, and the coordinator's advertised source
`nebula_addr` (derived from the existing listen addr / nebula interface). Donor
`node.yaml`: `storage_max_bytes` and the Kubo sidecar API address. Both validated
with shallow checks consistent with the existing loaders.

## Component ownership

| Concern | Coordinator (operator) | Donor | Notes |
|---|:--:|:--:|---|
| Ed25519 repair-token **mint** | ✓ | — | private key; `internal/federation/tokens` |
| repair-token **verify** | ✓ (own endpoint) | — (M5 reuses) | shared `wire.Verify` |
| `GET /fed/v1/blob/{cid}` **source** | ✓ | — (M5 adds donor source) | origin `Backend.Get` |
| dynamic `Source` in `/pins/changes` | ✓ | — | per-serve mint, not persisted |
| token public key delivery | ✓ (heartbeat) | consumes | base64url raw 32-byte |
| blob **fetch** + re-import + root-CID verify | — | ✓ | `SourceFetcher` + `ipfsclient` |
| local pin (Kubo sidecar) | ✓ (origin, embedded) | ✓ (replica, sidecar) | shared `importspec` ⇒ same CID |
| storage limit + size enforcement | — | ✓ | `out_of_space` |
| D11 egress bandwidth budget | — | ✓ (first debit **M5**) | source-side only |
| production `ack`/`fail` + crash-safe state | — (serves) | ✓ | verify→persist→ack |
| assignment creation | ✓ (`novactl pin`) | — | M5 scheduler reuses `AssignPin` |

## Forward-compatibility

- **P2-M4.1** (required before M5) consumes M4's verified-holder set: the
  "eligible read source" query over the preserved `(cid, state)` index +
  `nodes.reputation_score`, donor-backed reads on cache miss,
  `require_replication_quorum_before_commit`, origin pruning + `prune_safety_floor`,
  `coordinator_storage_mode`/bounded cache, transform re-fetch.
- **P2-M5** adds the donor-as-source `/fed/v1/blob/{cid}` server (reusing M4's
  token-verify + replay seams), donor↔donor repair, the first D11 egress-budget
  debit, the healing scheduler (calling `AssignPin`), 5-state liveness,
  failure-domain anti-affinity, and `blob_replication_state`. **M5 assumes no
  guaranteed coordinator origin copy** (M4.1 has pruned).
- **P2-M6** uses the acked-holder set + the Kubo sidecar's block access for
  possession audits.
- **P2-M7** promotes the D-M4-13 slog set to Prometheus.
- **P2-M8+** may replace whole-buffer transfer with CAR / `Importer(io.Reader)`,
  justified by the M4 `verify_ms` / `transfer_bytes` measurements, alongside the
  streaming-AEAD v2 envelope.

## Trust-model notes (Phase 2 adversaries; canonical text in `THREAT_MODEL.md`)

- **Repair-token forgery / replay (D1).** Donors hold only the public key
  (Ed25519, asymmetric) and cannot mint. Replay is defended by the source-side
  single-use `jti` cache **plus** the `source_boot_time` floor (D-M4-9); `max_bytes`
  + short TTL + source/dest/cid/generation binding bound any single grant. In M4
  the source is the coordinator, so the budget-drain vector is latent until M5
  donor sources exist — the seam is built replay-safe regardless.
- **False durability.** The crash-safe verify→persist→ack ordering + the ACK-time
  `Has` re-check (D-M4-5) ensure an `ack` always reflects bytes the blockstore
  actually holds, not stale local JSON.
- **Bandwidth exhaustion (D11).** Unchanged in spirit; the authoritative
  egress bucket is exercised in M5 where the donor sends bytes. M4 caps ingest by
  size + storage only and does not pretend to enforce the egress budget.
- **Supply-chain.** The `donor-deps-boundary` gate keeps the M4 Kubo-sidecar
  client a minimal, reviewed allowlist extension; embedded Kubo never enters the
  donor build graph.

## Exit criterion

A manually assigned CID is replicated from the coordinator to a donor over the
federation transport: the donor fetches bounded ciphertext through a signed
coordinator-as-source grant, verifies the computed root CID by deterministic
re-import, pins it in its Kubo sidecar, persists local verified state, and sends a
production `ack` that the coordinator conditionally accepts by `assignment_id` +
`generation`. A crash between pin and ack recovers by re-checking the local pin
and retrying the idempotent ack; terminal conditions (`out_of_space`,
`cid_mismatch`, `blob_unavailable`, `source_unauthorized`) post a classified
`fail`. `novactl pin list` shows the donor as a **verified holder**.
`donor-deps-boundary` and `migrations-frozen` stay green. **No donor-backed user
reads, upload quorum, origin pruning, or donor-as-source repair are in this
milestone — those are P2-M4.1 (required before P2-M5).**
