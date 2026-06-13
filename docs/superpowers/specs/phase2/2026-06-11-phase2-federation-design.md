# Phase 2 — Federation + Streaming-AEAD Design

Status: **design** (spec floor: Phase 0 v2/v3 normative specs in `docs/specs/`,
amended by this document — see § "Spec reconciliation backlog"). Implementation
plan generated under the writing-plans skill:
[`../../plans/phase2/2026-06-11-phase2-federation.md`](../../plans/phase2/2026-06-11-phase2-federation.md).

Authors: Bug Plowman (operator), Claude (implementation partner).

## Purpose and scope

Phase 1 shipped a single-host Nova: one coordinator with an embedded hardened
Kubo, all data on one machine. Phase 2 makes the data **durable across machines
the operator does not own** by introducing volunteer-run **donor nodes** that
pin opaque ciphertext, coordinated by the operator's single coordinator over a
private mesh. It also lifts the single-shot encryption envelope to a
**streaming-AEAD** format so large objects can be Range-served.

Phase 2 is **two large, mostly-orthogonal workstreams**, sequenced
deliberately:

1. **Donor federation** (the headline): split the donor pinning node into its
   own binary (`cmd/node`), bring up Nebula-mesh mTLS federation,
   replication-factor enforcement, bandwidth-budgeted healing, and donor
   possession audits. Ships over the **existing v1 envelope** — donors are
   envelope-agnostic, so no crypto change is needed to federate.
2. **Streaming-AEAD envelope (v2 / `NOVE v2`)**: chunk-authenticated encryption
   that supports HTTP `Range` and bounded-memory partial decryption, for audio,
   video, and large archives. Operator-local; lands **after** the federation
   vertical slice is stable and is independently releasable.

### In scope

- Binary split: a donor product `cmd/node` (+ `internal/node/*`) built,
  packaged, signed, and deployed **separately** from the coordinator, with a
  mechanically-enforced dependency boundary (no operator-only code in the donor
  build).
- Coordinator-side federation: `internal/federation` (register / heartbeat /
  pin-changes / snapshot / ack / fail), the donor-inbound blob endpoint served
  by the coordinator as the **initial-upload source** (coordinator-as-source
  self-node), and a Nebula-interface-only federation listener separate from the
  public/admin mux.
- Healing orchestrator: `internal/orchestrator` implementing `HEALING_PROTOCOL`
  with the corrections in this document (acked-only durability, failure-domain
  anti-affinity, authoritative donor budgets, a durable replication-state
  projection instead of per-tick full scans).
- Possession audits: `internal/audit/possession` with the corrections in this
  document (coordinator receive-time deadlines, synchronous challenge-response,
  size/risk-weighted sampling).
- Operator security additions: **Nebula CA + lighthouse provisioning** and
  federation/Nebula cert issuance + revocation (`novactl node …`); an **Ed25519
  repair-token signing key**; a `trust_state`/probation dimension; failure-domain
  bookkeeping; release **signing + SBOM + provenance** promoted into this phase.
- Streaming-AEAD `NOVE v2`: authoritative wire format, deterministic import,
  Range read path, mixed v1/v2 coexistence (M8–M10).

### Out of scope (unchanged Tier-1 non-goals; see `ARCHITECTURE_DECISIONS.md`)

- Multi-coordinator HA / cross-operator (multi-master) federation — permanent
  non-goal (`T1.27`, `T1.28`).
- Erasure coding, formal PDP/POR, hot/cold tiering, payment surfaces — Phase 3+
  / 6+ research.
- Operator-blindness — Nova remains **donor-blind, not operator-blind** (`T1.26`).
- A separate `nova-node` **repository** — deferred until the protocol and donor
  API stabilize and independent maintenance is demonstrably useful (see
  § "Repository topology" → "When to split later").
- Pruning the coordinator's origin copy / donor-backed reads — Phase 2
  **replicates, it does not migrate**; origin pruning is a later, explicitly
  designed storage-tier feature.

## Source of truth and spec reconciliation

Phase 0 produced normative federation/healing/possession/envelope specs **before
any implementation existed**. Implementation review (and the external
architectural review that seeded this design) found that several of those specs
are **internally contradictory or unimplementable as written**, and that some of
the defects sit in the **Tier-1 protocol-enforced** table of
`ARCHITECTURE_DECISIONS.md`. Per that document's own amendment rules, a Tier-1
change requires a **spec version bump + an implementation gate + a Tier-1-table
update**.

Therefore **the first Phase 2 milestone (P2-M0) is specification reconciliation,
not code.** You cannot write a *conforming* `cmd/node` / `internal/federation`
against contradictory Tier-1 specs. This design is the reconciliation's source of
truth; P2-M0 carries the edits into the normative specs.

### Reconciliation backlog

Each item is verified against the cited spec/schema. "Tier" shows the amendment
weight.

