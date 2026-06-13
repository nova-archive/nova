# Phase 2 — Federation + Streaming-AEAD Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. **P2-M0 is documentation-only and gates all code milestones — do not start P2-M1 until P2-M0's spec amendments are merged.**

**Goal:** Make Nova's data durable across volunteer-run **donor nodes** over a private Nebula mesh — split the donor into its own signed binary, bring up bandwidth-budgeted healing and possession audits, and (independently) lift the envelope to streaming-AEAD for Range-serving — after first **reconciling the contradictory Tier-1 specs** that the protocol-first Phase 0 left behind.

**Architecture:** One Go module (`github.com/nova-archive/nova`), donor-extractable: a separate `cmd/node` product with a mechanically-enforced dependency boundary, a coordinator-side `internal/federation` + `internal/orchestrator` + `internal/audit/possession`, mTLS-over-Nebula transport, Ed25519 repair tokens, and a durable replication-state projection. See the design doc for the authoritative architecture and the D1–D14 reconciliation backlog: [`../../specs/phase2/2026-06-11-phase2-federation-design.md`](../../specs/phase2/2026-06-11-phase2-federation-design.md).

**Tech Stack:** Go (per `go.mod`, `go 1.26.2`), Postgres 16, embedded Kubo (boxo/kubo coreapi), `crypto/ed25519` (stdlib repair tokens), `github.com/ipld/go-car/v2` (already a dep; transfer/DAG verification), Nebula run as a **host/sidecar process** (no in-binary nebula dependency; `nova-node` needs no `NET_ADMIN`), pgx/v5 + sqlc + goose, chi router, cosign + syft (image signing + SBOM; new CI tooling), testcontainers-go, docker compose v2.

---

## Plan structure

This master plan summarizes all milestones with goals and exit criteria. **Only P2-M0 and P2-M1 are expanded into bite-sized tasks here.** Each subsequent milestone gets its own detailed design+plan pair saved to `docs/superpowers/{specs,plans}/phase2/` at the start of that milestone (the Phase 1 cadence).

| Milestone | Theme | Status | Plan | Tier-1? |
|---|---|---|---|:--:|
| P2-M0 | Spec reconciliation (docs only) | in progress | [dedicated design](../../specs/phase2/2026-06-13-phase2-m0-spec-reconciliation-design.md) + [plan](2026-06-13-phase2-m0-spec-reconciliation.md) (authoritative); summary § P2-M0 | **Yes** |
| P2-M1 | Build / repo separation | pending | this document, § P2-M1 | — |
| P2-M2 | Identity, registration, capability negotiation | pending | tbd | — |
| P2-M3 | Assignment synchronization | pending | tbd | partial |
| P2-M4 | v1 opaque replication vertical slice | pending | tbd | — |
| P2-M5 | Liveness & healing | pending | tbd | — |
| P2-M6 | Possession audits & reputation | pending | tbd | — |
| P2-M7 | Production hardening & donor release | pending | tbd | — |
| P2-M8 | Authoritative streaming-envelope design | pending | tbd | **Yes** |
| P2-M9 | Streaming write path | pending | tbd | — |
| P2-M10 | Range read path | pending | tbd | — |

The federation release (P2-M7) is shippable independent of the v2 envelope work (P2-M8–M10).

---

## Milestone summaries

### P2-M0 — Spec reconciliation
**Goal:** Every normative spec a conforming `cmd/node`/`internal/federation` must obey is internally consistent and implementable; each Tier-1 change has a version bump + an implementation gate + an updated `ARCHITECTURE_DECISIONS.md` row.
**Deliverables:** amendments to `FEDERATION_PROTOCOL.md` (Ed25519 tokens D1, re-import verification D4, change-log + fail-closed kinds D7, donor-authoritative budgets D11, capability negotiation + identity-from-mTLS D-cap), `HEALING_PROTOCOL.md` (acked-only durability D5, failure-domain anti-affinity D8, trust/probation D9, reservations D11), `POSSESSION_AUDIT.md` (receive-time + synchronous + weighted sampling D10), `DATA_MODEL.sql` deltas (D6, D7, D8, D9, D10 + `blob_replication_state`), `ARCHITECTURE_DECISIONS.md` Tier-1 edits (`T1.7`/`T1.7a` flagged pending-M8, `T1.10` HMAC→Ed25519, reconcile `T1.14`), and the `THREAT_MODEL.md` Phase 2 amendment + release-signing promotion (D-sign). **No production code.**
**Exit:** an internal-consistency sweep finds no doc claiming an HMAC repair token, `hash(bytes)==cid`, chunk-N==block-N (except as an explicitly M8-pending item), pending-toward-durability, or donor-supplied audit deadline; every amended spec carries a version bump.

