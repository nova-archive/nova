# P2-M0 — Spec Reconciliation (+ post-1.0 future-proofing) Design

Status: **design** (milestone design for P2-M0, the first Phase 2 milestone).
Expands the P2-M0 summary in the Phase 2 master design
[`2026-06-11-phase2-federation-design.md`](2026-06-11-phase2-federation-design.md)
and master plan
[`../../plans/phase2/2026-06-11-phase2-federation.md`](../../plans/phase2/2026-06-11-phase2-federation.md).
The reconciliation backlog (D1–D12, D-cap, D-sign) in the master design is the
source of truth; this document is the authoritative *what-and-why* for the
edits P2-M0 actually lands, and it folds in the forward-looking findings of the
second-pass resilience analysis
[`../phase6/2026-06-12-resilience-and-post-1.0-architecture-design.md`](../phase6/2026-06-12-resilience-and-post-1.0-architecture-design.md).

Authors: Bug Plowman (operator), Claude (implementation partner).

Implementation plan:
[`../../plans/phase2/2026-06-13-phase2-m0-spec-reconciliation.md`](../../plans/phase2/2026-06-13-phase2-m0-spec-reconciliation.md).

---

## Purpose and framing

P2-M0 is a **Tier-1 correctness gate, not housekeeping.** Phase 0 produced the
normative federation / healing / possession / envelope specs *before any
implementation existed*. Implementation review and the external architectural
review that seeded the Phase 2 design found that several of those specs are
**internally contradictory or unimplementable as written**, and that some of the
defects sit in the **Tier-1 protocol-enforced** table of
`ARCHITECTURE_DECISIONS.md`. You cannot write a *conforming*
`cmd/node` / `internal/federation` against contradictory Tier-1 specs, so the
first Phase 2 milestone reconciles the specs and **gates every Phase 2 code
milestone** (P2-M1 onward).

The concrete protocol bugs P2-M0 removes (verified against the cited specs):

- **HMAC repair token "verified by a coordinator pubkey"** — symmetric MAC,
  asymmetric verification; nonsensical, and distributing an HMAC verify-key lets
  *any* donor mint tokens (`FEDERATION_PROTOCOL.md:247`).
- **`hash(bytes) == cid` transfer verification** — wrong for any multi-block
  UnixFS/DAG-PB root (`FEDERATION_PROTOCOL.md:266`, `:320`).
- **`effective_count = acked + 0.5·pending`, `tier1 ⇔ 0 < effective < 2`** — a
  CID with 1 acked + 2 pending scores 2.0 ("safe") with one durable copy,
  contradicting Tier-1 `T1.14` (`HEALING_PROTOCOL.md:57`, `:62`).
- **Donor-supplied audit deadline** — verification passes when the *donor's*
  `completed_at < deadline`; a lying donor backdates it (`POSSESSION_AUDIT.md:112`).
- **Unversioned assignment transitions** — `pin_assignments` PK `(cid, node_id)`
  with no generation; a delayed `ack`/`fail` mutates a reused row.
- **Self-declared failure domains** — `nodes.geo_declared` only; placement cannot
  enforce the anti-affinity the volunteer guide promises.
- **"chunk N == block N"** streaming-AEAD claim — unachievable layout, and the
  per-chunk AAD binds the final CID circularly (`ENCRYPTION_ENVELOPE.md:423`).

P2-M0 layers one additional responsibility on top of that reconciliation:
**fold the post-1.0 resilience findings into the Phase 2 docs as
future-proofing**, so Phase 2 lays the right groundwork for the deliberate
post-1.0 phases (Phase 6 multi-coordinator HA, Phase 7 inter-federation peering)
*without building any of their machinery now*.

**P2-M0 is documentation-only. No production code.**

## Governing scope decisions

Three decisions were settled before this design (see the brainstorming round):

