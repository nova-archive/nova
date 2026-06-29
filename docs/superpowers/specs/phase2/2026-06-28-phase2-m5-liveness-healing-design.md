# P2-M5 — Liveness & healing

Status: **design**. Spec floor: ROADMAP.md P2-M5; the Phase-2 master design
([`2026-06-11-phase2-federation-design.md`](2026-06-11-phase2-federation-design.md)
§§ "Storage/read architecture (P2-M2.1 amendment)" M5 constraints, "Schema
deltas", "Performance", "Forward-compatibility with post-1.0 HA/peering",
milestone breakdown P2-M5); the P2-M4.1 storage/read-redirect design
([`2026-06-23-phase2-m4.1-storage-read-redirect-design.md`](2026-06-23-phase2-m4.1-storage-read-redirect-design.md)
Forward-compatibility — M5 reuses its read-source server, role-aware identity,
`replay` helper, and read-grant mint, and **assumes no guaranteed coordinator
origin**); the P2-M0-amended normative specs in `docs/specs/`
(`HEALING_PROTOCOL.md` — acked-only durability D5, soft anti-affinity D8,
trust/probation D9, best-effort reservations D11, `blob_replication_state`
projection; `FEDERATION_PROTOCOL.md` §§ "Liveness state machine (v2)", "Repair
transport (Phase 2)", "Donor inbound endpoint", "Mass-casualty event detection";
`POSSESSION_AUDIT.md` — M6, not M5); `ARCHITECTURE_DECISIONS.md`
(T1.10 Ed25519 repair token, T1.12 inviolable budgets, T1.14 acked-only Tier-1,
T1.26 donor-blind, T1.30 probationary placement cap).
Implementation plan:
[`../../plans/phase2/2026-06-28-phase2-m5-liveness-healing.md`](../../plans/phase2/2026-06-28-phase2-m5-liveness-healing.md).

Authors: Bug Plowman (operator), Claude (implementation partner).

> **M4 proved an acked donor holder is real; M4.1 made the verified donor replica
> set the durable substrate the coordinator reads and prunes against. M5 is the
> milestone that keeps that substrate *alive*: it detects donor failure, restores
> Tier-1 durability through budget-respecting donor↔donor repair, places replicas
> to resist correlated loss, and tells the operator when the federation is
> degrading — without ever overriding a donor's bandwidth budget or assuming the
> coordinator still holds a copy.**

## Context

Phase 2 has built the control plane (P2-M2 identity/registration, P2-M3 assignment
sync) and the data plane (P2-M4 replication slice, P2-M4.1 storage/read redirect),
all merged and tagged:

- **P2-M4** (`p2-m4-replication-slice`) made an `ack` mean something real:
  coordinator-as-source signed grants, the donor
  fetch→deterministic-re-import→root-CID-verify→pin→persist→ack loop, and
  crash-safe verified-holder state. Every later subsystem trusts the
  **acked-holder set as durability ground truth**.
- **P2-M4.1** (`p2-m4.1-storage-read-redirect`) turned the coordinator's local
  Kubo from canonical origin into a bounded, prunable cache. Donor-backed reads,
  `require_replication_quorum_before_commit`, origin pruning with
  `prune_safety_floor`, `coordinator_storage_mode`, and the **sourceable-holder**
  selection now run behind `storage.Service.OpenBytes`. **P2-M5 may assume the
  coordinator origin copy is not guaranteed** — M4.1 already prunes it.

**P2-M5 is liveness & healing.** It is where Phase 2's durability commitments stop
being schema and spec scaffolding and become a functioning system:

```
node failure → 5-state liveness transition → cheap replication-state projection
update → strict Tier-1 healing → donor↔donor budget-respecting repair → acked
recovery → operator-visible degraded / concentration signals
```

It is the first milestone with an `internal/orchestrator`, a 5-state liveness
sweeper (nothing transitions a node's `status` today — M2 only sets `active` on
register, and `UpdateNodeHeartbeat` never touches it), the `blob_replication_state`
projection (named in the 0012 migration comment but never created — M4.1's
`blob_storage_state` is a *different*, coordinator-local cache/commit projection),
the D8/D9 placement schema (the 0011 migration explicitly deferred
`failure_domain_id`/`donor_principal_id`/`provider`/`asn`/`placement_weight` to
P2-M5), true **donor↔donor** repair (M4.1's `internal/node/source` is
coordinator-only read-source), and the **first webhook dispatcher** (the
`WebhookDestination` config and paranoid gating exist; no emitter does).

**What M5 consumes vs. what it produces.** M5 *consumes* `reputation_score` and
`trust_state` (already columns) for source-selection weighting and placement caps;
it does **not** update them — possession-audit-driven reputation movement and
trust *graduation* are **P2-M6**. M5 makes the healing/placement/liveness machine
correct against the reputation and trust signals that exist.

**The donor-blind, coordinator-served invariant is preserved (T1.26).** Repair
moves **ciphertext** between donors under coordinator-minted Ed25519 grants; the
coordinator never stops being the sole decrypt/serve point for user bytes. Donors
serve ciphertext only — in M4.1 to the authenticated coordinator, and in M5 also
to an authenticated **destination donor** named by the coordinator's grant.

## Decisions (ratified with Bug, 2026-06-28)

These decisions were ratified across three review passes; the rev-2 and rev-3
passes corrected several places where an earlier draft assumed a seam already
carried information it does not. Each such correction is grounded in the current
tree with a `file:line` so the implementation cannot re-introduce the wrong
assumption.

### D-M5-1 — Scope is liveness & healing, as one internally-staged milestone.

M5 is **one** milestone (one design, one plan), staged into seven independently
testable slices, **not** split into M5/M5.1. The slices are: (1) schema +
projection, (2) liveness sweeper + recovery, (3) placement engine, (4) donor↔donor
repair, (5) healing scheduler, (6) signals/metrics, (7) functional chaos.
Splitting would leave invalid half-transitions: liveness without healing is mere
observability; repair without liveness has no trigger; placement without
concentration metrics has no feedback loop. The coordinator remains the sole
decrypt/serve authority throughout, and **cache/origin copies never count toward
`R`** (the M4.1 amendment is load-bearing here).

### D-M5-2 — A new `blob_replication_state` projection, distinct from M4.1's `blob_storage_state`.

`blob_storage_state` (migration 0013) is **coordinator-local** cache/commit
policy: `commit_state`, `local_role`, `local_present`, `local_bytes`,
`cache_segment`, prune eligibility. `blob_replication_state` is **donor-replica
health**. They have different update triggers, different consumers, and different
failure modes; keeping them separate preserves Nova's authority discipline — the
projection is **rebuildable cache**, and authority remains `pin_assignments` ⨝
node liveness.