### P2-M1 — Build / repo separation
**Goal:** `nova-node` builds and runs (config-validate + health only) as a separate, minimal, signed image whose dependency graph provably excludes operator-only code.
**Deliverables:** `cmd/node/main.go`; `internal/node/{agent,state,transfer,bandwidth,audit}` skeletons; `internal/federation/wire` shared types; `docker/node.Dockerfile` (no libvips/Node/migrate/novactl/master-key); `deploy/donor/{compose.yaml,node.yaml.example}`; a **dependency-boundary** CI job (`go list -deps ./cmd/node` allowlist); a donor **SBOM + cosign signing** CI job; `node.yaml` schema + refuse-to-start validation.
**Exit:** `go build ./cmd/node` succeeds; the boundary CI job fails if `cmd/node` imports any operator-only root; `nova-node --validate` rejects a malformed `node.yaml`; the donor image builds and is signed + SBOM'd in CI; **no live federation yet**.

### P2-M2 — Identity, registration, capability negotiation
**Goal:** A donor with operator-issued Nebula + federation certs registers, negotiates `fed/v1` + capabilities, and heartbeats; identity derives from the verified mTLS cert.
**Deliverables:** Nebula sidecar bring-up doc + compose; `internal/federation/transport` (mTLS-over-Nebula client/server); `POST /fed/v1/register` + `/heartbeat` (coordinator); capability negotiation + fail-closed; cert rotation + `novactl node revoke`; `trust_state` assignment on first registration.
**Exit:** integration test — register → heartbeat loop; incompatible capability set → clear registration failure; revoked cert → next mTLS handshake fails; new node lands `trust_state='probationary'`.

### P2-M3 — Assignment synchronization
**Goal:** A donor syncs assignments via the durable change log, recovers via snapshot after a long offline period, and applies idempotently with generations.
**Deliverables:** `pin_changes` table + retention; `GET /fed/v1/pins/changes` + `/snapshot` + `snapshot_required`; node-local cursor store; `assignment_id`/`generation` end-to-end; ack/fail/unpin conditional state machine.
**Exit:** integration test — apply a change stream; kill+restart the donor mid-stream (idempotent resume); expire the cursor past retention → `snapshot_required` → snapshot recovery; a delayed ack for a superseded generation is rejected.
**Forward-compat (post-1.0):** `assignment_id`/`generation` + the `pin_changes` log are also the Phase 6 multi-endpoint donor-failover prerequisites (a donor preserves its `since_seq` cursor across coordinators) — design them HA-compatibly; build no Phase-6 failover logic. See the phase6 resilience design.

### P2-M4 — v1 opaque replication vertical slice
**Goal:** The coordinator places a v1 blob on a donor, which fetches from the coordinator-as-source, verifies by re-import + root-CID, pins, and acks — the first real federation release.
**Deliverables:** coordinator-as-source `GET /fed/v1/blob/{cid}`; `internal/node/transfer` streaming fetch + deterministic re-import + root-CID compare (D4); donor storage-limit enforcement; `internal/node/bandwidth` authoritative token-bucket (D11); `internal/federation/tokens` Ed25519 mint+verify (D1); donor↔donor repair fetch.
**Exit:** integration test — upload → assignment → donor pins a byte-identical CID via re-import verification → ack; an over-budget assignment is refused by the donor; a forged/replayed token is rejected.