1. **HA groundwork = "document forward-deps + cheap additive reservations."**
   The governing rule:

   > Phase 2 MAY introduce structures needed for **Phase 2 correctness** that
   > *also* become Phase 6/7 prerequisites (`assignment_id`, `generation`,
   > `pin_changes`, verified `failure_domain_id` / `donor_principal_id`). Phase 2
   > MUST NOT introduce structures used **only** by Phase 6 runtime logic
   > (`coordinator_leases`, control-terms, job `lease_id`, `origin_locations`,
   > multi-endpoint donor failover, replicated upload staging). Those are *named
   > as additive future work*, not built.

   The single-coordinator architecture "remains correct and sufficient for the
   entire 1.0 line"; HA and peering are post-1.0. P2-M0 therefore *records* the
   Phase 6 seams so they land additively, and builds nothing Phase-6-only.

2. **Placement weight = "fold the direction into D8 now."** D8's
   `HEALING_PROTOCOL.md` amendment states the normative *direction* — steady-state
   placement weight decoupled from bandwidth, bandwidth governing **repair-source
   selection only**, soft failure-domain anti-affinity that never vetoes a
   placement — with the *exact formula* a Tier-2 tunable to be calibrated in
   P2-M5. Direction is settled; the formula is not (resilience design § 9).

3. **Execution = "inline, straight through."** P2-M0 is a bounded consistency
   pass over a known set of documents; one worker holding the full reconciliation
   matrix in working memory is the right shape, with a grep/link consistency
   sweep at the end. **No P2-M1 code begins until P2-M0 merges.**

## Why the resilience findings belong in P2-M0

The second-pass resilience analysis (and its `novasim` calibrated-hybrid
simulator) reached three load-bearing conclusions that change *how Phase 2
should be specified*, not just *what Phase 6 will build*:

1. **Capacity-weighted placement manufactures targeted-attack fragility.** Under
   bandwidth-weighted placement, purging the high-bandwidth VPS cohort (≈24 % of
   nodes) destroys **~64 %** of the corpus; diversity-optimized placement with
   soft anti-affinity cuts that to **~7.5 %** — same failure, ~8.5× less loss.
   This is a **Phase 2 durability decision**, not a Phase 6 one: if D8 only adds
   failure-domain columns but leaves bandwidth-weighted steady-state placement as
   the default, P2-M0 would bake in the exact policy the simulator flagged as the
   highest-leverage durability bug. → folded into **D8** (Decision 2).

2. **The job-queue lacks a fencing token.** `internal/jobs/queue.go`
   `Complete`/`Fail` guard only on `state='leased'` with no lease-owner /
   generation token. Contained *today* (one queue per coordinator process,
   handlers must be idempotent), but a genuine gap a multi-coordinator world must
   close. P2-M0 records it as **additive Phase 6 work** and constrains Phase 2 not
   to deepen it. → forward-compat note, no schema change now.

3. **Some Phase 2 structures are already the Phase 6 prerequisites.** Immutable
   `assignment_id` + `generation` and the durable `pin_changes` change-log are
   exactly what multi-endpoint donor failover (preserving a `since_seq` cursor
   across coordinators) needs; verified `failure_domain_id` is the dimension
   Phase 7 peers count as "at most one failure domain each." P2-M0 designs these
   for Phase 2 correctness **and** documents their dual role so Phase 6/7 reuse
   them rather than reinventing. → Decision 1, option 2.

## Reconciliation backlog handled in P2-M0

These are the master-design D-items that P2-M0 lands. D2/D3 (the v2 record/DAG
layout and per-chunk AAD cryptographic redesign) are **deferred to P2-M8**;
P2-M0 only removes the false *settled* claims and marks them M8-authoritative.