| # | Defect (verified location) | Resolution | Tier action |
|---|---|---|---|
| **D1** | Repair token called "HMAC-signed" (`FEDERATION_PROTOCOL.md` § Repair transport; Tier-1 `T1.10`) yet the source donor "verifies the token signature using a coordinator **pubkey**". HMAC is symmetric — there is no pubkey, and distributing an HMAC verify-key lets *any* donor mint tokens. | **Ed25519** asymmetric repair tokens. Coordinator holds the private key; donors receive the public key via `config_updates`. Claims: `jti`, `assignment_id`, `generation`, `cid`, `source_node_id`, `dest_node_id`, `not_before`, `not_after` (≤ `repair_token_ttl_seconds`), `max_bytes`, `protocol_version`. **Single-use / bounded-use** enforced at the source via a small replay cache so a malicious destination cannot replay a valid token to drain the source's budget. | **Amend Tier-1 `T1.10`** (FED v-bump) |
| **D2** | "Streaming chunks align with IPFS block boundaries — **chunk N == block N**" (`ENCRYPTION_ENVELOPE.md` § v2; Tier-1 `T1.7`). False: a 40-byte header + 256 KiB ciphertext + 16-byte tag per record cannot align with fixed 256 KiB UnixFS leaves; boundaries drift immediately. | Pick **one** authoritative layout in M8: (a) a **custom DAG with one raw leaf per encrypted record** (clean block↔record mapping, bespoke linking), or (b) a **fixed 256 KiB encrypted record** (reduced plaintext payload so header+ct+tag == leaf), or (c) **drop the alignment claim** and carry a ciphertext-offset → block map in `blob_manifests`. Recommended direction: (a) custom DAG — cleanest possession/Range story — pending golden vectors + crypto review. | **Amend Tier-1 `T1.7`/`T1.7a`** (ENVELOPE v-bump) — **deferred to M8** |
| **D3** | Per-chunk AAD binds the final `cid`, but the CID is computed over ciphertext that already contains the tags — **circular**; the spec admits it is "worked out in Phase 2" (`ENCRYPTION_ENVELOPE.md` § v2 "requires deliberation"). | Bind per-chunk AAD to a **canonical header commitment** (`hash(canonical_header) ‖ chunk_index ‖ total_chunks ‖ plaintext_len`), not the final CID. The content address authenticates the whole object; per-chunk AAD prevents reordering/contextual substitution. Exact construction + vectors in M8 under crypto review. | Tier-1 `T1.7a` (with D2) |
| **D4** | Destination donor verifies a transfer by `hash(bytes) == cid` (`FEDERATION_PROTOCOL.md` sequence + Repair transport). Wrong for any multi-block object: the CID is a UnixFS/DAG-PB **root**, not `sha256(bytes)` (`blob_manifests.merkle_root`, `codec='dag-pb'`). | Verify by **deterministic re-import through `IPFS_IMPORT_RULES` and compare the computed root CID** to the assigned CID (or receive a CAR and verify every block + root). `github.com/ipld/go-car/v2` is already a dependency. Consistent with Tier-1 `T1.6`. | FED clarification |
| **D5** | `effective_count = acked + 0.5·pending`, `tier1 ⇔ 0 < effective < 2` (`HEALING_PROTOCOL.md`). A CID with **1 acked + 2 pending** scores 2.0 → "safe", though only one durable copy exists. Contradicts Tier-1 `T1.14` ("CIDs at one **acked** pin"). | **Durability classification is acked-only** (Tier-1 `T1.14`). Pending assignments become a separate **in-flight reservation** used only to avoid double-scheduling; they **never** lift a 1-acked CID out of Tier-1. `pending_weight` may still order Tier-2 work but cannot affect the Tier-1 trigger. | Reconcile HEAL to Tier-1 `T1.14` |
| **D6** | `pin_assignments` PK is `(cid, node_id)` with no generation (`DATA_MODEL.sql`). A delayed `ack`/`fail` for a superseded assignment mutates a **reused** row. | Add `assignment_id uuid` + `generation bigint`. `ack`/`fail`/unpin-confirm carry both; transitions are conditional: `UPDATE … WHERE assignment_id=$ AND generation=$ AND state='pending'`. | Schema migration |
| **D7** | The protocol's `pins/changes?since_seq` + `current_epoch` recovery requires a durable monotonic change log; **no such table exists** in the schema. New change-log `kind` values are declared "backward-compatible" — unsafe for a state-mutating op an old donor would silently ignore. | Add `pin_changes(sequence bigserial, node_id, assignment_id, generation, kind, cid, created_at)` + retention; when a donor's `since_seq` predates retention, return machine-readable **`snapshot_required`**. Unknown `kind` values **fail closed** and are **capability-negotiated** (see D-cap), never silently ignored. | Schema + FED |
| **D8** | `nodes.geo_declared` is self-declared and there is **no owner/failure-domain** column; placement picks a random non-holder. A donor can register many nodes (different declared geos) on one host/provider/account. `VOLUNTEER_DEPLOYMENT_GUIDANCE.md` promises same-owner anti-affinity the schema cannot enforce. | Add `donor_principal_id` + `failure_domain_id` (+ `provider`, `asn`, `operator_verified_at`). Placement enforces **anti-affinity** — prefer/require distinct failure domains when enough exist. `geo_declared` stays informational only. Correct the volunteer guide. | Schema + HEAL + guide |
| **D9** | New nodes start at `reputation_score = 1.0` "(probationary)" but full weight (`HEALING_PROTOCOL.md`). Only audit *frequency* is probationary, not *placement weight* — a fresh Sybil can become a sole/second copy of critical data. | Add an **orthogonal `trust_state`** (`probationary` / `trusted` / `suspended`) + a `placement_weight` cap. Probationary nodes: capped data volume, higher audit cadence, **never** the sole or second copy of `important`-class data; graduate on age + successful transfers + passed audits. Do **not** overload the 5-state liveness enum. | Schema + HEAL |
| **D10** | Possession audit passes when `completed_at < deadline`, where `completed_at` is **donor-supplied** (`POSSESSION_AUDIT.md`, `pin_audits.completed_at`). A lying donor backdates it. Sampling is flat per-node regardless of bytes held. | Use **coordinator receive-time** for the deadline. Prefer a **synchronous** challenge→response (single HTTP round-trip; coordinator measures latency) over the two-call design. Sample **weighted by stored bytes / pin count / node age / risk**, not flat per-node. | POSS + schema |
| **D11** | The orchestrator computes source capacity from self-reported + previously-acked usage and schedules asynchronously, so several ticks/destinations can over-commit one donor's budget — yet Tier-1 `T1.12` promises budgets are inviolable. | Coordinator scheduling is a **best-effort reservation**; the **donor's local token-bucket is authoritative** and refuses work exceeding its configured budget. Repair tokens carry `max_bytes`; actual bytes are reconciled via heartbeat/transfer reports. | FED + HEAL + donor |
| **D12** | v2 claims CDN edges "store and serve individual **ciphertext** chunks" (`ENCRYPTION_ENVELOPE.md` § v2 Goals), contradicting Nova's default "reads go through the coordinator → plaintext" model. | Reframe the v2 benefit as **bounded-memory authenticated Range decryption at the origin** (the coordinator fetches + decrypts only the records covering the range and returns a plaintext `206`). A ciphertext-caching edge would require a **Nova-aware** intermediary and is not the default. | ENVELOPE wording (M8/M10) |
| **D-cap** | `register` carries a single `client_version` string — sufficient for support, insufficient for interop. | Add **capability negotiation**: `register` sends `supported_protocols` (`fed/v1`, …) + `capabilities` (`pin-change-log/v1`, `snapshot/v1`, `repair-stream/v1`, `audit-block-hash/v1`, …); the coordinator replies `selected_protocol` + `required_capabilities` and **fails registration clearly** when no compatible set exists. Identity is derived from the **verified mTLS cert**, not self-asserted JSON. | FED |
| **D-sign** | Release **signing** is deferred to Phase 5 in the threat model, while Phase 2 instructs volunteers to `docker pull` and run a **privileged network daemon**. | **Promote into Phase 2:** cosign signatures + per-image SBOM + SLSA/GitHub attestations for both images; the donor walkthrough pins an **image digest, not `latest`**, and verifies the signature. | THREAT_MODEL + CI |

