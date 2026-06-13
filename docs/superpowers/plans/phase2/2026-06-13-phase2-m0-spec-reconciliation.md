# P2-M0 — Spec Reconciliation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (this is a documentation-only, inline milestone) or superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax for tracking. **P2-M0 gates all Phase 2 code milestones — do not start P2-M1 until these amendments are merged and the consistency sweep passes.**

**Goal:** Reconcile the contradictory Phase 0 normative federation/healing/possession/envelope specs so a conforming `cmd/node`/`internal/federation` can be built, and fold the second-pass resilience findings into the Phase 2 docs as post-1.0 future-proofing — **documentation only, no production code.**

**Architecture:** Edit the cross-phase normative contracts in `docs/specs/` (+ `docs/THREAT_MODEL.md`), each with a `Status:` version bump and a changelog line; complete the half-done resilience-doc relocation to `phase6/` and repoint its stale cross-references; add a forward-compatibility record to the Phase 2 master design. Per the design's governing rule: introduce structures Phase 2 needs that *also* serve Phase 6/7 (assignment generations, change-log, verified failure domains), but build **no Phase-6-only machinery**.

**Tech Stack:** Markdown + SQL-commentary edits only. Verification via `grep`/`rg` for superseded claims and `python3 scripts/check_doc_links.py docs`. No Go, no migrations (live DDL ships in P2-M3; Phase 1 migrations stay frozen).

Design: [`../../specs/phase2/2026-06-13-phase2-m0-spec-reconciliation-design.md`](../../specs/phase2/2026-06-13-phase2-m0-spec-reconciliation-design.md). Master design backlog (D1–D12, D-cap, D-sign): [`../../specs/phase2/2026-06-11-phase2-federation-design.md`](../../specs/phase2/2026-06-11-phase2-federation-design.md).

---

## File map

**Modified — normative contracts (each gets a `Status:` v-bump + changelog line):**

| Path | D-items | Version |
|---|---|---|
| `docs/specs/FEDERATION_PROTOCOL.md` | D1, D4, D7, D11, D-cap | v2 → v3 |
| `docs/specs/HEALING_PROTOCOL.md` | D5, D8, D9, D11, projection | v2 → v3 |
| `docs/specs/POSSESSION_AUDIT.md` | D10 | (unversioned) → v2 |
| `docs/specs/ENCRYPTION_ENVELOPE.md` | D12; D2/D3 mark-M8 | v2 → v3 |
| `docs/specs/ARCHITECTURE_DECISIONS.md` | T1.10, T1.7/T1.7a, T1.14, +2 Tier-1 rows | v3 (table update) |
| `docs/specs/DATA_MODEL.sql` | D6, D7, D8, D9, D10 commentary | commentary only |
| `docs/THREAT_MODEL.md` | reconcile Phase 2 amendment; D-sign | verify/complete |

**Modified — Phase 2 design/index docs + reorg debt:**

| Path | Why |
|---|---|
| `docs/superpowers/specs/phase6/2026-06-12-resilience-and-post-1.0-architecture-design.md` | complete the uncommitted move (rename from `phase2/`) |
| `docs/specs/ARCHITECTURE_DECISIONS.md`, `docs/ROADMAP.md`, `simulations/go/README.md`, `simulations/README.md` | repoint `phase2/2026-06-12-…` → `phase6/2026-06-12-…` |
| `docs/superpowers/specs/README.md` | add Phase 6 section listing the resilience design |
| `docs/superpowers/plans/README.md` | add P2-M0 plan row; note Phase 6 design-only |
| `docs/superpowers/specs/phase2/2026-06-11-phase2-federation-design.md` | add "Forward-compatibility with post-1.0 HA/peering" subsection; cross-ref P2-M0 docs |
| `docs/superpowers/plans/phase2/2026-06-11-phase2-federation.md` | point P2-M0 section at dedicated docs; P2-M3/P2-M5 forward-compat one-liners |

**Not touched:** any `internal/`, `cmd/`, `pkg/` Go; any `internal/db/migrations/*` (frozen); `VOLUNTEER_DEPLOYMENT_GUIDANCE.md` (already corrected 2026-06-11 — verify only).