| # | Defect (one-line) | P2-M0 resolution | Spec(s) | Tier-1 action |
|---|---|---|---|:--:|
| **D1** | HMAC token verified by pubkey | **Ed25519** repair tokens; coordinator holds private key, donors get public key via `config_updates`; full claim set; single/bounded-use via source-side `jti` replay cache | FED, ARCH | Amend `T1.10` (FED v-bump) |
| **D4** | `hash(bytes)==cid` verify | Deterministic **re-import via `IPFS_IMPORT_RULES` + root-CID compare** (or CAR + full-DAG verify); `go-car/v2` noted | FED | FED clarification |
| **D5** | `acked + 0.5·pending` durability | **Acked-only** Tier-1 trigger (`acked_count < 2`); `pending` demoted to an in-flight reservation for scheduling dedup only; `pending_weight` may order Tier-2 but never the Tier-1 trigger | HEAL, ARCH | Reconcile to `T1.14` |
| **D6** | `(cid,node_id)` PK, no generation | Add `assignment_id uuid` + `generation bigint`; conditional state transitions carry both | DATA_MODEL | Schema commentary |
| **D7** | No durable change log; "unknown kinds backward-compatible" | `pin_changes(sequence bigserial, …)` + retention; machine-readable `snapshot_required` when `since_seq` predates retention; unknown `kind` **fail-closed + capability-negotiated** | FED, DATA_MODEL | Schema + FED |
| **D8** | self-declared geo, bandwidth-weighted placement | Add `donor_principal_id` + `failure_domain_id` (+ `provider`, `asn`, `operator_verified_at`); **soft failure-domain anti-affinity**; **steady-state placement weight decoupled from bandwidth (direction normative; formula = Tier-2, P2-M5); bandwidth = repair-source selection only** | HEAL, DATA_MODEL, guide | Schema + HEAL (new Tier-1 row) |
| **D9** | probationary nodes carry full placement weight | Orthogonal `trust_state` (`probationary`/`trusted`/`suspended`) + `placement_weight` cap; probationary nodes never the sole/second copy of `important`-class data; graduate on age + transfers + audits | HEAL, DATA_MODEL | Schema + HEAL (new Tier-1 row) |
| **D10** | donor-supplied audit deadline | **Coordinator receive-time** deadline; **synchronous** single-round-trip challenge→response primary (async kept as documented fallback); sampling **weighted by stored bytes / pin count / node age / risk** | POSS, DATA_MODEL | POSS v-bump |
| **D11** | over-committed donor budgets | Coordinator scheduling is a **best-effort reservation**; the **donor's local token-bucket is authoritative**; tokens carry `max_bytes`; reconciled via heartbeat/transfer reports | FED, HEAL | FED + HEAL (`T1.12` enforced at the donor) |
| **D12** | "CDN edges serve ciphertext chunks" | Reframe v2 benefit as **bounded-memory authenticated Range decryption at the origin**; ciphertext-caching edge needs a Nova-aware intermediary, not the default | ENVELOPE | Wording (M8/M10) |
| **D-cap** | single `client_version` string | **Capability negotiation**: `register` sends `supported_protocols` + `capabilities`; coordinator replies `selected_protocol` + `required_capabilities`, fail-closed on no overlap; **identity derives from the verified mTLS cert**, not request JSON | FED | FED |
| **D-sign** | signing deferred to Phase 5 while volunteers run a privileged daemon | **Promote release signing into Phase 2**: cosign + per-image SBOM + provenance for both images; donor walkthrough pins an image **digest** and verifies the signature | THREAT_MODEL, CI plan | THREAT_MODEL |

D2/D3 (deferred to P2-M8): P2-M0 removes "chunk N == block N" from the *settled*
section of `ENCRYPTION_ENVELOPE.md` and marks the v2 record/DAG layout + AAD
construction **authoritative in M8**; `ARCHITECTURE_DECISIONS.md` `T1.7`/`T1.7a`
gain a "v2 layout/AAD authoritative in M8" note. No cryptographic redesign in
P2-M0.

## D8 in detail — placement-weight direction (new normative content)

This is the only substantively *new* normative content P2-M0 introduces beyond
reconciling existing contradictions, so it is specified precisely.

The current `HEALING_PROTOCOL.md` samples initial placement proportional to
`capacity × reputation` with new nodes at `reputation = 1.0`. The amendment
makes the **direction** normative while leaving the **magnitude** tunable:

- **Steady-state replica placement MUST NOT be weighted by donor bandwidth.**
  Bandwidth/throughput is a *repair-source-selection* input (fast nodes do the
  heavy lifting during recovery), **not** a steady-state placement weight (fast
  nodes must stop accreting a disproportionate share of the archive).
- **Placement applies soft failure-domain anti-affinity**: prefer distinct
  `failure_domain_id` (and, secondarily, `donor_principal_id` / `provider` /
  `asn`) when enough domains exist. This is a **preference, never a veto** — a
  hard ceiling can block healing into the only surviving capacity during a
  casualty, precisely when a placement veto is most harmful. (Consistent with
  the Tier-2 "alert, not prevent" stance and the existing `federation.degraded` /
  `federation.shrinking` webhook philosophy.)