> Anything in the normative specs that still says *HMAC repair token*,
> *`hash(bytes) == cid`*, *chunk N == block N*, *pending counts toward
> durability*, or *donor-supplied audit deadline* is **superseded by this
> design** and corrected in P2-M0 / M8.

### Phase 1 impact: reused, new, evolved — and how the Tier-1 fixes land

The three Tier-1 corrections (D1 Ed25519, D2 chunk layout, D3 AAD) require **no
rework of shipped Phase 1 code.** Phase 1 is single-node: it never implemented
federation or the v2 envelope, so the only thing wrong *today* is **spec prose**
in the Phase 0 normative docs — fixed by editing text, not code.

- **D1 (Ed25519 tokens).** `internal/federation`, `internal/orchestrator`,
  `internal/audit/possession`, and `cmd/node` **do not exist** in Phase 1. The
  repair token is net-new Phase 2 (`internal/federation/tokens`, `crypto/ed25519`,
  a new federation signing key delivered to donors via `config_updates`). The
  Phase 1 signed-URL HMAC (`signing_keys`, M7) is a *different* mechanism and
  correctly stays HMAC. **Phase 1 rework: none.**
- **D2 / D3 (v2 record layout + AAD).** Phase 1 shipped only the **v1**
  single-shot codec plus a deliberately layout-agnostic v2 **seam**:
  `envelope.Decode` dispatches on the version byte, `VersionV2 (0x02)` is reserved
  and refused cleanly, the `Codec` interface comment already plans v2's streaming
  decrypt as *"a separate optional interface that v1 does not implement"*,
  `blob_manifests.codec` is free-form, and `blob_blocks` records whatever blocks
  result. v2 is a **new** `internal/envelope/v2.go` + a new import/Range path that
  **coexists with v1**; the seam is untouched. v1 carries no content AAD at all
  (key-wrap uses empty AAD), so D3 changes nothing in v1. **Phase 1 rework: none.**

The single **additive evolution** is the v2 write path (M9): the whole-buffer
`envelope.Encrypt` and `ipfs.AddDeterministic([]byte)` gain **streaming siblings**
(`StreamingEncoder.EncryptTo`, `Importer(io.Reader)`, `RangeDecrypter.OpenRange`)
— *new methods alongside* the existing ones, exercised only when emitting/reading
v2. v1 blobs flow through the unchanged whole-buffer path forever.