```sql
CREATE TABLE blob_replication_state (
  cid                    text PRIMARY KEY REFERENCES blobs(cid) ON DELETE CASCADE,
  healthy_acked_count    integer NOT NULL DEFAULT 0,  -- acked on countable nodes (Tier-1/2 trigger)
  sourceable_acked_count integer NOT NULL DEFAULT 0,  -- subset currently selectable as a source
  in_flight_count        integer NOT NULL DEFAULT 0,  -- pending reservations (never lift Tier-1)
  target_count           integer NOT NULL,            -- R(durability_class); see D-M5-2b
  safety_tier            text NOT NULL CHECK (safety_tier IN
                           ('donor_lost','tier1','tier2','healthy')),  -- see D-M5-2c
  local_recoverable      boolean NOT NULL DEFAULT false,  -- coordinator holds a usable local copy
  durability_class       text NOT NULL CHECK (durability_class IN ('important','normal','cache')),
  dirty                  boolean NOT NULL DEFAULT false,  -- lags authority; recompute before scheduling
  updated_at             timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX blob_replication_safety_idx     ON blob_replication_state (safety_tier, updated_at);
CREATE INDEX blob_replication_class_tier_idx ON blob_replication_state (durability_class, safety_tier);
CREATE INDEX blob_replication_dirty_idx      ON blob_replication_state (dirty) WHERE dirty;
```

`healthy_acked_count` drives the acked-only Tier-1/Tier-2 trigger (D5);
`sourceable_acked_count` makes degraded *read* availability observable without
re-joining the M4.1 sourceable filter on every query. The orchestrator reads the
projection — it never re-aggregates `pin_assignments ⨝ nodes ⨝ blobs` per tick
(master design § Performance forbids per-tick full scans). A periodic full
reconciliation rebuilds the projection as a correctness audit.

#### D-M5-2a — Sourceability becomes status-based; the liveness sweeper is the sole freshness authority.

Today the sourceable predicate carries an independent **time** condition —
`n.last_seen_at > now() - make_interval(secs => $2::float)`
(`internal/db/gen/storage_state.sql.go:43,192`). No transaction fires when "now"
crosses that boundary, so a *maintained* `sourceable_acked_count` would silently
go stale. Post-M5, **liveness status is the single freshness authority**:
redefine sourceability as `status IN ('active','suspect')` **+**
`assignment_sync_state = 'current'` **+** the read-source capability **+**
`source_nebula_addr` present **+** `trust_state <> 'suspended'`, and **drop the
second time-based rule**. Freshness is represented by `status`, not duplicated in
every query, so the sweeper's transition is the one event that updates
`sourceable_acked_count`.

**Two predicates, one projected.** *Read*-sourceability requires the
`read-source/v1` capability (M4.1); *repair*-sourceability additionally requires
`repair-stream/v1` (D-M5-8c). `sourceable_acked_count` is the **read**-availability
count (it feeds commit/prune/read selection, the M4.1 consumers) and is the value
projected onto `blob_replication_state`. Repair-source selection recomputes the
**repair**-sourceable set live during scheduling (it is selection-time, not a
durability count), so M5 does **not** project a separate repair count — a
mixed-version donor that advertises read but not repair stays a valid read source
while being skipped as a repair source.

This **orphans** the M4.1 `prune_stale_seconds` knob (`internal/config/types.go:258`,
feeding `CountSourceableHolders`). M5 **deprecates / reinterprets** it: the pruner
counts `sourceable_acked_count` derived from status, not an independent
`stale_secs` window. Config / `/settings` / docs stop exposing a knob that no
longer does what it says.

#### D-M5-2b — `target_count` is persisted; an R change must reconcile the projection.

`target_count` is stored (read-cheap for selection and safety-tier classification),
so a change to `orchestrator.replication.factor.*` MUST recompute `target_count`
for the affected `durability_class` rows, recompute `safety_tier`, and enqueue
newly under-replicated CIDs (D-M5-2d). A hot-reload of `R` that skips this would
leave a stale durability view that under- or over-schedules.

#### D-M5-2c — Safety-tier names admit a local emergency copy.

`lost` is too strong when the coordinator still holds a copy (the M4.1 default
`origin_copy` mode keeps one). Use `donor_lost` (zero *healthy donor* holders)
plus the `local_recoverable` flag:

- `donor_lost ∧ local_recoverable` → repair from the coordinator's local copy as
  an emergency source (D-M5-8b);
- `donor_lost ∧ ¬local_recoverable` → genuinely unavailable; **do not** spin the
  healing queue (there is nowhere to source from).

`tier1` = exactly one healthy donor holder (one failure from loss); `tier2` =
2 ≤ healthy < target; `healthy` = healthy ≥ target.

#### D-M5-2d — Projection maintenance is bounded and resumable; `dirty` means "recompute before scheduling."

A provider-purge can dirty a very large affected set; a single "same-transaction
updates everything" pass is unsafe at that scale (a *Release It!* unbounded-result-
set / integration-point hazard). The wording is precise about what is and isn't
in-transaction:

- A **single-CID** writer (one assign/ack/fail/unpin) updates the projection **in
  its own mutation transaction**.
- A **bulk** node-status transition only **enqueues/dirties** the affected CIDs in
  its mutation transaction (set `dirty = true` and/or insert into
  `blob_replication_reconcile_queue(cid PK, reason, enqueued_at)`); recomputation
  is **asynchronous and bounded** by a batch drain loop.

`dirty` means **"recompute from authority before scheduling,"** not "schedule as
under-replicated." Scheduling directly from a stale `dirty` row's counts would
double-assign, because `healthy_acked_count`/`in_flight_count`/`target_count` are
all stale. So a `dirty` row is conservative for *readiness / status display*, but
the scheduler **recomputes that CID from authority before placing any new
assignment** (it may *prioritize* dirty CIDs for recompute). The drain loop is
idempotent and batch-bounded; no single unbounded transaction.

### D-M5-3 — D8/D9 placement schema (migration 0014).