- **The exact steady-state weight is a Tier-2 tunable, calibrated in P2-M5.** The
  resilience analysis settles the direction (`~sqrt(free_capacity) × trust` with
  soft anti-affinity beats bandwidth-weighting) but explicitly defers the precise
  form to tuning against real donor populations and a steady-state-churn model
  (resilience design § 9). P2-M0 names the direction and the calibration
  milestone; it does **not** freeze a formula.
- **Concentration is alerted on, not enforced.** P2-M0 cross-references the Tier-2
  concentration-alerting set already added to `ARCHITECTURE_DECISIONS.md`
  (`federation.concentrated` / `federation.homogeneous`; pin-incidence Gini +
  per-dimension largest-share / top-k / normalized entropy) and notes that the
  Phase 2 healing/metrics layer (built in P2-M5) MUST emit those metrics so the
  webhooks have data. No metric *emission code* is written in P2-M0.

## Forward-compatibility with post-1.0 HA/peering

P2-M0 adds an explicit forward-compatibility record so Phase 2 implementers know
which structures carry a Phase 6/7 obligation and which future work is
deliberately *not* built now. This lives as a new subsection in the Phase 2
master design and is summarized here.

**Phase 2 structures that double as Phase 6/7 prerequisites (build now, for
Phase 2 correctness; design HA-compatibly):**

| Phase 2 structure (this milestone's spec) | Phase 2 purpose | Post-1.0 role |
|---|---|---|
| `pin_assignments.assignment_id` + `generation` (D6) | safe ack/fail under superseded assignments | the immutable handle multi-endpoint donor failover keys on |
| `pin_changes` change-log + `since_seq` / `snapshot_required` (D7) | durable incremental assignment sync | the cursor a donor preserves when failing over between coordinators (Phase 6) |
| verified `failure_domain_id` / `donor_principal_id` (D8) | placement anti-affinity + Sybil resistance | the unit a Phase 7 peer counts as "at most one failure domain"; feeds concentration alerting |
| acked-only durability (D5) | correct Tier-1 safety | the durability semantics HA replicas and peer custodians must preserve |

**Phase 6/7-only work named as additive, NOT built in Phase 2:**

- **Control-plane / job-queue fencing.** `internal/jobs/queue.go`
  `Complete`/`Fail` guard only on `state='leased'` (no lease-owner/generation
  token). Contained today (one queue per coordinator process; idempotent
  handlers). Phase 6 adds `lease_id`/`lease_generation` on `jobs` and a
  `coordinator_leases(subsystem, holder, term, expires_at)` table **additively**;
  every control mutation and issued repair token then carries the current term.
  **P2-M0 obligation:** record this, and require that Phase 2 not deepen the gap
  (the orchestrator stays single-leader-per-process; repair tokens already carry
  per-assignment generation, which is forward-compatible with a future term).
- **Origin-location tracking** (`origin_locations(cid, coordinator_id, state,
  failure_domain)` + a transactional outbox for the non-atomic
  Kubo-pin/Postgres-commit boundary) — Phase 6. Phase 2 keeps the single-origin
  model; it only notes the boundary as a known residual (already documented in
  the upload path).
- **Multi-endpoint donor config + failover, replicated/shared upload staging,
  cross-instance signed-URL revocation, shared ingress rate-limiting, redundant
  Nebula lighthouses + Kubo bootstrap peers** — all Phase 6, all per-process in
  Phase 2 by design.
- **Opaque inter-federation peering** (`peer/v1`, home-federation invariant,
  generation-ordered tombstones, encrypted DR packages) — Phase 7.

The reframed Tier-1 invariants `T1.27` (one logical authoritative history;
fenced multi-replica serving allowed) and `T1.28` (opaque-ciphertext-only
peering) are **already in `ARCHITECTURE_DECISIONS.md`** from the second-pass
analysis; P2-M0 does not re-edit them — it only ensures the Phase 2 docs and
cross-references point at the resilience design correctly (see Housekeeping).

## Per-document amendment specification

Each amended normative spec gets a **`Status:` version bump** plus a one-line
changelog entry citing this design. Version convention: the versioned Phase 0
specs go **v2 → v3** (matching the `ARCHITECTURE_DECISIONS.md` v3 generation);
`POSSESSION_AUDIT.md` (currently unversioned "Phase 2 deliverable") gains an
explicit **v2**. The live DDL is **not** written here — `DATA_MODEL.sql` carries
Phase 2 *commentary* and the executable migration ships in **P2-M3** (Phase 1
migrations stay frozen; `migrations-frozen` gate stays green).

1. **`FEDERATION_PROTOCOL.md`** (v2 → v3): D1 (Ed25519 token + claim set +
   replay cache), D4 (re-import + root-CID verify; retire `hash(bytes)==cid` at
   lines 266/320), D7 (`pin_changes` + `snapshot_required` + fail-closed
   capability-negotiated `kind`s), D11 (donor-authoritative token-bucket; tokens
   carry `max_bytes`), D-cap (capability negotiation + identity-from-mTLS-cert).
   Replace the HMAC language at line 247.
2. **`HEALING_PROTOCOL.md`** (v2 → v3): D5 (acked-only Tier-1; demote `pending`
   to in-flight reservation; the `effective_count`/`tier1`/`tier2` definitions at
   lines 56–63 and the `pending_weight` tunable at line 378 are reworded so
   pending cannot lift a 1-acked CID out of Tier-1), D8 (anti-affinity +
   placement-weight direction, per the detailed section above), D9 (trust/probation
   + `placement_weight` cap), D11 (best-effort reservation note), and the
   `blob_replication_state` durable-projection note (no per-tick full scans).
3. **`POSSESSION_AUDIT.md`** (→ v2): D10 — coordinator receive-time deadline (not
   donor `completed_at` at line 112); synchronous single-round-trip challenge as
   the primary design; size/risk-weighted sampling; tighten the "what this
   proves" framing (timely retrievability under the node identity, not unique
   physical residency).