| Fix | Spec reconciliation | Authoritative design / implementation |
|---|---|---|
| **D1** Ed25519 tokens | **P2-M0** (FED + `T1.10`) | implement in **P2-M4** |
| **D2** chunk / DAG layout | **P2-M0** removes the false "chunk N == block N" claim and marks it M8-pending | **P2-M8** (layout + golden vectors + crypto review); build in M9/M10 |
| **D3** per-chunk AAD | **P2-M0** flags the circular-CID AAD as unresolved/M8 | **P2-M8** (header-commitment AAD + vectors + crypto review) |

So all three are handled as **P2 spec-reconciliation tasks**: D1 fully in P2-M0;
D2/D3 reconcile the *contradiction* in P2-M0 and defer the *cryptographic
redesign* to P2-M8 so it never gates the federation release. The classes of work
across the whole phase:

- **Reused as-is from Phase 1:** version dispatch (`envelope.Decode`), the `Codec`
  interface, the `VersionV2` reservation, `blob_manifests.codec` / `blob_blocks`,
  `envelope_version` surfacing, master-key wrapping + rotation + crypto-shred, and
  the entire single-node v1 read/write path.
- **Net-new in Phase 2:** all federation (`cmd/node`, `internal/federation`,
  `internal/orchestrator`, possession audits) and the v2 codec + import/Range path.
- **Evolved (additive, not rework):** streaming variants of the encrypt/import
  interfaces on the v2 write path only.

## Architecture overview

### Repository topology — monorepo, donor-extractable

The donor node stays in the `github.com/nova-archive/nova` module but is a
**separately built, separately packaged, dependency-constrained product**. The
single module is intentional: the federation contract is undergoing its first
implementation and the Tier-1 corrections above require atomic cross-cutting
changes to schema + coordinator + donor + simulations + docs. Splitting the repo
now would freeze boundaries around an untested API (Phase 1 deferred `pkg/node`
for exactly this reason).

```
cmd/
  coordinator/      operator binary  → image: nova-coordinator
  node/             donor binary     → image: nova-node            (NEW)
  novactl/          + node/ca/lighthouse subcommands               (EXTENDED)
  migrate/

internal/
  federation/
    wire/           shared protocol + token + capability types     (NEW, importable by both binaries)
    coordinator/    register/heartbeat/changes/snapshot/ack/fail + coordinator-as-source
    tokens/         Ed25519 repair-token mint (coordinator) + verify (shared)
    transport/      mTLS-over-Nebula client/server helpers
  node/                                                            (NEW — donor only)
    agent/          register→heartbeat→sync loop
    state/          local cursor/cert/replay store (NO Postgres)
    transfer/       streaming fetch + deterministic re-import + root-CID verify
    bandwidth/      authoritative local token-bucket
    audit/          possession-challenge responder (local blockstore only)
  orchestrator/     HEALING_PROTOCOL (coordinator)                 (NEW)
  audit/possession/ challenge scheduler + verifier (coordinator)   (NEW)
  envelope/         + NOVE v2 streaming codec (M8–M10)             (EXTENDED)
  ipfs/             + streaming Importer(io.Reader)                (EXTENDED)
  masterkey/ moderation/ auth/ api/ db/ …                          (operator-only, unchanged)

pkg/
  coordinator/      unchanged public surface
  node/             graduates from internal/node ONLY when an external donor
                    implementation needs it (not in Phase 2)
```

**Dependency boundary (mechanically enforced).** A blocking CI job runs
`go list -deps ./cmd/node` and fails if the donor build graph imports any
operator-only root (`internal/masterkey`, `internal/moderation`, `internal/auth`,
`internal/setup`, `internal/api/handlers/admin*`, `nova-image`, `pkg/coordinator`,
`internal/db`, …). The shared contract lives in `internal/federation/wire` — **not**
a public `pkg/fedwire` — until an external consumer justifies a semver-stable
public promise. The load-bearing requirement is the *boundary*, not the path.

**Separate artifacts.** Two images from two Dockerfiles. `nova-node` contains
**only** the donor binary + CA certs + minimal health tooling: no libvips, no
Node/web bundles, no migrate, no novactl, no Postgres client, no master-key code
or secret paths. The donor is **not** a profile in the operator compose file —
different actor, trust, secrets, ports, storage, and guidance.

```
docker/
  coordinator.Dockerfile     deploy/operator/compose.yaml   .env.example
  node.Dockerfile            deploy/donor/compose.yaml      node.yaml.example
```

#### When to split into a separate `nova-node` repo (later)

Reconsider a separate repo only when several hold: `fed/v1` stable across
releases; donor releases routinely ship without coordinator changes; a distinct
maintainer group owns donor code; volunteers materially benefit from a
donor-only source audit; an **external** donor implementation exists; the shared
protocol is generated from a language-neutral schema. The likely future shape is
`nova` (coordinator + specs) / `nova-node` (reference donor) / `nova-protocol`
(generated schemas + conformance vectors) — created when an organizational
boundary actually exists, not speculatively.

### Deployment topology