`nodes` gains operator-verified `failure_domain_id`, `donor_principal_id`,
`provider`, `asn`, `region`, `operator_verified_at`, and the D9 `placement_weight`
cap (`trust_state` already exists from 0011). `geo_declared` stays informational
only. The operator sets the verified fields via new DB-direct `novactl node`
subcommands (like M2's `revoke`/`list`). Migration 0014 also creates
`blob_replication_state` + `blob_replication_reconcile_queue` + the
`webhook_suppression` window store (D-M5-9a), adds
`pin_assignments.source_node_id` (D-M5-8a, a **nullable FK to `nodes(id)`** —
NULL means coordinator-sourced; the reserved `wire.CoordinatorSourceID` is never
stored, only translated onto the wire), `nodes.assignment_sync_state` (D-M5-2a/4a),
and `nodes.revoked_signaled_at` (D-M5-4-REVOKE-OBS). It backfills
`blob_replication_state` from **`blob_storage_state`, left-joining holder state**,
so `active`/`quarantined` blobs with zero donor holders are represented as
`donor_lost` (anchoring on `pin_assignments` would miss exactly that set). Forward-only; appended to
`MANIFEST.sha256`; `migrations-frozen` + `donor-deps-boundary` stay green.

#### D-M5-3a — Unknown / unverified domains collapse into one bucket.

Anti-affinity **trusts only `operator_verified_at`-backed** dimension values. A
NULL or unverified `failure_domain_id`/`donor_principal_id`/`provider`/`asn`/
`region` **collapses into a single `unknown` bucket** — all unknowns compare
*equal* for anti-affinity, so a node cannot evade diversity placement by leaving
fields blank or varying self-declared data (consistent with the existing rule that
`geo_declared` is informational and placement uses operator-verified domains). A
probationary or unverified node **cannot** satisfy the sole/second-copy rule for
`important` blobs (T1.30).

### D-M5-4 — 5-state liveness sweeper.

`reconcile_node_liveness()` runs in the coordinator process (single-leader-
per-process — see Forward-compatibility) and transitions, per the FED v2 state
machine, `active→suspect` (missed `suspect_after_missed_heartbeats`)
`→unreachable` (missed `> unreachable_after_seconds` — healing engages here)
`→evicted` (missed `> evicted_after_seconds`). Each transition updates
`blob_replication_state` for the affected CIDs (D-M5-2d) and feeds the
mass-casualty burst detector (D-M5-11). The per-class healing target is
content-class-keyed (`important` 5 / `normal` 3 / `cache` 2), read from the
projection's `target_count`.

#### D-M5-4-REVOKE — Revocation fallout is owned by M5 (corrects a rev-1 error).

The rev-1 draft claimed M2's `novactl node revoke` already deletes assignments.
It does not: `RevokeNode` is **UPDATE-only** —
`UPDATE nodes SET status='revoked', cert_revoked_at=now(), last_status_change_at=now()`
(`internal/db/gen/federation.sql.go:687`). M5 owns the fallout. **Chosen: retain
the revoked node's `pin_assignments` rows as forensic / security-incident
evidence, but mark them non-counting** (the projection update excludes revoked
nodes from `healthy_acked_count`) **and enqueue the affected CIDs for healing**,
all in the **same transaction** as the status flip and the `federation.node_revoked`
signal. (Deletion would destroy history and complicate projection debugging; we
delete only if a protocol requirement forces it, and then only same-tx.) A revoked
cert cannot reattach, so retained rows are safely non-counting.

#### D-M5-4-REVOKE-OBS — A coordinator path observes DB-direct revocation and emits once.

`novactl node revoke` is **DB-direct**, so a coordinator-local emitter never sees
it. M5 adds an observation path: the liveness sweeper detects `status='revoked'`
rows not yet signaled (durable `nodes.revoked_signaled_at`) and emits
`federation.node_revoked` **exactly once** + enqueues the fallout — rather than
assuming the CLI emits.

#### D-M5-4a — Liveness recovery and re-registration are status transitions, with a countability guard.

Heartbeats and re-registration are **status transitions, not just telemetry
writes** — today `UpdateNodeHeartbeat` refreshes only `last_seen_at`/free/stored
bytes/`source_nebula_addr` (`internal/db/gen/federation.sql.go:722`) and never
touches `status`, and `RegisterNode` ON CONFLICT does not reset it. On an
authenticated heartbeat: `active→active`; `suspect→active`; `unreachable→active`;
`evicted` does **not** silently reactivate (it must re-register / be explicitly
reactivated, and `RegisterNode` ON CONFLICT must now update `status` so an evicted
node re-registering under the same cert-derived id becomes usable); `revoked`
never reactivates by heartbeat.

**Countability guard.** Status alone is too coarse: a node that flips
`unreachable→active` must **not** have its stale old `acked` rows counted before
it has reconciled its desired set to the current epoch — that would reintroduce
false durability. `nodes.assignment_sync_state ∈ {current, snapshot_required,
reconciling}` gates countability; healthy/sourceable counts require
`assignment_sync_state = 'current'`. A reactivated node counts again only after it
completes the changes/snapshot recovery (P2-M3) to the current epoch.

#### D-M5-4a-EVICT — Eviction retires the node's live assignments.

Eviction removes the node's assignments from the **desired set**. `pin_state` has
no `retired` value, so M5 **deletes** the evicted node's `pin_assignments` (after
enqueueing the affected CIDs for healing, same-tx) rather than leaving ordinary
`acked` rows that a later re-registration could make look current. This is
deliberately **asymmetric** with revoked (which *retains* rows as evidence,
D-M5-4-REVOKE): a revoked cert cannot reattach, so its retained rows are safely
non-counting; an evicted node *can* re-register, so its stale rows must not
survive. Both end non-counting. Forensic history, if wanted, goes to a separate
audit row, never overloaded onto live `pin_assignments`.

#### D-M5-4a-LIVENESS-SIGNAL — Heartbeat is the canonical liveness path.

Today only the heartbeat handler updates `last_seen_at`
(`internal/federation/coordinator/handlers.go:163`). A donor polling `changes` or
sending `ack`/`fail` is also demonstrably alive, but **heartbeat stays the
canonical transition path**: a non-heartbeat authenticated request **may** bump
`last_seen_at` opportunistically but **never** bypasses the assignment-sync
reconciliation gate. So an `ack` from a recovering node does not silently mark it
countable, and does not leave it weirdly `unreachable` either.

### D-M5-5 — Endpoint × status authorization matrix.

Make endpoint authorization explicit rather than incidental handler behavior:

| Endpoint / role | active | suspect | unreachable | evicted | revoked |
|---|---|---|---|---|---|
| `heartbeat` | ok (stay) | ok → **active** | ok → **active** (reconcile before counting) | **reject → re-register** | reject |
| `pins/changes` | ok | ok | only after heartbeat reactivation | reject | reject |
| `ack` / `fail` | ok (generation-current) | ok | ok (recovery) | reject | reject |
| selectable as repair **source** | yes | yes (incl. last-copy) | no | no | no |
| selectable as repair / new-pin **destination** | yes | **no** (pause new) | no | no | no |
| counts toward `healthy_acked_count` | yes | yes | no | no | no |

A `suspect` node may be the only surviving **source** of a Tier-1 CID — refusing
it as a source would *raise* data-loss risk — so it stays a valid source, but is
never a new **destination** (new assignments pause while it is suspect).
Countability additionally requires `assignment_sync_state='current'` (D-M5-4a).

### D-M5-6 — The healing orchestrator (`internal/orchestrator`).