4. **`ENCRYPTION_ENVELOPE.md`** (v2 → v3): D12 (reframe the CDN/ciphertext-edge
   wording); remove "chunk N == block N" from the settled v2 section (line 423)
   and mark the record/DAG layout + AAD construction **authoritative in P2-M8**.
   No v2 cryptographic redesign here.
5. **`ARCHITECTURE_DECISIONS.md`** (Tier-1 edits, following its own amendment
   process — v-bump + implementation-gate note + table update): `T1.10`
   HMAC → Ed25519; `T1.7`/`T1.7a` gain "v2 record/DAG layout + AAD authoritative
   in P2-M8"; `T1.14` wording reconciled to acked-only; **new Tier-1 rows** for
   operator-verified failure-domain placement (anti-affinity) and the
   `trust_state`/probation placement-weight cap. `T1.27`/`T1.28` already reframed
   — left as-is.
6. **`DATA_MODEL.sql`**: Phase 2 *commentary* (not live DDL) for
   `pin_assignments.assignment_id`/`generation` (D6); `pin_changes` (D7);
   `blob_replication_state` projection; `nodes.donor_principal_id` /
   `failure_domain_id` / `provider` / `asn` / `operator_verified_at` /
   `trust_state` / `placement_weight` (D8/D9); `pin_audits` receive-time +
   sampling-weight columns (D10). Each block notes "live DDL ships as a new
   migration in P2-M3."
7. **`THREAT_MODEL.md`**: **reconcile and complete** the "Phase 2 amendment"
   section (drafted during the 2026-06-11 design pass) against the final D-item
   resolutions, and ensure the **D-sign release-signing promotion** is present
   (cosign + SBOM + provenance for both images; digest-pinned, signature-verified
   donor walkthrough). If already complete and consistent, this task is a verify.

## Housekeeping — reorg debt left by the Phases 6/7 carve-out

The second-pass design was moved to `docs/superpowers/specs/phase6/` but several
cross-references still point to the old `phase2/` path, and the index READMEs
do not list it. P2-M0 closes this:

- **Repoint** every reference to the resilience design from
  `…/specs/phase2/2026-06-12-…` to `…/specs/phase6/2026-06-12-…` in
  `docs/specs/ARCHITECTURE_DECISIONS.md`, `docs/ROADMAP.md`, and
  `simulations/go/README.md`.