```
            ┌──────────────────────── OPERATOR HOST ─────────────────────────┐
   public ─►│ nginx (TLS) ──► coordinator ──► Postgres                        │
   admin  ─►│  public/admin   │  embedded hardened Kubo (origin copy)         │
            │  vhosts         │  orchestrator + possession-audit workers       │
            │                 │  federation listener  ── binds Nebula iface ONLY
            │                 └───────────────┬─────────────────────────────── │
            │  Nebula (host/sidecar process) ─┤  Ed25519 repair-token signer   │
            └─────────────────────────────────┼──────────────────────────────-┘
                                               │  private Nebula overlay (mTLS)
                 ┌─────────────────────────────┼───────────────────────────┐
                 ▼                             ▼                            ▼
        ┌──── DONOR HOST A ────┐      ┌──── DONOR HOST B ────┐     ┌── DONOR HOST C ─┐
        │ Nebula (sidecar)     │      │ Nebula (sidecar)     │     │ Nebula (sidecar)│
        │ nova-node            │      │ nova-node            │     │ nova-node       │
        │  embedded Kubo       │      │  embedded Kubo       │     │  embedded Kubo  │
        │  inbound HTTPS  ◄────────────────  donor↔donor repair (Ed25519 token) ──► │
        │  (Nebula iface only) │      │  NO public ports     │     │  NO public ports│
        │  local token-bucket  │      │  local state store   │     │                 │
        └──────────────────────┘      └──────────────────────┘     └─────────────────┘

Nebula runs as a separate host/sidecar process so nova-node needs no NET_ADMIN.
Donors expose NO public ports; all federation traffic is mTLS over the overlay.
```

### Component ownership

| Concern | Coordinator (operator) | Donor (`nova-node`) | Replicated to donor? |
|---|:--:|:--:|---|
| Plaintext upload / transforms / `nova-image` | ✓ | — | no |
| Envelope encrypt/decrypt, master + per-blob keys | ✓ | — | **never** (donor-blind) |
| Users / roles / moderation / legal-hold | ✓ | — | no |
| Public + signed-URL reads (origin) | ✓ | — | no |
| Canonical blob/collection catalog | ✓ | — | no |
| Node registry, cert authorization/revocation | ✓ | — | no |
| Assignment generation, replication targets, liveness, **healing** | ✓ | — | no |
| Possession-audit scheduling + verification, reputation/trust | ✓ | — | no |
| Nebula CA / lighthouse, repair-token signing | ✓ | — | no |
| Local hardened Kubo blockstore | ✓ (origin) | ✓ (replica) | n/a |
| Pull-based register/heartbeat/sync; deterministic re-import + verify | — | ✓ | n/a |
| Authoritative local bandwidth budget (token-bucket) | — | ✓ | n/a |
| Repair-token **verify**; Nebula-only repair + audit server | — | ✓ | n/a |
| Local cursor / cert / replay store | — | ✓ (no Postgres) | n/a |

**Replicated to donors: almost nothing.** A donor receives only the minimal
assignment tuple — `{assignment_id, generation, cid, expected_size, source
(node_id + nebula_addr + Ed25519 token), op}`. It is **not** given users,
collections, filenames, MIME, product, moderation status, replication factor,
owner, visibility — **or even the `priority:"tier1"` flag**, which leaks "this is
the federation's last safe copy" and is purely coordinator scheduling metadata.

**Replicate, don't migrate.** The coordinator's local Kubo copy remains the read
origin, transform source, initial replication source, repair fallback, and audit
verification source. Donor replicas **add** durability. The replication factor
`R` counts **donor** replicas; the operator origin copy is **additional**, not
counted toward `R` — so donor loss is independent from origin loss. Origin
pruning is a later, explicitly-designed feature.

### Versioning model

The single Go module forbids independent *module* semver for `pkg/coordinator`
vs `pkg/node` without nested modules — which we will **not** introduce just to
make labels look independent. Instead Phase 2 separates **four** version
concepts:

| Version | Example | Meaning |
|---|---|---|
| Coordinator software | `nova-coordinator 0.2.4` | operator image/release tag |
| Donor software | `nova-node 0.1.7` | donor image/release tag (independent cadence) |
| **Federation protocol** | `fed/v1` | the **interop contract** — donor `node/vX` ↔ coordinator `vY` interoperate iff both speak a common `fed/vN` (negotiated at register, D-cap) |
| **Envelope** | `NOVE v1` / `v2` | persisted object encoding; **orthogonal** to donor/coordinator compat — donors are envelope-agnostic |

One root Go module stays at `v0.x`; images/releases are tagged independently;
`fed/vN` + capability negotiation is the real compatibility boundary.

## Schema deltas (P2-M0 / P2-M3 migration)

A new migration (the first **non-frozen** Phase 2 migration) adds:

- `pin_assignments`: `assignment_id uuid`, `generation bigint`, conditional
  state transitions (D6). PK strategy: keep `(cid, node_id)` as a natural
  current-assignment key but make `assignment_id` the immutable handle carried in
  the change log + tokens + acks.
- `pin_changes(sequence bigserial PK, node_id, assignment_id, generation,
  kind, cid, created_at)` + retention policy (D7).
- `blob_replication_state(cid PK, healthy_acked_count, in_flight_count,
  target_count, safety_tier, updated_at)` — the durable, rebuildable projection
  (see Performance).
- `nodes`: `donor_principal_id`, `failure_domain_id`, `provider`, `asn`,
  `operator_verified_at` (D8); `trust_state` enum + `placement_weight` (D9).