**Convention for every spec edit:** bump the `Status:` line and add a one-line changelog/amendment note of the form `Amended by P2-M0 (2026-06-13) — see docs/superpowers/specs/phase2/2026-06-13-phase2-m0-spec-reconciliation-design.md`.

---

## Task 1: Complete the resilience-doc relocation + repoint cross-references

**Files:** rename `docs/superpowers/specs/{phase2→phase6}/2026-06-12-resilience-and-post-1.0-architecture-design.md` (already moved on disk, uncommitted); modify `docs/specs/ARCHITECTURE_DECISIONS.md`, `docs/ROADMAP.md`, `simulations/go/README.md`, `simulations/README.md`.

- [ ] **Step 1: Stage the relocation as a rename**

```bash
cd /home/archbug/projects/nova
git add -A docs/superpowers/specs/phase2/2026-06-12-resilience-and-post-1.0-architecture-design.md \
           docs/superpowers/specs/phase6/2026-06-12-resilience-and-post-1.0-architecture-design.md
git status -s   # expect: R  docs/.../phase2/2026-06-12-... -> docs/.../phase6/2026-06-12-...
```

- [ ] **Step 2: Find every stale reference**

Run: `rg -n "specs/phase2/2026-06-12-resilience" docs simulations`
Expected before edit: hits in `ARCHITECTURE_DECISIONS.md`, `ROADMAP.md`, `simulations/go/README.md`, possibly `simulations/README.md`.