### P2-M5 — Liveness & healing
**Goal:** Donor loss triggers acked-only Tier-1 healing within the SLA, never exceeding budgets, respecting failure-domain anti-affinity.
**Deliverables:** 5-state liveness reconciliation; `internal/orchestrator` tick loop over `blob_replication_state`; unreachable-triggered enqueue (node index); strict Tier-1 (acked-only, D5); durable in-flight reservations; failure-domain anti-affinity placement (D8); mass-casualty + slow-attrition webhooks; chaos tests; sim alignment.
**Exit:** integration/chaos test — kill a multi-CID donor → Tier-1 cleared within target, no budget exceeded, replicas land in distinct failure domains; `federation.degraded`/`shrinking` fire correctly.
**Placement-weight calibration (D8):** this milestone **calibrates the steady-state placement-weight formula** (direction set in P2-M0: decoupled from bandwidth, `~sqrt(free)×trust` + soft anti-affinity; bandwidth = repair-source only) and **emits the concentration metrics** (pin-incidence Gini + per-dimension largest-share / top-k / entropy) feeding the Tier-2 `federation.concentrated`/`federation.homogeneous` alerts. Alert, not prevent.

### P2-M6 — Possession audits & reputation
**Goal:** Audits detect lying donors and gate placement; probationary nodes graduate on evidence.
**Deliverables:** `internal/audit/possession` synchronous challenge-response (D10); coordinator receive-time deadline; size/risk-weighted sampling; reputation + `trust_state` graduation; audit-aware placement; collusion/replay tests.
**Exit:** integration test — a donor that drops a block fails its next audit (404/hash-mismatch) and is excluded from new assignments; backdated `completed_at` does not pass; a probationary node graduates after age + passed audits.

### P2-M7 — Production hardening & donor release
**Goal:** A volunteer-ready, signed, digest-pinned `nova-node` release with operational runbooks.
**Deliverables:** cosign signatures + SBOM + provenance for both images; N−1 + mixed-version compat tests; revocation + provider-loss drills; disk-full / corrupt-state recovery; graceful decommission/drain; final `docs/quickstart/donor.md` + volunteer guide.
**Exit:** a fresh host pulls a **digest-pinned, signature-verified** `nova-node`, joins, serves, and decommissions gracefully; N−1 coordinator/donor interoperate.

### P2-M8 — Authoritative streaming-envelope design
**Goal:** A buildable, reviewed `NOVE v2` spec resolving D2/D3 with golden vectors.
**Deliverables:** chosen record/DAG layout (D2); header-commitment AAD (D3); exact Range→block mapping; corruption semantics; max object/chunk counts; `blob_blocks` scalability benchmark; golden test vectors; parser/range fuzzing; focused crypto review; `ENCRYPTION_ENVELOPE.md` + `ARCHITECTURE_DECISIONS.md` `T1.7`/`T1.7a` amendments.
**Exit:** golden vectors committed; fuzz green; crypto review signed off; Tier-1 envelope amendments merged.

### P2-M9 — Streaming write path
**Goal:** New uploads can be written as `NOVE v2` with bounded memory, coexisting with v1.
**Deliverables:** `StreamingEncoder.EncryptTo`; `Importer(io.Reader, expectedSize)`; streaming finalization; manifest generation for v2; mixed v1/v2 storage; "emit v2 for new uploads" rollout flag.
**Exit:** integration test — a multi-GiB upload encrypts+imports under bounded memory; v1 and v2 blobs coexist and both decrypt.

### P2-M10 — Range read path
**Goal:** v2 blobs Range-serve correctly; donors remain envelope-agnostic.
**Deliverables:** `RangeDecrypter.OpenRange`; authenticated partial decryption; `206`/`416`/corruption semantics; HEAD; transforms over v2 originals; large-object benchmarks; mixed-version integration.
**Exit:** integration test — `Range: bytes=A-B` on a v2 blob returns a correct `206` decrypting only covering records; a corrupt record fails closed; donors replicate v2 without knowing the version.

---

## P2-M0 — Spec Reconciliation: Detailed Tasks