- `pin_audits`: coordinator-receive-time columns; sampling-weight inputs (D10).
- Federation key material references (Ed25519 repair-signing key version;
  Nebula/federation CA metadata) — keys themselves stay out of the DB, loaded
  via the existing `ResolveSecret` chain.

All Phase 2 migrations are new files; **no Phase 1 migration is modified**
(the `migrations-frozen` gate stays green).

## Trust-model exploration (Phase 2 adversaries)

Phase 1's `THREAT_MODEL.md` already models a malicious donor, a compromised
coordinator, hostile crawlers, network observers, and supply-chain attackers.
Phase 2 *adds machines the operator does not own*, so the donor-facing attack
surface grows. Each adversary below is paired with the mitigation **now
specified** in this design (the threat-model amendment in `docs/THREAT_MODEL.md`
carries the canonical text).

- **Repair-token forgery / replay (was D1).** A donor must not mint tokens, and a
  malicious destination must not replay a valid token to drain a source's budget.
  *Mitigation:* Ed25519 (donors hold only the public key); single/bounded-use via
  source-side `jti` replay cache; `max_bytes` + short TTL + source/dest/assignment
  binding.
- **Assignment replay (was D6).** A delayed `ack`/`fail` mutating a reused
  `(cid,node)` row could mark a stale assignment durable or cancel a live one.
  *Mitigation:* `assignment_id` + `generation`; conditional state transitions.
- **Sybil / failure-domain forgery (was D8/D9).** One operator-of-donors spins up
  many nominal nodes (distinct declared geos) on one host/provider/account to
  capture placement weight and defeat anti-affinity, or a fresh node becomes a
  sole copy. *Mitigation:* operator-verified `failure_domain_id` /
  `donor_principal_id` placement (self-declared geo is informational); orthogonal
  `trust_state` with capped `placement_weight` for probationary nodes; probationary
  nodes never sole/second copy of `important` data.
- **Audit collusion / backdating (was D10).** A lying donor satisfies a challenge
  via a co-located cache, a colluding fast peer, or by backdating `completed_at`.
  *Mitigation:* coordinator receive-time deadline (not donor-supplied); synchronous
  single round-trip; repair transport is Ed25519-token-gated and Bitswap-disabled
  so a donor under audit cannot lawfully fetch in-window; honest framing — audits
  prove *timely retrievability under the node identity*, **not** unique physical
  residency.
- **Bandwidth exhaustion (was D11).** A coordinator bug or a malicious schedule
  overshoots a donor's agreed budget, tripping its ISP/provider heuristics.
  *Mitigation:* the **donor's** token-bucket is authoritative and refuses
  over-budget work; tokens carry `max_bytes`; Tier-1 `T1.12` "no doomsday
  override" is enforced at the node that actually sends bytes.
- **Supply-chain against volunteers (extends actor G; was D-sign).** Volunteers
  pull and run a privileged daemon. *Mitigation:* cosign-signed images + SBOM +
  provenance; digest-pinned, signature-verified donor walkthrough; the
  dependency-boundary CI keeps operator-only code out of the donor artifact.

Residual risks (acknowledged, unchanged in spirit): silent coordinator
compromise is still total compromise; possession audits are retrievability
sampling, not proof-of-replication; a determined donor can still copy ciphertext
off-box (it stays computationally opaque without the per-blob key).

### Extended trust boundary

```
 PUBLIC INTERNET ─①TLS─► nginx ─②loopback─► coordinator
                                                │ ③ env/secret-mount master key → (Postgres, Kubo origin)
                                                │ ④ Nebula overlay + HTTPS/mTLS
                                                ▼
                              ┌──────────── donor pinning nodes ───────────┐
                              │ ⑤ donor↔donor repair: mTLS + Ed25519 token │
                              │    (jti single-use, max_bytes, src/dst/gen)│
                              │ ⑥ donor-local token-bucket = authoritative │
                              │    budget enforcement (coordinator only     │
                              │    reserves, best-effort)                   │
                              │ ⑦ possession audit: coordinator-receive-time│
                              │    deadline, synchronous, Bitswap-disabled  │
                              └─────────────────────────────────────────────┘
 Placement anti-affinity uses operator-verified failure_domain_id / donor_principal_id (④),
 NOT self-declared geo. Probationary trust_state caps placement weight (④).
```

## Performance concerns

- **No per-tick full scans.** `HEALING_PROTOCOL` rebuilds replica counts from
  `pin_assignments ⨝ nodes ⨝ blobs` each tick — fine at small scale, costly at
  millions of CIDs every 60 s. Phase 2 maintains a durable
  **`blob_replication_state`** projection updated **in the same transaction** as
  assignment/liveness changes, with affected CIDs **enqueued** on node failure via
  the `pin_assignments(node_id, state)` index. A periodic **full reconciliation**
  remains as a correctness audit. This preserves "DB authoritative, in-memory
  disposable" without continuous whole-table aggregation.
- **Repair fan-out is bounded.** When a large donor goes `unreachable`, every CID
  it held is enqueued (via the node index), not discovered by a global scan;
  healing uses bounded batches, dedup, assignment leases, per-source/per-dest
  concurrency caps, and backpressure.