`reconcile_node_liveness()` and the healing tick loop live in a new
`internal/orchestrator` package implementing `HEALING_PROTOCOL.md`: **strict
Tier-1-first** (the orchestrator MUST NOT process Tier-2 in any tick where Tier-1
is non-empty); **acked-only durability** (pending assignments are in-flight
reservations only and never lift a CID out of Tier-1, D5); **asymmetric source
selection** weighted by capacity × reputation; **trickle pacing**; **best-effort
reservation** via the existing M3 `AssignPin` advisory-locked seam; an Ed25519
repair token minted from `internal/federation/tokens`; and an `assign` change-log
entry carrying the source designation. The loop reads counts from the projection
(no per-tick full scan) and is single-leader-per-process. On restart it re-derives
tier sets from the projection; donors re-sync via their next changes poll (no
persisted in-flight queue).

#### D-M5-6-TEL — The scheduler's budget math is a telemetry-fed hint; the donor bucket is authoritative.

The coordinator **does not know** a donor's remaining daily budget today: the
heartbeat carries only free/stored bytes (`internal/federation/wire/messages.go:104`)
and the donor's `bandwidth.Bucket` exposes only `Take(n, now) bool`
(`internal/node/bandwidth/bucket.go:40`). M5 adds **optional** heartbeat telemetry
— `egress_budget_remaining_bytes`, `egress_budget_capacity_bytes`,
`egress_refill_bytes_per_second` (and optionally `source_rate_limit_bytes_per_second`)
— used **only as a best-effort scheduling hint**. The `step_capacity =
min(remaining_daily_budget, link_speed × step_seconds)` formula is computed from
*reported* telemetry, **not** treated as an authoritative coordinator quantity.
**The donor-local token-bucket stays authoritative (D11/T1.12)** and may still
refuse `budget_exceeded` regardless of any coordinator reservation. The design is
explicit: *the coordinator reserves optimistically; the donor decides.*

### D-M5-7 — One placement engine, over `pkg/coordinator/admission/assigner.go`.