- [ ] **Step 3: Repoint each hit** `…/specs/phase2/2026-06-12-…` → `…/specs/phase6/2026-06-12-…` (preserve each file's existing relative-path prefix: `docs/superpowers/specs/phase6/…` in `docs/specs/*` and `docs/ROADMAP.md`; `../../docs/superpowers/specs/phase6/…` in `simulations/go/README.md`).

- [ ] **Step 4: Verify no stale path remains**

Run: `rg -n "phase2/2026-06-12-resilience" docs simulations`
Expected: **no matches.**
Run: `rg -ln "phase6/2026-06-12-resilience" docs simulations`
Expected: `ARCHITECTURE_DECISIONS.md`, `ROADMAP.md`, `simulations/go/README.md` (+ `simulations/README.md` if it referenced it).

- [ ] **Step 5: Commit**

```bash
git add -A docs simulations
git commit -s -m "docs(phase2): complete resilience-doc move to phase6/ and repoint refs

The 2026-06-12 resilience/post-1.0 design was moved phase2/->phase6/ in
the working tree but never committed; all cross-references still pointed
at the old path. Commit the rename and repoint ARCHITECTURE_DECISIONS,
ROADMAP, and the novasim READMEs. P2-M0 housekeeping.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

## Task 2: README index updates (specs + plans)

**Files:** `docs/superpowers/specs/README.md`, `docs/superpowers/plans/README.md`.

- [ ] **Step 1: specs README — add a Phase 6 section** after the Phase 2 table:

```markdown
## Phase 6 — Multi-coordinator single-authority HA (post-1.0, design-only)

Located in [`phase6/`](phase6/). A forward-looking second-pass analysis/design,
not a 1.0 milestone; no implementation plan yet.

| Doc | Scope |
|---|---|
| [phase6/2026-06-12-resilience-and-post-1.0-architecture-design.md](phase6/2026-06-12-resilience-and-post-1.0-architecture-design.md) | Resilience analysis (multiplex decomposition, `novasim` calibrated-hybrid sim) + post-1.0 HA (Phase 6) & opaque peering (Phase 7) architecture; draft Tier-1 reframes |
```

- [ ] **Step 2: specs README — add the P2-M0 row** to the Phase 2 table:

```markdown
| [phase2/2026-06-13-phase2-m0-spec-reconciliation-design.md](phase2/2026-06-13-phase2-m0-spec-reconciliation-design.md) | P2-M0 — spec reconciliation (+ post-1.0 future-proofing) |
```

- [ ] **Step 3: plans README — add the P2-M0 row** to the Phase 2 table and a Phase 6 note:

```markdown
| [phase2/2026-06-13-phase2-m0-spec-reconciliation.md](phase2/2026-06-13-phase2-m0-spec-reconciliation.md) | P2-M0 — spec reconciliation (docs-only gate) |
```

Add below the Phase 2 table: `> Phase 6 (post-1.0 HA) is design-only so far — see the specs index; no plan yet.`

- [ ] **Step 4: Verify links resolve**

Run: `python3 scripts/check_doc_links.py docs`
Expected: no *new* broken links (pre-existing quickstart screenshot placeholders excepted).

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/specs/README.md docs/superpowers/plans/README.md
git commit -s -m "docs(phase2): index the phase6 resilience design + P2-M0 docs

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

## Task 3: `FEDERATION_PROTOCOL.md` amendments (D1, D4, D7, D11, D-cap)

**Files:** `docs/specs/FEDERATION_PROTOCOL.md` (current claims at :247 HMAC, :266/:320 `hash(bytes)==cid`).

- [ ] **Step 1: D1 — Ed25519 repair tokens.** Replace the "HMAC-signed" grant language (line ~247) with **Ed25519 asymmetric** tokens: the coordinator holds the private signing key; donors receive the **public** key via `config_updates`. Specify the claim set: `jti`, `assignment_id`, `generation`, `cid`, `source_node_id`, `dest_node_id`, `not_before`, `not_after` (≤ `repair_token_ttl_seconds`), `max_bytes`, `protocol_version`. State **single/bounded-use** enforcement via a source-side `jti` replay cache (a malicious destination must not replay a valid token to drain the source's budget).

- [ ] **Step 2: D4 — transfer verification.** Replace `hash(bytes) == cid` (lines ~266 and the sequence diagram ~320) with **deterministic re-import via `IPFS_IMPORT_RULES.md` + root-CID comparison** (or receive a CAR and verify every block + the root). Note `github.com/ipld/go-car/v2` is already a dependency. Cross-reference Tier-1 `T1.6`.

- [ ] **Step 3: D7 — durable change log.** Specify the `pin_changes` log backing `since_seq`/`current_epoch`, its retention policy, and the machine-readable **`snapshot_required`** response when a donor's `since_seq` predates retention. Change "new change-log `kind` values are backward-compatible" to **fail-closed + capability-negotiated** (an unknown `kind` for a state-mutating op must not be silently ignored).

- [ ] **Step 4: D-cap — capability negotiation + identity.** Add to `register`: `supported_protocols` (`fed/v1`, …) + `capabilities` (`pin-change-log/v1`, `snapshot/v1`, `repair-stream/v1`, `audit-block-hash/v1`, …); the coordinator replies `selected_protocol` + `required_capabilities` and **fails registration clearly** when no compatible set exists. State that **node identity derives from the verified mTLS cert**, not self-asserted request JSON.

- [ ] **Step 5: D11 — donor-authoritative budget.** State that coordinator scheduling is a **best-effort reservation**; the **donor's local token-bucket is authoritative** and refuses work exceeding its configured budget; repair tokens carry `max_bytes`; actual bytes reconcile via heartbeat/transfer reports. Tie to Tier-1 `T1.12` (enforced at the node that sends bytes).

- [ ] **Step 6: Version bump.** `Status:` Phase 0 v2 → **v3**; add the changelog/amendment line citing the P2-M0 design.

- [ ] **Step 7: Verify superseded claims are gone**

Run: `rg -ni "hmac" docs/specs/FEDERATION_PROTOCOL.md`
Expected: no HMAC *repair-token* claim (any remaining `hmac` must be unrelated, e.g. signed-URL context if present — confirm by reading the hit).
Run: `rg -n "hash\(bytes\)|== hash|hash == cid" docs/specs/FEDERATION_PROTOCOL.md`
Expected: **no matches.**

- [ ] **Step 8: Commit**

```bash
git add docs/specs/FEDERATION_PROTOCOL.md
git commit -s -m "docs(specs): FEDERATION_PROTOCOL v3 — Ed25519 tokens, re-import verify, change-log, capability negotiation, donor-authoritative budget (P2-M0 D1/D4/D7/D11/D-cap)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

## Task 4: `HEALING_PROTOCOL.md` amendments (D5, D8, D9, D11, projection)

**Files:** `docs/specs/HEALING_PROTOCOL.md` (current `effective_count` at :56–63; `pending_weight` tunable at :378; capacity×reputation placement ~:321).

- [ ] **Step 1: D5 — acked-only Tier-1.** Rework the durability classification (lines ~56–63): the **Tier-1 trigger is acked-only** — CIDs with `acked_count < 2` (and `> 0`, per the donor-plane convention) are Tier-1. Demote `pending` to a separate **in-flight reservation** used only to avoid double-scheduling; it **never** lifts a 1-acked CID out of Tier-1. Reword the `pending_weight` tunable (line ~378) to "orders Tier-2 work only; cannot affect the Tier-1 trigger." Delete/replace the `effective_count = acked + 0.5*pending` definition.

- [ ] **Step 2: D8 — anti-affinity + placement-weight direction (the new normative content).** Amend placement to:
  - **MUST apply soft failure-domain anti-affinity** — prefer distinct `failure_domain_id` (then `donor_principal_id`/`provider`/`asn`) when enough domains exist; a **preference, never a veto** (a hard ceiling can block healing into the only surviving capacity during a casualty).
  - **Steady-state replica placement MUST NOT be weighted by donor bandwidth.** Bandwidth governs **repair-source selection only**, not steady-state placement weight.
  - The **exact steady-state weight is a Tier-2 tunable, calibrated in P2-M5** (direction settled — `~sqrt(free_capacity) × trust` with soft anti-affinity — exact form deferred; cite resilience design § 4.3, § 9).
  - `geo_declared` is informational only; anti-affinity uses **operator-verified** `failure_domain_id`.
  - Cross-reference the Tier-2 concentration-alerting set in `ARCHITECTURE_DECISIONS.md` and note the P2-M5 healing/metrics layer MUST emit pin-incidence Gini + per-dimension metrics (alert, not prevent).

- [ ] **Step 2b:** Replace the `capacity * reputation` initial-placement sampling text (~line 321) so it reflects the decoupled-from-bandwidth direction + `trust_state` weighting (not raw capacity/bandwidth).

- [ ] **Step 3: D9 — trust/probation.** Add an **orthogonal `trust_state`** (`probationary`/`trusted`/`suspended`) with a `placement_weight` cap, separate from the 5-state liveness enum and from `reputation_score`. Probationary nodes: capped data volume, higher audit cadence, **never** the sole or second copy of `important`-class data; graduate on age + successful transfers + passed audits.

- [ ] **Step 4: D11 — reservations.** Note the orchestrator schedules **best-effort reservations**; the donor's token-bucket is authoritative (mirror of FED D11).

- [ ] **Step 5: Projection.** Replace any "rebuild replica counts from a full join each tick" with the durable, rebuildable **`blob_replication_state`** projection updated in the same transaction as assignment/liveness changes, plus a periodic full reconciliation as a correctness audit (no per-tick whole-table aggregation).

- [ ] **Step 6: Version bump** Phase 0 v2 → **v3** + changelog line.

- [ ] **Step 7: Verify**

Run: `rg -n "0\.5 ?\* ?pending|effective_count = acked|0\.5 \* pending_count" docs/specs/HEALING_PROTOCOL.md`
Expected: **no match** that lets pending count toward durability (a residual mention is acceptable only if it explicitly says pending does NOT lift Tier-1 — confirm by reading).
Run: `rg -ni "bandwidth" docs/specs/HEALING_PROTOCOL.md`
Expected: remaining bandwidth references are about budgets / repair-source selection, not steady-state placement weight (confirm by reading the hits).

- [ ] **Step 8: Commit**

```bash
git add docs/specs/HEALING_PROTOCOL.md
git commit -s -m "docs(specs): HEALING_PROTOCOL v3 — acked-only Tier-1, failure-domain anti-affinity + placement-weight decoupled from bandwidth, trust/probation, durable projection (P2-M0 D5/D8/D9/D11)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

## Task 5: `POSSESSION_AUDIT.md` amendments (D10)

**Files:** `docs/specs/POSSESSION_AUDIT.md` (donor `completed_at` deadline at :112; schema block ~:200–214).

- [ ] **Step 1: Receive-time deadline.** Change the pass condition from donor-supplied `completed_at < deadline` (line ~112) to **coordinator receive-time** — the coordinator measures when it receives the response; the donor's self-reported timestamp is not trusted for the deadline.

- [ ] **Step 2: Synchronous primary.** Specify a **synchronous** single-HTTP-round-trip challenge→response (coordinator measures latency) as the **primary** design; keep the two-call async form only as a documented fallback.

- [ ] **Step 3: Weighted sampling.** Replace flat per-node sampling with sampling **weighted by stored bytes / pin count / node age / risk**.

- [ ] **Step 4: Framing.** Tighten "what this proves": **timely retrievability under the node identity**, not unique physical residency (a donor cannot lawfully in-window fetch from elsewhere because repair transport is Ed25519-token-gated and Bitswap is disabled).

- [ ] **Step 5: Version.** Add an explicit `Status:` **v2** + changelog line (this spec was previously unversioned).

- [ ] **Step 6: Verify**

Run: `rg -n "completed_at < deadline|donor.?supplied|completed_at" docs/specs/POSSESSION_AUDIT.md`
Expected: `completed_at` may persist as a recorded column, but the **deadline decision uses coordinator receive-time** — confirm no remaining text makes the pass/fail decision on donor `completed_at`.

- [ ] **Step 7: Commit**

```bash
git add docs/specs/POSSESSION_AUDIT.md
git commit -s -m "docs(specs): POSSESSION_AUDIT v2 — coordinator receive-time deadline, synchronous challenge, weighted sampling (P2-M0 D10)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

## Task 6: `ENCRYPTION_ENVELOPE.md` amendments (D12; mark D2/D3 M8-authoritative)

**Files:** `docs/specs/ENCRYPTION_ENVELOPE.md` (chunk==block at :423; v2 section ~:410–460).

- [ ] **Step 1: D12 — reframe the CDN claim.** Replace "CDN edges store and serve individual ciphertext chunks" with **bounded-memory authenticated Range decryption at the origin** (the coordinator fetches + decrypts only the records covering the range, returns a plaintext `206`). Note a ciphertext-caching edge would require a Nova-aware intermediary and is **not** the default.

- [ ] **Step 2: Remove the settled "chunk N == block N" guarantee** (line ~423) from the v2 section. Mark the v2 **record/DAG layout (D2)** and **per-chunk AAD construction (D3)** as **authoritative in P2-M8** (with golden vectors + crypto review), not settled here. Note the circular-CID-AAD problem is resolved in M8 via a header-commitment, not the final CID.

- [ ] **Step 3: Version** Phase 0 v2 → **v3** + changelog line. (Leave the v1 single-shot format and the `T1.2` version-dispatch seam unchanged.)

- [ ] **Step 4: Verify**

Run: `rg -ni "chunk n == block n|chunk-n.*block-n" docs/specs/ENCRYPTION_ENVELOPE.md`
Expected: **no match** in a settled/normative claim (any surviving mention must be explicitly "P2-M8-authoritative / pending").
Run: `rg -ni "cdn.*ciphertext|ciphertext.*chunk" docs/specs/ENCRYPTION_ENVELOPE.md`
Expected: reframed to origin-side Range decryption.

- [ ] **Step 5: Commit**

```bash
git add docs/specs/ENCRYPTION_ENVELOPE.md
git commit -s -m "docs(specs): ENCRYPTION_ENVELOPE v3 — reframe v2 Range benefit (D12), mark v2 layout/AAD authoritative in P2-M8 (D2/D3)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

## Task 7: `ARCHITECTURE_DECISIONS.md` Tier-1 edits

**Files:** `docs/specs/ARCHITECTURE_DECISIONS.md` (T1.7/T1.7a at :72–73; T1.10 at :81; T1.14 at :85).

- [ ] **Step 1: T1.10** — change "HMAC repair token" to "coordinator-issued, source-and-destination-pinned **Ed25519** repair token (single/bounded-use)". Note the FED v-bump as the implementation gate.

- [ ] **Step 2: T1.7 / T1.7a** — append to each: "(Phase 2) the v2 streaming-AEAD **record/DAG layout and per-chunk AAD construction are authoritative in P2-M8**; the earlier 'chunk N == block N' / 'AAD binds … cid' phrasing is superseded pending that milestone." Keep v1 determinism unchanged.

- [ ] **Step 3: T1.14** — confirm/clarify wording to **acked-only** ("CIDs at one **acked** pin"; pending pins never satisfy the Tier-1 trigger), consistent with the HEALING D5 amendment.

- [ ] **Step 4: Add two new Tier-1 rows** under "Transport and federation" (or a new "Placement and trust" subgroup):
  - `T1.29` — Replica placement applies **operator-verified failure-domain anti-affinity** (soft preference, never a veto); steady-state placement weight is **decoupled from donor bandwidth** (bandwidth governs repair-source selection only). Enforced: `HEALING_PROTOCOL.md`; 2026-06-12 resilience design.
  - `T1.30` — Donor `trust_state` (`probationary`/`trusted`/`suspended`) gates `placement_weight`; **probationary nodes are never the sole or second copy of `important`-class data**. Enforced: `HEALING_PROTOCOL.md`.

  (Renumber if `T1.29`/`T1.30` already exist; pick the next free numbers.)

- [ ] **Step 5: Amendment-process note.** Add a sentence to the "A Tier-1 change requires…" area recording that these rows were added/amended by **P2-M0 (2026-06-13)** with FED/HEAL spec v-bumps as the implementation gates.

- [ ] **Step 6: Verify**

Run: `rg -n "HMAC|Ed25519|chunk N == block N|one acked pin|failure-domain|trust_state" docs/specs/ARCHITECTURE_DECISIONS.md`
Expected: T1.10 Ed25519; T1.7/T1.7a M8-note; T1.14 acked-only; T1.29/T1.30 present; no HMAC repair-token claim.

- [ ] **Step 7: Commit**

```bash
git add docs/specs/ARCHITECTURE_DECISIONS.md
git commit -s -m "docs(specs): ARCHITECTURE_DECISIONS Tier-1 — T1.10 Ed25519, T1.7/T1.7a M8-pending, T1.14 acked-only, +T1.29/T1.30 placement & trust (P2-M0)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

## Task 8: `DATA_MODEL.sql` Phase 2 commentary

**Files:** `docs/specs/DATA_MODEL.sql` (annotated living reference; the migrations are authoritative and frozen).

- [ ] **Step 1:** Add Phase 2 **commentary** (SQL comments, not a new live table unless the file already carries forward-looking commentary blocks — match the file's existing style) for:
  - `pin_assignments`: `assignment_id uuid` + `generation bigint`; conditional transitions `… WHERE assignment_id=$ AND generation=$ AND state='pending'` (D6).
  - `pin_changes(sequence bigserial PK, node_id, assignment_id, generation, kind, cid, created_at)` + retention (D7).
  - `blob_replication_state(cid PK, healthy_acked_count, in_flight_count, target_count, safety_tier, updated_at)` projection.
  - `nodes`: `donor_principal_id`, `failure_domain_id`, `provider`, `asn`, `operator_verified_at` (D8); `trust_state` enum + `placement_weight` (D9).
  - `pin_audits`: coordinator-receive-time columns + sampling-weight inputs (D10).

- [ ] **Step 2:** Each block ends with: `-- Live DDL ships as a new (non-frozen) Phase 2 migration in P2-M3; Phase 1 migrations under internal/db/migrations/ remain frozen.`

- [ ] **Step 3:** Add a forward-compat note (comment): the Phase 6 fencing columns (`jobs.lease_id`/`lease_generation`, `coordinator_leases`) and `origin_locations` are **additive future work, not part of Phase 2** — do not add them here.

- [ ] **Step 4: Verify**

Run: `rg -n "assignment_id|generation|pin_changes|blob_replication_state|failure_domain_id|trust_state|placement_weight" docs/specs/DATA_MODEL.sql`
Expected: all present as commentary; `git diff` shows no change to any live `CREATE TABLE` that a frozen migration depends on.

- [ ] **Step 5: Commit**

```bash
git add docs/specs/DATA_MODEL.sql
git commit -s -m "docs(specs): DATA_MODEL Phase 2 commentary — assignment_id/generation, pin_changes, blob_replication_state, failure-domain/trust columns, audit receive-time (P2-M0 D6/D7/D8/D9/D10)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

## Task 9: `THREAT_MODEL.md` — reconcile Phase 2 amendment + verify D-sign

**Files:** `docs/THREAT_MODEL.md` (a "Phase 2 amendment" section was drafted 2026-06-11; `edd0da6` later touched 4 lines).

- [ ] **Step 1: Read** the existing "Phase 2 amendment" section and check each P2-M0 adversary/mitigation pair is consistent with the final D-item resolutions: repair-token forgery/replay (D1 Ed25519 + `jti` cache), assignment replay (D6), Sybil/failure-domain forgery (D8/D9), audit collusion/backdating (D10 receive-time), bandwidth exhaustion (D11 donor-authoritative), supply-chain against volunteers (D-sign).

- [ ] **Step 2: D-sign.** Confirm the release-signing promotion is present: cosign signatures + per-image SBOM + SLSA/GitHub provenance for **both** images; the donor walkthrough pins an **image digest** (not `latest`) and verifies the signature. Add it if missing.

- [ ] **Step 3:** Only bump/changelog if content changed; if it is already complete and consistent, record "verified consistent with P2-M0" in the changelog line and make no substantive edit.

- [ ] **Step 4: Verify**

Run: `rg -ni "phase 2 amendment|ed25519|cosign|sbom|digest" docs/THREAT_MODEL.md`
Expected: the Phase 2 amendment section exists and references Ed25519 + cosign/SBOM/digest.

- [ ] **Step 5: Commit** (skip if no change)

```bash
git add docs/THREAT_MODEL.md
git commit -s -m "docs: THREAT_MODEL — reconcile Phase 2 amendment with final P2-M0 D-item resolutions; confirm D-sign release-signing promotion

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

## Task 10: Phase 2 master design forward-compat section + master plan pointers

**Files:** `docs/superpowers/specs/phase2/2026-06-11-phase2-federation-design.md`, `docs/superpowers/plans/phase2/2026-06-11-phase2-federation.md`.

- [ ] **Step 1: Master design** — add a subsection **"Forward-compatibility with post-1.0 HA/peering"** (after "Performance concerns" or near the milestone table) containing: the governing scope rule (build Phase-2 structures that double as Phase 6/7 prerequisites; build no Phase-6-only machinery); the prerequisites table (`assignment_id`/`generation`, `pin_changes`, verified `failure_domain_id`, acked-only durability) with their post-1.0 roles; the `jobs` queue fencing gap (contained today, fenced additively in Phase 6); and an explicit "named-but-not-built" list (`coordinator_leases`, `origin_locations`, multi-endpoint failover, peering). Cross-reference the phase6 resilience design and this P2-M0 design.

- [ ] **Step 2: Master design** — add the P2-M0 design + plan to its cross-references; in the milestone table, note P2-M0 has a dedicated design/plan pair (2026-06-13).

- [ ] **Step 3: Master plan** — in the "### P2-M0" summary and the milestone table `Plan` column, point at the dedicated design/plan pair (2026-06-13) as the detailed authority; keep the inline P2-M0 task list as a summary or replace it with a pointer (the dedicated plan is now authoritative).

- [ ] **Step 4: Master plan** — add forward-compat one-liners: to P2-M3 ("`assignment_id`/`generation` + `pin_changes` are also the Phase 6 multi-endpoint-failover prerequisites — design HA-compatibly") and P2-M5 ("placement decoupled from bandwidth per D8; emit concentration metrics for the Tier-2 alerts; calibrate the steady-state weight formula here").

- [ ] **Step 5: Verify links**

Run: `python3 scripts/check_doc_links.py docs`
Expected: no new broken links.

- [ ] **Step 6: Commit**

```bash
git add docs/superpowers/specs/phase2/2026-06-11-phase2-federation-design.md docs/superpowers/plans/phase2/2026-06-11-phase2-federation.md
git commit -s -m "docs(phase2): future-proof master design/plan — HA/peering forward-compat record; point P2-M0 at dedicated docs

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

## Task 11: Final consistency sweep + doc-link check (the exit gate)

**Files:** none (verification); fix any straggler found.

- [ ] **Step 1: Superseded-claim sweep across all of `docs/`**

```bash
cd /home/archbug/projects/nova
echo "-- HMAC repair token --";        rg -ni "hmac.*(repair|token)|repair.*token.*hmac" docs/specs
echo "-- hash(bytes)==cid --";          rg -n  "hash\(bytes\)|== hash|hash == cid" docs/specs
echo "-- chunk N == block N --";        rg -ni "chunk n == block n" docs/specs
echo "-- 0.5*pending durability --";    rg -n  "0\.5 ?\* ?pending|effective_count = acked" docs/specs
echo "-- donor completed_at deadline -- (inspect each hit)"; rg -n "completed_at" docs/specs/POSSESSION_AUDIT.md
```

Expected: the first four print **nothing** (or only explicitly-M8-pending / "does-not-count" context, confirmed by reading); the `completed_at` hits are recorded-column references, not the deadline decision.

- [ ] **Step 2: Stale phase6 path sweep**

Run: `rg -n "phase2/2026-06-12-resilience" docs simulations`
Expected: **no matches.**

- [ ] **Step 3: Version-bump audit** — each amended spec's `Status:` line shows the new version + a P2-M0 changelog line:

Run: `rg -n "Status:|P2-M0|2026-06-13" docs/specs/FEDERATION_PROTOCOL.md docs/specs/HEALING_PROTOCOL.md docs/specs/POSSESSION_AUDIT.md docs/specs/ENCRYPTION_ENVELOPE.md`
Expected: v3 / v3 / v2 / v3 + changelog citations.

- [ ] **Step 4: Doc-link check**

Run: `python3 scripts/check_doc_links.py docs`
Expected: no new broken links (pre-existing quickstart screenshot placeholders excepted).

- [ ] **Step 5: Commit any straggler fixes** (or a no-op if the sweep is clean):

```bash
git add -A docs
git commit -s -m "docs(phase2): P2-M0 consistency sweep — no superseded claims remain; links resolve

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>" || echo "nothing to commit — sweep clean"
```

---

## Gotchas and mitigations

- **`DATA_MODEL.sql` is commentary, not the migration.** P2-M0 adds annotations only; the executable DDL ships in P2-M3 as a **new** migration file. Never edit `internal/db/migrations/000*` (the `migrations-frozen` gate).
- **`completed_at` may legitimately survive** as a recorded column — the fix is that the **deadline decision** uses coordinator receive-time. Don't blindly delete the column reference; reword the decision logic.
- **Bandwidth still matters for repair-source selection.** D8 decouples *steady-state placement* from bandwidth, not *repair sourcing*. Don't over-correct and strip bandwidth from source selection.
- **Don't build Phase 6.** No `coordinator_leases`, no `jobs.lease_id`, no `origin_locations` in any spec's live DDL — name them as additive future work only.
- **`check_doc_links.py` is manual + reports known placeholders.** Compare against the pre-existing baseline; only *new* breakage is a failure.
- **The phase6 move must be staged as a rename** so history is preserved (`git add -A` both paths before committing Task 1).
- **No P2-M1 code until merged.** This plan ends at a clean spec set; the federation skeleton is the next milestone.