> **Superseded by the dedicated P2-M0 plan** (2026-06-13):
> [`2026-06-13-phase2-m0-spec-reconciliation.md`](2026-06-13-phase2-m0-spec-reconciliation.md),
> which expands these tasks and folds in the post-1.0 future-proofing from the
> phase6 resilience design. The summary below is kept for historical context;
> the dedicated plan is the authoritative task list.
>
> Documentation-only. Each amended spec gets a **version bump** in its `Status:` line and a one-line changelog entry referencing this design. Cross-check every edit against the design doc's reconciliation table.

### Files for P2-M0

#### Modified
| Path | Why |
|---|---|
| `docs/specs/FEDERATION_PROTOCOL.md` | D1 (Ed25519 tokens), D4 (re-import verify), D7 (change log + fail-closed kinds), D11 (donor-authoritative budget), D-cap (capability negotiation, identity-from-mTLS) |
| `docs/specs/HEALING_PROTOCOL.md` | D5 (acked-only Tier-1), D8 (failure-domain anti-affinity), D9 (trust/probation), D11 (reservations), projection note |
| `docs/specs/POSSESSION_AUDIT.md` | D10 (receive-time, synchronous, weighted sampling) |
| `docs/specs/DATA_MODEL.sql` | schema deltas D6, D7, D8, D9, D10 + `blob_replication_state` (as Phase 2 commentary; the live DDL ships as a new migration in P2-M3) |
| `docs/specs/ARCHITECTURE_DECISIONS.md` | Tier-1 `T1.10` HMAC→Ed25519; `T1.7`/`T1.7a` marked "v2 layout pending M8"; reconcile `T1.14` wording; new rows for failure-domain + trust |
| `docs/specs/ENCRYPTION_ENVELOPE.md` | D12 (CDN wording); flag D2/D3 as M8-authoritative, remove the "chunk N == block N" guarantee from the settled section |
| `docs/THREAT_MODEL.md` | Phase 2 amendment (handled in the dedicated threat-model task; cross-link here) |

### Task 1: Federation protocol amendments
- [ ] Replace the "HMAC-signed" repair-token language with **Ed25519**: coordinator holds the private key; donors receive the public key via `config_updates`. Specify the full claim set (`jti`, `assignment_id`, `generation`, `cid`, `source_node_id`, `dest_node_id`, `not_before`, `not_after`, `max_bytes`, `protocol_version`) and **single/bounded-use** enforcement via a source-side `jti` replay cache.
- [ ] Replace `hash(bytes) == cid` verification with **deterministic re-import via `IPFS_IMPORT_RULES` and root-CID comparison** (or CAR + full-DAG verify); note `go-car/v2` availability.
- [ ] Specify the durable **`pin_changes`** log backing `since_seq`/`current_epoch`, its retention, and the machine-readable **`snapshot_required`** response when `since_seq` predates retention.
- [ ] Change "new change-log `kind` values are backward-compatible" to **fail-closed + capability-negotiated**.
- [ ] Add **capability negotiation** to `register` (`supported_protocols`, `capabilities` → `selected_protocol`, `required_capabilities`, clear failure on no overlap) and state that **identity derives from the verified mTLS cert**, not request JSON.
- [ ] State that the **donor's local token-bucket is the authoritative** budget enforcer; the coordinator only reserves (best-effort); tokens carry `max_bytes`.
- [ ] Bump `Status:` to a new FED version; add a changelog line citing this design.

### Task 2: Healing protocol amendments
- [ ] Make **durability classification acked-only** (Tier-1 trigger = CIDs with `acked_count < 2`); demote `pending` to a separate **in-flight reservation** used only for scheduling dedup; clarify `pending_weight` cannot affect the Tier-1 trigger.
- [ ] Add **failure-domain anti-affinity** to placement (prefer/require distinct `failure_domain_id`; self-declared geo is informational).
- [ ] Add the orthogonal **`trust_state`/probation** model with `placement_weight` cap and the "never sole/second copy of `important`" rule.
- [ ] Replace any "full scan each tick" with the durable, rebuildable **`blob_replication_state`** projection + periodic reconciliation.
- [ ] Bump version + changelog.