There is exactly one placement system. M4.1's `pkg/coordinator/admission/assigner.go`
(whose own comment reads "Anti-affinity, healing, and the async commit gate are
M5/Task 11") grows into the placement engine; M4.1's interim admission selection
is replaced, not duplicated. **`internal/api/handlers/admission.go` is
upload-concurrency admission and stays out of M5.** The engine owns: **soft
failure-domain anti-affinity** (prefer a distinct `failure_domain_id` →
`donor_principal_id` → `provider` → `asn` → `region`; a **preference, never a
veto** — place into the best available rather than block healing); **trust /
probation caps** (probationary never the sole or second copy of `important`,
T1.30); `policy_filters` + capacity checks; a **steady-state placement weight
`~sqrt(free_capacity) × trust`, decoupled from bandwidth** (the direction is
settled by the resilience analysis; the exact form is a Tier-2 tunable **calibrated
in M5**); **repair-source** weighting by available egress × reputation; and the
`reputation_floor` exclusion (nodes below the floor get no new assignments, and
their acked pins are scheduled for re-replication).

#### D-M5-7-CAP — Self-reported free capacity is a hint, not authority.

Placement uses `last_free_bytes` + trust, but free space is **donor self-reported**
— a malicious donor could lie to attract assignments. So `last_free_bytes` is a
**feasibility hint**; the **donor-side `storage_max_bytes` + actual accept/refuse
stays authoritative** (a donor refuses `out_of_space` regardless of what it
advertised); repeated `out_of_space` / mismatch failures feed **P2-M6**
reputation/audit inputs. Trust caps (D-M5-3a) already bound how much a probationary
liar can capture.

#### D-M5-7a — Replication-factor validation, class-aware.

Today only `factor.important` is range-checked and `R=1` is permitted
(`internal/config/operator_yaml.go:144`). M5 validates **all three** factors
`[1,20]`, and the `R<2` rule is **class-aware** (not blanket): **refuse `R<2` for
`important`** (irreplaceable user-uploaded originals — `R=1` makes "one failure
from loss" the steady state and defeats Tier-1), and **warn-not-force for
`normal`/`cache`** (regenerable derivatives and transient artifacts, which can be
rebuilt). The design states the class taxonomy explicitly so the asymmetry is
intentional.

### D-M5-8 — Donor↔donor repair: generalize the M4.1 source server, do not duplicate it.

`internal/node/source` (the M4.1 coordinator-only read-source) is extended to
accept `transport.RoleNode` callers (donor repair) in addition to
`RoleCoordinator` (coordinator read), gated by `token.dest_node_id == verified
requester id`. On an `assign` change whose `source.node_id` is another **donor**,
the destination donor fetches from that donor over Nebula under the
coordinator-minted token → deterministic re-import + root-CID verify → pin →
persist → ack (the existing M4 `internal/node/transfer` loop). This is the **only**
sanctioned repair path; Bitswap-backed `ipfs pin add` for repair stays disabled.

**D11 wording.** M4.1 already introduced the donor-authoritative egress debit for
**read-source** serving; M5 **extends the same donor-local token-bucket** to
donor↔donor **repair** serving. This is the first *repair* debit, **not** the
first donor egress debit overall.

#### D-M5-8e — Repair-token assignment semantics: the token names the *source* assignment.

A repair grant spans **two** assignments — the source donor's existing **acked**
assignment (its proof of legitimate possession) and the destination donor's
**pending** assignment (what the destination will ack). `wire.Claims` carries a
single `AssignmentID`/`Generation` today, and the M4.1 source server verifies it
against the **source's own** local progress —
`prog.State == ProgressAckDelivered && prog.AssignmentID == claims.AssignmentID &&
prog.Generation == claims.Generation` (`internal/node/source/server.go:173-176`).
That semantics carries forward unchanged and is made explicit: **`Claims.AssignmentID`
/ `Generation` always name the *source* donor's acked assignment**, because the
serving side is the only side that can verify possession against a token claim. The
**destination** assignment stays carried by `PinChange.AssignmentID`/`Generation`
(and the donor's resulting `Ack`), exactly as in M3/M4.

To make cross-generation and replay reasoning explicit rather than implicit, M5
**additively** extends `Claims` with optional `DestAssignmentID` / `DestGeneration`
(omitempty; the coordinator binds the destination assignment into the grant it
mints). This is a backward-compatible field addition — `Claims` marshals
deterministically and old verifiers ignore unknown-but-absent fields; a coordinator
that sets them and a donor that checks them gain explicit destination binding, and
a donor that does not check them still verifies correctly against the source claim.
For an emergency local-origin source (D-M5-8b) the source assignment is the
reserved `wire.CoordinatorSourceID` identity with the coordinator's progress
standing in for `ProgressAckDelivered`.

#### D-M5-8a — Durable repair-source binding that survives snapshot recovery.

`AssignPin` writes **no source** into `pin_changes` today
(`internal/federation/coordinator/assignments.go:42,65`); `/pins/changes` always
mints a coordinator-source token. Storing the source only in `pin_changes` fixes
incremental delivery but **strands a long-offline donor that recovers via
snapshot** after change-log retention — the snapshot wire item and query carry no
source (`SnapshotItem`, `internal/federation/wire/messages.go:152`;
`getPinSnapshotPage`, `internal/db/gen/federation.sql.go:293`). So the source must
live with the **current desired assignment**, not only the change log:

- Add **`pin_assignments.source_node_id`** (nullable FK to `nodes(id)`) — the live
  assignment source. Required for `pending` assignments; for `acked` it is
  historical / non-authoritative. **A NULL value means coordinator-sourced**; the
  reserved `wire.CoordinatorSourceID` is a synthetic non-`nodes` constant and is
  **never** stored in this FK column — `/pins/changes` translates NULL into
  `ChangeSource.NodeID = wire.CoordinatorSourceID` on the wire.
- **Copy** `source_node_id` into `pin_changes` for incremental delivery/audit, and
  **extend `SnapshotItem` + the snapshot SQL projection** so snapshot recovery
  reconstructs pending-assignment sources after change-log retention.
- Affected protocol surfaces (named so snapshot sync is not missed):
  `PinChange.Source`, `SnapshotItem.Source`, `AssignPin` → `AssignPinWithSource`,
  `pin_assignments.source_node_id`, `pin_changes.source_node_id`, the
  `GetPinSnapshotPage` source join/projection.
- **Backfill:** existing **pending** assignments created before M5 are
  coordinator-sourced unless the M5 scheduler rewrites them; existing **acked**
  assignments may have NULL `source_node_id`.
- The scheduler stores the source **node id** (not the nebula address);
  `/pins/changes` mints with the source's **current** address at delivery; if the
  stored source is **no longer sourceable**, the assignment is **requeued /
  rewritten** (never silently substituted), with a max-retry / backoff (stale-source
  counter) to avoid tight rewrite loops when a source flaps.

#### D-M5-8b — Local-origin emergency repair source.

Coordinator local copies never count toward `R`/`healthy_acked_count`, **but**
when `healthy_acked_count == 0 ∧ local_recoverable` the coordinator may serve as
the **emergency repair source** to rebuild donor replicas (reusing M4
coordinator-as-source). `healthy_acked_count == 0 ∧ ¬local_recoverable` ⇒
`donor_lost` — do not spin the queue. `local_recoverable` is computed:
`blob_storage_state.local_present = true ∧ local_role ∈ {origin,staging,cache} ∧
the blob state is repair-eligible (D-M5-RE) ∧ backend.Has(cid)` (or reconciliation
confirms the pin) — not merely "a row says present."

#### D-M5-8c — `repair-stream/v1` is a capability transition, not just "now advertised."

The capability is reserved in M4 (`wire.CapRepairStream`) and unadvertised. M5
makes it operative with explicit mixed-version rules: a donor **without**
`repair-stream/v1` can still receive **coordinator-as-source** assignments (if a
source copy exists) but **cannot be selected as a repair source**; the coordinator
**requires** `repair-stream/v1` only when the deployment posture needs donor↔donor
repair. Making it globally required at registration would make mixed-version
upgrades brittle; if M5 chooses to require it, the design says so deliberately.

#### D-M5-8d — Donor source oversize hardening.

Today the donor source does `io.Copy(w, io.LimitReader(rc, size+1))` and detects
oversize only *after* `size+1` bytes are on the wire
(`internal/node/source/server.go:230`). For repair, harden the serve to stream
**exactly `size`** (or validate before writing), **and** add a test proving the
**destination** donor refuses oversize content and does **not** ack. (The
destination's deterministic-re-import root-CID verify is the backstop, but the
"stream exactly size" comment must become true.)

### D-M5-RE — Repair-eligible blob states.

The M4 source preflight serves `b.state = 'active'` only
(`internal/db/gen/federation.sql.go:133`), while the slow-attrition corpus
calculation counts `active` + `quarantined` (`HEALING_PROTOCOL.md`). M5 resolves
the ambiguity, keeping **donor-repair eligibility separate from public read
visibility**: **heal `active` + `quarantined`/legal-hold**. Quarantined / legal-hold
material may need durable preservation as evidence; donors hold **ciphertext only**
and public reads stay blocked, so durable replication does **not** imply public
readability. Tombstoned / soft-deleted blobs stay non-eligible (crypto-shred /
pending disposition). The design states the active↔public-visibility split
explicitly.

### D-M5-9 — Webhook dispatcher: a best-effort, signed-when-configured emitter; no durable outbox.

M5 ships the first webhook dispatcher (`internal/notify`): an `Emitter` interface
+ a `BestEffortHTTP` implementation issuing **one bounded HTTP POST per event**
(per-destination timeout), with an **HMAC request signature when
`WebhookDestination.Secret` (`secret_file`) is set**
(`internal/config/types.go:383`), a **durable suppression-window store** (a small
table) so once-per-window semantics survive restart, and slog + a failure counter
on error. **No** persistent outbox, retry worker, or delivery guarantee — webhooks
are notifications, not control plane; the emitter is a protected integration point
that never cascades into healing. It honors the existing paranoid gating (webhooks
honored only when `paranoid=false`). Events: `federation.node_revoked` (via
D-M5-4-REVOKE-OBS), `federation.degraded` (mass-casualty), `federation.shrinking`
(slow-attrition), `federation.concentrated` / `federation.homogeneous` (Tier-2).

#### D-M5-9a — Suppression keys are scoped.

The suppression store must **not** suppress all events of a type globally (node
A's revocation must not swallow node B's). The key is
`event_type + destination + scope_key`, where `scope_key` is: `node_revoked` →
`node_id`; `degraded` → `federation/global`; `shrinking` → `limiting_class` (or
global); `concentrated` → `dimension + bucket/value`; `homogeneous` → `dimension`.
Once-per-window applies per scoped key, so distinct events are not lost.

### D-M5-10 — Concentration metrics (a HEALING_PROTOCOL "MUST").

The healing/metrics layer emits the inputs to the Tier-2 `federation.concentrated`
/ `federation.homogeneous` webhooks: per-node pin-incidence **Gini**; and
per-dimension (`donor_principal` / `failure_domain` / `provider` / `asn` /
`region`) **largest_share**, **top_k_share**, and **normalized entropy** (entropy
distinguishes "one dominant provider" from "many evenly distributed providers").
Computed periodically and bounded over the projection ⨝ `pin_assignments` ⨝
`nodes`. **Unverified / NULL dimension values collapse into the single `unknown`
bucket *before* Gini/entropy** — the same collapse anti-affinity uses (D-M5-3a) —
so the placement engine and the concentration metrics never disagree about what
counts as one domain. Placement never refuses a replica purely for homogeneity (a
hard ceiling could block healing into the only surviving capacity during a
casualty). Numerical edge cases are specified and tested: zero pins, one node,
all-`unknown` dimension, the `log(1)` normalized-entropy denominator,
`k > #groups`, NULL/empty dimensions.

### D-M5-11 — Mass-casualty and slow-attrition detection, with corrected runway math.

**Mass-casualty:** a burst of `active→unreachable` transitions exceeding
`mass_casualty_threshold_ratio` of active nodes within `mass_casualty_window_seconds`
emits `federation.degraded` once per window — a notification only; **no budget
override** (T1.12). **Slow-attrition:** the spec's
`runway_days = surviving_daily_budget / (corpus_bytes × R)` is dimensionally
`1/day`, **not** days. M5 replaces it with named, dimensionally-correct metrics,
**split across the limiting axes** (attrition is not only an egress problem):

```
repair_time_days   = desired_replicated_bytes / surviving_daily_egress_budget   # days to full rebuild
daily_rebuild_frac = surviving_daily_egress_budget / desired_replicated_bytes   # the old (mislabeled) scalar
storage_headroom   = surviving_free_donor_bytes / projected_required_replica_bytes
active_node_trend  = current (active+suspect)  vs  trailing-28d baseline
```

`federation.shrinking` carries all three (egress runway, storage headroom, node
trend) so a healthy-egress-but-storage-starved federation is not misread as fine.
Once-per-24h with re-arm. The `HEALING_PROTOCOL.md` runway formula/label is
corrected as part of D-M5-14 (it is a spec-level bug, not only an implementation
choice).

### D-M5-12 — warn-not-force emission for `replication.factor.important < 5`.

The default `important` R is 5; lowering it is allowed but **warn-not-force**
(P2-M2.1). The HEALING_PROTOCOL implementation note deferred the *emission* of
that warning to P2-M5, "wired when the orchestrator consumes the replication
factor." M5 emits it where the orchestrator reads `R`, surfaced alongside the
existing `PrivacyWarnings`.

### D-M5-13 — Observability is slog evidence + metric-shaped values; no Prometheus `/metrics`.

Per the M4.1 style, M5 emits structured slog evidence and metric-shaped fields but
**does not** build the `/metrics` surface (that is P2-M7; this set is its
blueprint): `orchestrator.tick.*`, `liveness.transition.*`,
`heal.{scheduled,source_selected,skipped_budget}`,
`repair.{served,refused,fetched}`, `fed.concentration.*`,
`federation.{degraded,shrinking,node_revoked,concentrated,homogeneous}`. The values
exist, are tested, and feed the webhooks/logs.

### D-M5-14 — Spec amendments and drift fixes.

The design mandates: amend `HEALING_PROTOCOL.md` (cache ≠ replica; repair-source =
donors; calibrate the placement-weight formula; **fix the runway formula/label**
per D-M5-11), `FEDERATION_PROTOCOL.md` (donor↔donor repair now live; the
`repair-stream/v1` transition; the repair egress first-debit; heartbeat telemetry
fields; the snapshot `source` field), `DATA_MODEL.sql` (`blob_replication_state`,
the reconcile queue, D8/D9 columns, `pin_assignments.source_node_id`,
`nodes.assignment_sync_state`), and `THREAT_MODEL.md` (Sybil / failure-domain
forgery and repair-token forgery/replay are now code-enforced). It **fixes the
real drift at `ARCHITECTURE_DECISIONS.md:161`** — `replication.factor.important`
default `3 → 5`, matching `HEALING_PROTOCOL.md` and `config.DefaultReplicationImportant`
(P2-M2.1 raised the default but missed this table). It marks `ROADMAP.md` P2-M5
done.

## Scope

**In scope.** Migration 0014 (D8/D9 `nodes` columns, `blob_replication_state`,
`blob_replication_reconcile_queue`, `pin_assignments.source_node_id`,
`nodes.assignment_sync_state` / `revoked_signaled_at`, backfill) (D-M5-3);
the `blob_replication_state` projection + its in-transaction single-CID writers,
bounded async bulk reconciliation, status-based sourceability, and config-change
recompute (D-M5-2/2a/2b/2c/2d); the 5-state liveness sweeper + recovery /
re-registration / countability guard / endpoint matrix / revocation fallout +
observation (D-M5-4/4a/4-REVOKE/-OBS/5); the healing orchestrator tick loop with
strict Tier-1, asymmetric source selection, trickle pacing as a telemetry-fed
hint, best-effort reservation + repair-token mint + assign-with-source, and
warn-not-force R emission (D-M5-6/6-TEL/12); the single placement engine over
`pkg/coordinator/admission/assigner.go` with anti-affinity, unknown-domain
collapse, trust/probation caps, bandwidth-decoupled weight, free-capacity-as-hint,
reputation_floor, and class-aware R validation (D-M5-7/7-CAP/7a/3a); donor↔donor
repair by generalizing `internal/node/source` (RoleNode + dest binding + oversize
hardening), the D11 repair egress debit, the `repair-stream/v1` transition, durable
source binding across incremental + snapshot with requeue-on-loss, local-origin
emergency source, and the repair-eligible-state policy (D-M5-8/8a/8b/8c/8d/RE);
heartbeat egress telemetry (D-M5-6-TEL); the best-effort signed webhook dispatcher
+ scoped suppression (D-M5-9/9a); concentration metrics + edge cases (D-M5-10);
mass-casualty + corrected slow-attrition detection (D-M5-11); the slog evidence
set (D-M5-13); `novactl node` operator-verified-domain subcommands; the
`donor-deps-boundary` allowlist for the generalized source server + heartbeat
telemetry; functional chaos E2E; doc amendments (D-M5-14).

**Out of scope (owning milestone).**

- Possession audits, reputation *updates*, and trust *graduation* — **P2-M6**. M5
  *consumes* `reputation_score`/`trust_state`; M6 moves them. The `pin_audits`
  receive-time / sampling-weight columns (D10) land in M6, not 0014.
- Corpus-scale (≈9.8M-row) benchmarks for manifest insert / replication-state /
  selection — **P2-M6/M7**. M5 ships query/fixture tests, not scale regression
  thresholds (at most an optional non-blocking M5 benchmark script).
- Prometheus `/metrics` — **P2-M7** (the D-M5-13 set is its blueprint).
- Multi-coordinator HA / leader election / control-plane fencing — **Phase 6**. M5
  stays single-leader-per-process, forward-compatible.
- Streaming-AEAD v2 / CAR transfer / `Importer(io.Reader)` — **P2-M8+**.
- Client-direct donor reads (would require client-side keys) — a permanent non-goal
  (T1.26).

## Wire reconciliation

- **Capability transition.** `wire.CapRepairStream = "repair-stream/v1"` moves from
  reserved to operative (D-M5-8c): the donor advertises it, the coordinator records
  and requires it for donor-source repair selection. Mixed-version rules are
  explicit (a non-advertising donor receives coordinator-as-source assignments but
  is never selected as a repair source).
- **Token direction + assignment semantics (D-M5-8e).** Repair grants set
  `source_node_id = the designated source donor` (or `wire.CoordinatorSourceID`
  for an emergency local-origin source), `dest_node_id = the destination donor`,
  and bind `cid`/`max_bytes`/`jti`/`not_before`. `Claims.AssignmentID`/`Generation`
  name the **source** donor's acked assignment (the serving side verifies it against
  its own `ProgressAckDelivered` record); the **destination** assignment stays in
  `PinChange`/`Ack`. `Claims` is **additively** extended with optional
  `DestAssignmentID`/`DestGeneration` (omitempty) for explicit destination binding —
  a backward-compatible field addition, since `Claims` marshals deterministically.
  The coordinator mints (it holds the private key); the source verifies via the
  shared `wire.Verify` + `internal/federation/replay` (M4.1's helper).
- **`HeartbeatRequest`** gains optional egress telemetry —
  `egress_budget_remaining_bytes`, `egress_budget_capacity_bytes`,
  `egress_refill_bytes_per_second` (D-M5-6-TEL). The coordinator persists/uses them
  only as a scheduling hint.
- **`PinChange` and the `assign` change-log entry** gain `source` (node id +
  late-bound nebula address + minted token), per `FEDERATION_PROTOCOL.md`'s
  documented `assign` shape; **`SnapshotItem` gains `source`** so snapshot recovery
  reconstructs pending-assignment sources (D-M5-8a).
- **Donor source endpoint** (`GET /fed/v1/blob/{cid}`) now accepts the `RoleNode`
  destination role as well as `RoleCoordinator`, gated by `dest_node_id` ==
  verified requester id (D-M5-8). Refusals reuse the M4/M4.1 reason domain
  (`source_unauthorized`, `blob_unavailable`, `budget_exceeded`, `blob_too_large`).
- **`wire.Fail`** reasons are unchanged; repair failures reuse `budget_exceeded`,
  `blob_unavailable`, `source_unauthorized`, `cid_mismatch`, `network_error`,
  `kubo_error`.

The `wire` package stays dependency-free (donor-safe).

## Architectural notes

### Liveness sweeper + healing orchestrator (net-new, coordinator-only)

`internal/orchestrator` runs two coordinator background loops on the tick cadence:
`reconcile_node_liveness()` (D-M5-4) and the healing scheduler (D-M5-6).
`reconcile_node_liveness()` is the single freshness authority (D-M5-2a): it applies
the FED v2 timers, performs recovery/eviction/revocation transitions (D-M5-4a /
4a-EVICT / 4-REVOKE-OBS), and on each transition enqueues the affected CIDs onto
the bounded projection reconcile path (D-M5-2d). The scheduler computes Tier-1 /
Tier-2 from `blob_replication_state` (recomputing `dirty` CIDs from authority
first), selects sources asymmetrically using telemetry-hinted `step_capacity`
(D-M5-6-TEL), picks destinations through the placement engine, reserves via
`AssignPin` → `AssignPinWithSource`, mints repair grants, and emits `assign`
entries. It is single-leader-per-process; repair tokens already carry per-assignment
`generation`, forward-compatible with a future control term (Phase 6).

### Placement engine (one engine, replacing M4.1's interim assigner)

`pkg/coordinator/admission/assigner.go` becomes the single placement engine
(D-M5-7), shared by initial admission and healing. Anti-affinity and concentration
both read the operator-verified domain dimensions, collapsing unverified/NULL into
one `unknown` bucket (D-M5-3a / D-M5-10) so the two never disagree. Steady-state
weight is `~sqrt(free) × trust`, decoupled from bandwidth; bandwidth governs
repair-source selection only. Trust/probation caps enforce T1.30.

### Donor↔donor repair (generalized M4.1 source server)

`internal/node/source` accepts a destination-donor caller (D-M5-8) and debits the
donor's authoritative egress bucket (D11) on serve. The destination donor's
`internal/node/transfer` loop fetches under the coordinator-minted grant,
re-imports + verifies the root CID, pins, persists, and acks — exactly the M4 path,
now with a donor source address resolved late from `pin_assignments.source_node_id`
(D-M5-8a). The server imports only donor-boundary-safe packages
(`wire`, `transport`, `replay`, `ipfsclient`, `bandwidth`, `node/*`); the
`donor-deps-boundary` allowlist grows only by the reviewed entries.

### Projection, reconciler, and concentration

`blob_replication_state` (D-M5-2) is written in-transaction by single-CID
assignment writers and asynchronously (bounded) by the bulk reconcile drain
(D-M5-2d); a periodic full reconciliation is the correctness audit. The
concentration computation (D-M5-10) and the slow-attrition runway (D-M5-11) are
bounded periodic reads feeding the webhook dispatcher.

### Webhook dispatcher (net-new)

`internal/notify` is a protected integration point (D-M5-9): bounded per-event
POST, optional HMAC, durable scoped suppression (D-M5-9a), slog + counter on
failure, no outbox/retry. It never blocks or cascades into healing.

### Config (coordinator + donor)

Coordinator (`internal/config` + hot-reload + `/settings`): the orchestrator
timers / thresholds (already-present `Orchestrator` block — `tick_interval_seconds`,
`mass_casualty_*`, `capacity_runway_floor_days`, `replication.factor.*`), the FED
liveness timers (`suspect_after_missed_heartbeats`, `unreachable_after_seconds`,
`evicted_after_seconds`), placement selection mode + `reputation_floor`, and the
`webhooks` destinations the dispatcher consumes. `prune_stale_seconds` is
deprecated/reinterpreted (D-M5-2a). Validation rejects unsafe combinations
(class-aware R, D-M5-7a). Donor (`node.yaml`): the egress budget (present from M1)
now also debited for repair; optional egress-telemetry reporting on heartbeat.

**Operator-surface simplicity (normative).** The liveness/healing knobs ship with
safe defaults from the normative specs; an operator must not have to tune a dozen
distributed-systems controls to keep data durable. The first-class `/settings`
exposure stays small (replication factors, mass-casualty/runway thresholds,
reputation floor); timers and selection internals remain advanced/defaulted.

## Component ownership

| Concern | Coordinator (operator) | Donor | Notes |
|---|:--:|:--:|---|
| 5-state liveness sweeper | ✓ | — | `internal/orchestrator` (D-M5-4) |
| healing scheduler (Tier-1 strict, source select, pacing) | ✓ | — | reads projection; `AssignPin` seam (D-M5-6) |
| `blob_replication_state` projection | ✓ | — | rebuildable cache; authority = `pin_assignments` ⨝ liveness (D-M5-2) |
| placement engine (anti-affinity, trust caps, weight) | ✓ | — | `pkg/coordinator/admission/assigner.go` (D-M5-7) |
| repair-grant **mint** (source-designation) | ✓ | — | `internal/federation/tokens` (D-M5-6) |
| repair-grant **verify** + replay | — | ✓ | shared `wire.Verify` + `internal/federation/replay` |
| `GET /fed/v1/blob/{cid}` **repair source** | — (emergency local-origin only) | ✓ | generalized M4.1 server (D-M5-8) |
| donor↔donor repair fetch → verify → pin → ack | — | ✓ | M4 `internal/node/transfer` loop |
| authoritative egress budget (repair debit) | — (best-effort reserve) | ✓ | `bandwidth.Bucket`, D11/T1.12 (D-M5-6-TEL) |
| egress telemetry (scheduling hint) | ✓ (consumes) | ✓ (reports) | heartbeat fields (D-M5-6-TEL) |
| webhook dispatcher | ✓ | — | `internal/notify`, best-effort (D-M5-9) |
| concentration metrics / slow-attrition runway | ✓ | — | feeds Tier-2 + shrinking webhooks (D-M5-10/11) |
| operator-verified failure-domain fields | ✓ | — | `novactl node`; self-declared geo informational (D-M5-3) |

## Forward-compatibility

- **P2-M6** consumes the acked/sourceable holder set and donor block access for
  possession audits, and **moves** `reputation_score` (feeding D-M5-6 source
  selection and D-M5-7 placement) and **graduates** `trust_state`
  (probationary→trusted), which M5 only reads. Audit-aware placement plugs into the
  D-M5-7 engine.
- **P2-M7** promotes the D-M5-13 slog/metric-shaped set to Prometheus `/metrics`,
  and runs the corpus-scale benchmarks M5 deferred.
- **Phase 6** adds control-plane fencing (`coordinator_leases(term)`,
  `jobs.lease_id`); M5's single-leader-per-process orchestrator and the
  per-assignment `generation` on repair tokens are forward-compatible — every
  control mutation can later carry the current term without re-architecture.
- **P2-M8+** may replace whole-buffer repair transfer with CAR / `Importer(io.Reader)`.

## Trust-model notes (Phase 2 adversaries; canonical text in `THREAT_MODEL.md`)

- **Donor-blind preserved (T1.26).** Repair moves ciphertext between donors under
  coordinator-minted grants; the coordinator stays the sole decrypt/serve point. No
  donor plaintext path and no client-direct donor read path are added.
- **Repair-token forgery / replay (D1).** Repair grants are coordinator-minted and
  donor-verified (Ed25519, asymmetric — donors hold only the public key);
  source/dest/cid/assignment/generation/max_bytes-bound; single-use `jti` +
  `source_boot_time` floor + short TTL via the shared `replay` helper. A malicious
  destination cannot replay a grant to drain a source's budget.
- **Bandwidth exhaustion (D11 / T1.12).** The donor's authoritative egress bucket
  debits per repair serve and refuses `budget_exceeded` regardless of any
  coordinator reservation; the coordinator's `step_capacity` is a telemetry-fed
  hint only (D-M5-6-TEL). No doomsday override exists, even under mass-casualty.
- **Sybil / failure-domain forgery (D8/D9).** Anti-affinity trusts only
  operator-verified domains; unverified/NULL collapse to one `unknown` bucket
  (D-M5-3a) so blank/varying self-declared fields cannot evade diversity;
  probationary nodes are never the sole or second copy of `important` (T1.30);
  self-reported free capacity is a hint, not authority (D-M5-7-CAP). Concentration
  metrics surface residual correlation for operator action (D-M5-10).
- **False durability / premature healing decisions.** Durability is acked-only on
  **countable** nodes — `status IN ('active','suspect')` **and**
  `assignment_sync_state='current'` (D-M5-2a/4a) — so a reactivated node's stale
  acks never count before reconciliation, and the operator cache never counts as a
  replica. Repair-source selection and pruning both reason over **sourceable live**
  holders.
- **Wrong / tampered repaired bytes.** Caught by deterministic re-import + root-CID
  compare at the destination donor before it acks (D-M5-8); a hardened source serve
  never puts oversize bytes on the wire and the destination refuses oversize
  content without acking (D-M5-8d).
- **Lost evidence under quarantine.** Quarantined / legal-hold blobs remain
  durable (repair-eligible) without becoming publicly readable — repair eligibility
  and read visibility are separate (D-M5-RE).

## Exit criterion

With a coordinator and ≥2 donors over loopback-mTLS, `important` R configured, and
a blob replicated and acked on multiple donors so `blob_replication_state` reads
`healthy`:

1. **Liveness.** Killing a donor transitions it `active→suspect→unreachable` within
   the configured timers; the projection drops its acks for the affected CIDs (only
   those CIDs are touched, not a full scan), and a CID left at one healthy holder is
   classified `tier1`.
2. **Healing.** The orchestrator processes Tier-1 strictly first, selects a source
   asymmetrically (capacity × reputation, telemetry-hinted), picks a destination
   under soft failure-domain anti-affinity and trust caps, reserves via
   `AssignPin` → `AssignPinWithSource`, and mints a repair grant — **without
   exceeding any donor's authoritative budget** (the source donor refuses
   `budget_exceeded` if over budget).
3. **Donor↔donor repair.** The destination donor fetches the **ciphertext envelope**
   from the **source donor** over the coordinator-minted grant (debiting the source
   donor's egress budget), verifies by deterministic re-import + root-CID compare,
   pins, persists, and acks; the projection returns the CID to `tier2`/`healthy`.
   An oversize source serve is refused and the destination does not ack.
4. **Recovery.** A returned node flips `unreachable→active` on heartbeat but does
   **not** count toward durability until it reconciles its desired set to the
   current epoch (`assignment_sync_state='current'`); a long-offline node that
   recovers via **snapshot** still learns its pending-assignment source
   (`SnapshotItem.source`).
5. **Eviction / revocation.** An evicted node's live assignments are deleted (after
   heal-enqueue); a revoked node's rows are retained but non-counting, the affected
   CIDs are enqueued for healing, and `federation.node_revoked` fires exactly once —
   even though `novactl node revoke` is DB-direct.
6. **Signals.** A provider-purge burst emits `federation.degraded` once per window
   (no budget override) while the bounded projection drain keeps up; a corpus whose
   surviving egress runway / storage headroom falls below the floor emits
   `federation.shrinking` once per 24h with the corrected metrics; a skewed
   placement fixture trips `federation.concentrated`/`homogeneous` off the Gini /
   per-dimension entropy; node A's revocation does not suppress node B's
   (scoped suppression keys).
7. **Invariants hold.** Cache/origin copies never counted toward `R`; the
   coordinator served no unverified bytes; no donor exceeded its budget; the
   `donor-deps-boundary` and `migrations-frozen` gates stayed green; and a
   `donor_lost ∧ ¬local_recoverable` CID did not spin the healing queue.