- **Specs README index** (`docs/superpowers/specs/README.md`): add a **Phase 6**
  section listing the resilience design (note it is a forward-looking
  analysis/design, not a 1.0 milestone).
- **Plans README index** (`docs/superpowers/plans/README.md`): add the P2-M0
  plan to the Phase 2 table; note Phase 6 is design-only (no plan yet).
- **Phase 2 master design** (`2026-06-11-phase2-federation-design.md`): add the
  "Forward-compatibility with post-1.0 HA/peering" subsection summarized above,
  and add this P2-M0 design + plan to its cross-references.
- **Phase 2 master plan** (`2026-06-11-phase2-federation.md`): point the P2-M0
  section at this dedicated design/plan pair (keep the milestone table and the
  detailed P2-M1 tasks); add forward-compat one-liners to the P2-M3 and P2-M5
  summaries.

## Non-goals (P2-M0)

- **No production code** — not `cmd/node`, not `internal/federation`,
  `internal/orchestrator`, or `internal/audit/possession`; not a migration.
- **No Phase-6/7 machinery** — no `coordinator_leases`, no job `lease_id`, no
  `origin_locations`, no multi-endpoint failover, no peering protocol. Named as
  additive future work only.
- **No v2 envelope cryptographic redesign** — D2 (record/DAG layout) and D3
  (header-commitment AAD) are P2-M8. P2-M0 only deletes the false settled claim.
- **No exact placement-weight formula** — direction only; formula is Tier-2,
  calibrated in P2-M5.
- **No VOLUNTEER_DEPLOYMENT_GUIDANCE rewrite** — already corrected in the
  2026-06-11 design pass; P2-M0 only verifies consistency with the final D8/D-sign
  text.

## Exit criteria

1. Every amended normative spec carries a **`Status:` version bump** and a
   changelog line citing this design.
2. A **consistency sweep** finds no normative doc still asserting a superseded
   claim: `HMAC` repair token, `hash(bytes)` / `== hash(bytes)`, `chunk N ==
   block N` (except explicitly marked P2-M8-pending), `0.5 * pending` /
   pending-toward-durability, or a donor-supplied (`completed_at`) audit
   deadline.
3. `ARCHITECTURE_DECISIONS.md` Tier-1 reflects Ed25519 (`T1.10`), acked-only
   (`T1.14`), the M8-pending envelope note (`T1.7`/`T1.7a`), and the new
   failure-domain + trust rows; the amendment process (v-bump + implementation
   gate) is followed for each.
4. The Phase 2 master design carries the forward-compatibility subsection; the
   Phase 6 resilience design is reachable from the specs README and every
   cross-reference resolves.
5. `python3 scripts/check_doc_links.py docs` reports no **new** broken links
   (the pre-existing un-captured quickstart screenshot placeholders excepted).
6. **Guardrail:** no P2-M1 (`cmd/node`) code is started until P2-M0 is merged and
   the sweep passes — writing `internal/federation` against the un-amended specs
   would bake in the HMAC-token and `hash(bytes)==cid` bugs.

## Cross-references

- Master design + backlog: [`2026-06-11-phase2-federation-design.md`](2026-06-11-phase2-federation-design.md)
  (D1–D12, D-cap, D-sign).
- Master plan: [`../../plans/phase2/2026-06-11-phase2-federation.md`](../../plans/phase2/2026-06-11-phase2-federation.md).
- Resilience analysis + `novasim`: [`../phase6/2026-06-12-resilience-and-post-1.0-architecture-design.md`](../phase6/2026-06-12-resilience-and-post-1.0-architecture-design.md).
- Normative contracts amended here: `docs/specs/FEDERATION_PROTOCOL.md`,
  `HEALING_PROTOCOL.md`, `POSSESSION_AUDIT.md`, `ENCRYPTION_ENVELOPE.md`,
  `DATA_MODEL.sql`, `ARCHITECTURE_DECISIONS.md`; `docs/THREAT_MODEL.md`.
- Implementation plan: [`../../plans/phase2/2026-06-13-phase2-m0-spec-reconciliation.md`](../../plans/phase2/2026-06-13-phase2-m0-spec-reconciliation.md).