### Task 3: Possession audit amendments
- [ ] Deadline uses **coordinator receive-time**, not donor `completed_at`.
- [ ] Specify the **synchronous** challenge→response (single round-trip) as the primary design; keep the async form only as a documented fallback.
- [ ] Make sampling **weighted by stored bytes / pin count / node age / risk**.
- [ ] Tighten the "what this proves" framing (timely retrievability under the node identity, not unique physical residency).
- [ ] Bump version + changelog.

### Task 4: Data model + architecture-decisions amendments
- [ ] Add Phase 2 schema commentary to `DATA_MODEL.sql`: `pin_assignments.assignment_id`/`generation`; `pin_changes`; `blob_replication_state`; `nodes.donor_principal_id`/`failure_domain_id`/`provider`/`asn`/`operator_verified_at`/`trust_state`/`placement_weight`; `pin_audits` receive-time + sampling-weight columns. (Note: live DDL ships as a new migration in P2-M3; Phase 1 migrations remain frozen.)
- [ ] Amend `ARCHITECTURE_DECISIONS.md` Tier-1: `T1.10` HMAC→Ed25519; mark `T1.7`/`T1.7a` "v2 record/DAG layout + AAD authoritative in M8"; reconcile `T1.14` to acked-only; add rows for failure-domain placement and trust/probation. Follow the Tier-1 amendment process (v-bump + implementation-gate note).
- [ ] Amend `ENCRYPTION_ENVELOPE.md` § v2: reframe the CDN benefit (D12); remove "chunk N == block N" from the settled claims, pointing to M8.

### Task 5: Consistency sweep
- [ ] Grep the repo for superseded claims (`HMAC` repair token, `hash(bytes)`, `chunk N == block N`, `0.5 * pending`, donor `completed_at` deadline) and confirm each is corrected or explicitly marked M8-pending.
- [ ] Run `python3 scripts/check_doc_links.py docs` and confirm no new broken links.

---

## P2-M1 — Build / Repo Separation: Detailed Tasks

> First code milestone. Goal is a **minimal, boundary-enforced, signed** donor product — no live federation. Commit style follows the repo convention (`git commit -s`, Co-Authored-By trailer).

### Files for P2-M1

#### Created
| Path | Responsibility |
|---|---|
| `cmd/node/main.go` | donor entrypoint: load+validate `node.yaml`, health, (stub) agent start |
| `internal/node/agent/agent.go` | register→heartbeat→sync loop skeleton (no-op transport in M1) |
| `internal/node/state/store.go` | local cursor/cert/replay store interface (file or embedded KV; **no Postgres**) |
| `internal/node/bandwidth/bucket.go` | authoritative token-bucket skeleton |
| `internal/node/transfer/transfer.go` | streaming fetch + re-import verify interface (stub in M1) |
| `internal/node/audit/responder.go` | possession-challenge responder interface (stub in M1) |
| `internal/federation/wire/wire.go` | shared protocol + token + capability types (imported by both binaries) |
| `internal/config/node.go` | `node.yaml` schema + refuse-to-start validation |
| `docker/node.Dockerfile` | minimal donor image (no libvips/Node/migrate/novactl/master-key/Postgres) |
| `deploy/donor/compose.yaml` | donor-only compose (Nebula sidecar + nova-node; no public ports; read-only rootfs; cap_drop ALL) |
| `deploy/donor/node.yaml.example` | annotated donor config |
| `scripts/check_node_deps.sh` | `go list -deps ./cmd/node` allowlist enforcement |

#### Modified
| Path | Why |
|---|---|
| `.github/workflows/ci.yml` | add `donor-deps-boundary`, `donor-build`, `donor-sbom-sign` jobs |
| `docker/coordinator.Dockerfile` | rename/split from the existing single `Dockerfile` so coordinator and donor build independently |
| `Makefile` | `node-build`, `node-validate`, `node-deps-check`, `node-image` targets |