- **`blob_blocks` growth.** At 256 KiB/block, 1 TiB ≈ 4.2 M leaf rows; a 2.4 TiB
  corpus ≈ 9.8 M rows before replica metadata. Possession audits depend on this
  table. P2-M5/M6 **benchmark** manifest insert throughput, random-challenge
  selection, delete cascades, and backup/restore at that scale; compressed
  manifests / CAR indexes are a later option if needed.
- **v2 must be streaming end-to-end.** A streaming codec wrapped by whole-buffer
  `envelope.Encrypt` and `ipfs.AddDeterministic` is not actually streaming. M8–M10
  evolve storage to `Importer(io.Reader, expectedSize)`, `StreamingEncoder.EncryptTo`,
  and `RangeDecrypter.OpenRange` with bounded memory and explicit concurrency.
- **Audit cost stays tiny.** ~256 KiB egress + a sha256 per challenge; at hourly
  base cadence ≈ 6 MiB/day per donor (≈0.012 % of a 50 GB/day budget). Size-weighted
  sampling raises cadence for large nodes while keeping the per-donor fraction small.

## Forward-compatibility with post-1.0 HA/peering

The second-pass resilience analysis
([`../phase6/2026-06-12-resilience-and-post-1.0-architecture-design.md`](../phase6/2026-06-12-resilience-and-post-1.0-architecture-design.md))
promoted two items out of the research grab-bag into deliberate **post-1.0**
phases — multi-coordinator *single-authority* HA (Phase 6) and opaque
inter-federation *ciphertext-only* peering (Phase 7). The single-coordinator
architecture remains correct and sufficient for the **entire 1.0 line**; Phase 2
builds none of that machinery. But several Phase 2 structures are *also* the
Phase 6/7 prerequisites, so Phase 2 designs them HA-compatibly now to avoid
accidental rework later.

**Governing rule.** Phase 2 MAY introduce structures it needs for its own
correctness that *also* serve Phase 6/7; it MUST NOT introduce structures used
**only** by Phase 6 runtime logic. The former are built; the latter are named
here as additive future work and left unbuilt.

**Phase 2 structures that double as Phase 6/7 prerequisites (built now):**

| Phase 2 structure | Phase 2 purpose | Post-1.0 role |
|---|---|---|
| `pin_assignments.assignment_id` + `generation` (D6) | safe ack/fail under superseded assignments | the immutable handle multi-endpoint donor failover keys on (Phase 6) |
| `pin_changes` change-log + `since_seq` / `snapshot_required` (D7) | durable incremental assignment sync | the cursor a donor preserves when failing over between coordinators (Phase 6) |
| operator-verified `failure_domain_id` / `donor_principal_id` (D8) | placement anti-affinity + Sybil resistance | the unit a Phase 7 peer counts as "at most one failure domain"; feeds concentration alerting |
| acked-only durability (D5) | correct Tier-1 safety | the durability semantics HA replicas and peer custodians must preserve |

**Phase 6/7-only work — named, NOT built in Phase 2:**

- **Control-plane / job-queue fencing.** `internal/jobs/queue.go` `Complete`/`Fail`
  guard only on `state='leased'` (no lease-owner/generation token). Contained
  *today* (one queue per coordinator process; idempotent handlers). Phase 6 adds
  `jobs.lease_id`/`lease_generation` + `coordinator_leases(subsystem, holder,
  term, expires_at)` **additively**; every control mutation and issued repair
  token then carries the current term. **Phase 2 obligation:** do not deepen the
  gap — the orchestrator stays single-leader-per-process, and repair tokens
  already carry per-assignment `generation`, forward-compatible with a future
  control term.
- **Origin-location tracking** (`origin_locations(cid, coordinator_id, state,
  failure_domain)` + a transactional outbox for the non-atomic
  Kubo-pin/Postgres-commit boundary) — Phase 6. Phase 2 keeps the single-origin
  model and only notes that boundary as a known residual.
- **Multi-endpoint donor config + failover, replicated/shared upload staging,
  cross-instance signed-URL revocation, shared ingress rate-limiting, redundant
  Nebula lighthouses + Kubo bootstrap peers** — all Phase 6; per-process in
  Phase 2 by design.
- **Opaque inter-federation peering** (`peer/v1`, immutable `home_federation_id`,
  generation-ordered tombstones, encrypted DR packages) — Phase 7. Peering
  replicates **bytes, not authority**.

The Tier-1 reframes `T1.27` (one logical authoritative history; fenced
multi-replica serving allowed) and `T1.28` (opaque-ciphertext-only peering) are
already in `ARCHITECTURE_DECISIONS.md`; P2-M0 does not re-edit them.

## Milestone breakdown

Mirrors Phase 1: the master plan details P2-M0 + P2-M1; later milestones get
their own design/plan pairs at the start of each milestone. "Tier-1?" flags a
formal spec amendment.