### Task 1: Donor binary skeleton
- [ ] `cmd/node/main.go`: parse flags (`--config`, `--validate`), load `node.yaml` via `internal/config.ResolveSecret` chain, run health endpoint on the Nebula iface only.
- [ ] `internal/config/node.go`: schema (coordinator URL, cert paths, swarm key path, storage dir, `bandwidth_budget_bytes_per_day`, `failure_domain` hints) + refuse-to-start on missing certs / swarm key / budget.
- [ ] `nova-node --validate` exits non-zero on a malformed config (table-driven test).

### Task 2: Shared wire package + node skeletons
- [ ] `internal/federation/wire`: request/response structs for register/heartbeat/changes/snapshot/ack/fail, the Ed25519 token claim struct + `Verify`, capability identifiers, normalized error codes. Pure types + verification only — no operator deps.
- [ ] Stub `internal/node/{agent,state,bandwidth,transfer,audit}` with interfaces + table-driven unit tests; transport is a no-op until M2.

### Task 3: Dependency boundary (the load-bearing gate)
- [ ] `scripts/check_node_deps.sh`: run `go list -deps ./cmd/node`, fail if the graph contains any disallowed root (`internal/masterkey|moderation|auth|setup|db`, `internal/api/handlers/admin*`, `nova-image`, `pkg/coordinator`). Prefer an **allowlist** of expected roots.
- [ ] Add a `donor-deps-boundary` CI job that runs it; add a deliberately-failing fixture test in review to prove it catches a violation.

### Task 4: Minimal donor image + separate builds
- [ ] `docker/node.Dockerfile`: multi-stage; runtime contains only `nova-node` + CA certs + (optional) minimal health tool; non-root; read-only rootfs; `cap_drop: [ALL]`; `no-new-privileges`; writable data volume only.
- [ ] Split the existing `docker/Dockerfile` into `coordinator.Dockerfile` (unchanged contents) so the two images build independently; update `make docker-build` + smoke references.
- [ ] `deploy/donor/compose.yaml`: Nebula sidecar (host or shared-netns) + nova-node; **no published ports**; volumes for state + ciphertext data.

### Task 5: Supply-chain (promote D-sign into Phase 2)
- [ ] `donor-sbom-sign` CI job: build the donor image, generate an SBOM (syft), sign with cosign (keyless/OIDC), attach provenance (GitHub artifact attestation).
- [ ] Document digest-pinning + signature verification for operators/volunteers (feeds the P2-M7 docs and the volunteer guide).

### Testing (M1)
- [ ] Unit: `node.yaml` validation table; `wire` token verify (valid / expired / wrong-key / replay); bandwidth bucket arithmetic.
- [ ] CI: `donor-deps-boundary` green on `main`, red on the injected-violation fixture; `donor-build` produces an image; `donor-sbom-sign` emits SBOM + signature.
- [ ] `go build ./...` and `go vet ./...` stay green; existing coordinator smoke unaffected.

## Gotchas and mitigations

- **Nebula is not a Go dependency.** Run it as a host/sidecar process so `nova-node` needs no `NET_ADMIN`; the donor binds to the Nebula interface address discovered from config. Do not pull a Nebula library into `cmd/node` (would bloat the artifact and the boundary).
- **Frozen migrations.** Phase 2 schema changes ship as **new** migration files; never edit `internal/db/migrations/000*` (the `migrations-frozen` gate). sqlc regen (`make sqlc-generate`) only after adding the new queries.
- **Dependency boundary is easy to regress.** Any accidental `internal/db` or `pkg/coordinator` import from a shared helper re-pollutes the donor graph — the CI job must be **blocking**, not advisory.
- **P2-M0 is a hard gate.** Writing `internal/federation` against the un-amended specs would bake in the HMAC-token and `hash(bytes)==cid` bugs. Merge P2-M0 first.
- **v2 is not a drop-in.** Whole-buffer `envelope.Encrypt` + `ipfs.AddDeterministic` must become streaming (M9) before v2 is real; do not let M8's "drop-in" framing from the Phase 1 envelope spec mislead estimation.