| ID | Theme | Exit criterion (summary) | Tier-1? |
|---|---|---|:--:|
| **P2-M0** | **Spec reconciliation** | FED/HEAL/POSS + `DATA_MODEL` + `ARCHITECTURE_DECISIONS` amended for D1,D4–D11,D-cap,D-sign; identity-from-mTLS + capability negotiation specified. **No production code.** | **Yes** |
| P2-M1 | Build/repo separation | `cmd/node` + `internal/node/*` + `internal/federation/wire` skeleton; `node.Dockerfile`; `deploy/donor/*`; dependency-boundary CI green; donor SBOM/sign pipeline; `nova-node` builds and validates config. **No live federation.** | — |
| P2-M2 | Identity, registration, capability negotiation | Nebula sidecar path; mTLS federation identity; stable `node_id` from verified cert; register/heartbeat; cert rotation/revocation; protocol + capability selection (fail-closed); `trust_state` assigned. | — |
| P2-M3 | Assignment synchronization | `pin_changes` log + retention; snapshot/epoch recovery + `snapshot_required`; node-local cursor; `assignment_id`/`generation`; idempotent apply; ack/fail/unpin state machine; crash + long-offline recovery tests. | partial |
| P2-M4 | v1 opaque replication vertical slice | coordinator-as-source; streaming transfer; deterministic re-import + root-CID compare; donor storage limit; **authoritative donor budget**; **Ed25519** repair grants — first real federation release over v1 envelopes. | — |
| P2-M5 | Liveness & healing | 5-state liveness; unreachable-triggered repair; strict Tier-1 (acked-only); durable reservations; **failure-domain anti-affinity**; `blob_replication_state` projection; mass-casualty/slow-attrition webhooks; chaos tests. | — |
| P2-M6 | Possession audits & reputation | synchronous challenge-response; coordinator receive-time; size/risk-weighted sampling; probation graduation; audit-aware placement; collusion/replay tests. | — |
| P2-M7 | Production hardening & donor release | signed images + SBOM + provenance; N−1 compat + mixed-version tests; revocation drills; provider-loss sims; disk-full / corrupt-state recovery; graceful decommission; final volunteer docs. **Stable federation release even if v2 is still cooking.** | — |
| P2-M8 | Authoritative streaming-envelope design | settle D2 (record/DAG layout) + D3 (AAD); exact Range mapping; corruption semantics; max object/chunk counts; `blob_blocks` scalability; golden vectors; fuzz; crypto review. | **Yes** |
| P2-M9 | Streaming write path | streaming encoder + import + finalization; bounded memory; manifest gen; mixed v1/v2; "emit v2 for new uploads" rollout flag. | — |
| P2-M10 | Range read path | authenticated partial decryption; `206`/`416`/corruption semantics; transforms over v2 originals; large-object benchmarks; mixed-version integration. | — |

## Exit criteria (phase)

1. A volunteer can `docker pull` a **signed, digest-pinned** `nova-node`, join
   the operator's Nebula mesh with operator-issued certs, and have the coordinator
   place pins on it; the donor pins opaque v1 ciphertext, verifies by re-import +
   root-CID, and acks.
2. Killing a donor transitions it to `unreachable` within the SLA window and the
   orchestrator restores **acked-only** Tier-1 replication without exceeding any
   donor's authoritative budget, respecting failure-domain anti-affinity.
3. Possession audits detect a lying donor (404 / hash-mismatch / receive-time
   deadline) and degrade its reputation; probationary nodes never become a sole
   copy of `important` data.
4. The donor build graph provably excludes operator-only packages
   (dependency-boundary CI) and ships as a minimal signed image.
5. (v2, independently gateable) a large object uploaded under `NOVE v2` Range-serves
   a `206` with bounded coordinator memory, v1 and v2 blobs coexist, and donors
   replicate both without knowing the envelope version.
6. Every Tier-1 amendment (D1, D2/D3, D5) has a spec v-bump, an implementation
   gate, and an updated `ARCHITECTURE_DECISIONS.md` row; no normative doc still
   states a superseded claim.

## Cross-references

- Normative contracts (amended in P2-M0/M8): `docs/specs/FEDERATION_PROTOCOL.md`,
  `ENCRYPTION_ENVELOPE.md`, `HEALING_PROTOCOL.md`, `POSSESSION_AUDIT.md`,
  `DATA_MODEL.sql`, `ARCHITECTURE_DECISIONS.md`, `KUBO_HARDENING.md`,
  `IPFS_IMPORT_RULES.md`.
- Threat model amendment: `docs/THREAT_MODEL.md` § "Phase 2 amendment".
- Donor operations: `docs/VOLUNTEER_DEPLOYMENT_GUIDANCE.md`,
  `docs/quickstart/donor.md`.
- Empirical: `simulations/orchestrator_resilience.py`; calibrated-hybrid
  `novasim` + post-1.0 resilience design:
  [`../phase6/2026-06-12-resilience-and-post-1.0-architecture-design.md`](../phase6/2026-06-12-resilience-and-post-1.0-architecture-design.md).
- P2-M0 (spec reconciliation) design + plan:
  [`2026-06-13-phase2-m0-spec-reconciliation-design.md`](2026-06-13-phase2-m0-spec-reconciliation-design.md),
  [`../../plans/phase2/2026-06-13-phase2-m0-spec-reconciliation.md`](../../plans/phase2/2026-06-13-phase2-m0-spec-reconciliation.md).
- Implementation plan (master): [`../../plans/phase2/2026-06-11-phase2-federation.md`](../../plans/phase2/2026-06-11-phase2-federation.md).
