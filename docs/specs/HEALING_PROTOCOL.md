# Healing Protocol

Status: **Phase 0 v3 — normative.** Specifies the orchestrator's
bandwidth-aware healing algorithm. `internal/orchestrator` (Phase 2)
implements this protocol. The simulation under
`simulations/orchestrator_resilience.py` is the empirical reference;
its conclusions (network-size thresholds, priority-queue
effectiveness) are baked into the parameters below.

> **Amended by P2-M0 (2026-06-13)** — durability classification is now
> **acked-only** (D5; pending never lifts a CID out of Tier 1); placement
> gains **soft failure-domain anti-affinity** and a steady-state weight
> **decoupled from donor bandwidth** (D8; bandwidth = repair-source selection
> only, exact formula a Tier-2 tunable calibrated in P2-M5); an orthogonal
> **`trust_state`/probation** placement cap (D9); best-effort reservations with
> a donor-authoritative budget (D11); and a durable **`blob_replication_state`**
> projection instead of per-tick full scans. See
> `docs/superpowers/specs/phase2/2026-06-13-phase2-m0-spec-reconciliation-design.md`.

> **Amended by P2-M6 (2026-06-29)** — `reputation_score` now **moves**:
> possession audits (`internal/audit/possession`) increment it on pass,
> decay it on soft failure (deadline), halve it on hard failure (404 /
> not-present), and zero it on hash mismatch. Audit recency feeds into source
> selection ordering only as a **bounded preference** (the existing
> `reputation_score`-ordered `ListSourceableHolders` / `ListRepairSourceHolders`
> queries already carry it; no new countability term, no direct audit-recency
> term in the placement engine — D-M6-9). Acked-only countability
> (`assignment_sync_state='current'`) is **unchanged** from M5. A node below
> `reputation_floor` is excluded from new placement but its existing acked pins
> remain countable unless a pin-specific hard audit failure explicitly invalidates
> that row (`pin_assignments.state → 'failed'`); bulk re-replication of
> below-floor replicas is **deferred to P2-M7**. See
> `docs/superpowers/specs/phase2/2026-06-29-phase2-m6-possession-audits-design.md`
> § D-M6-7 and D-M6-9.

## Purpose

When a federation loses donor nodes — to a hosting provider purge, a
network partition, an extended outage, or operator-initiated
revocation — the orchestrator restores replication for every
under-replicated CID without ever overriding a donor's bandwidth
budget.

This document specifies:

1. The data the orchestrator reads and the state it maintains.
2. The trigger conditions and the steady-state loop.
3. The Tier-1-priority queue rule.
4. The asymmetric source-selection algorithm.
5. The trickle-pacing rule that keeps small donors safe.
6. Cross-cluster mass-casualty event semantics.
7. Configuration parameters, including content-class-keyed
   replication factors.

## Inputs and state

The orchestrator runs in the coordinator process and reads only from
the storage core's database. It keeps **no persistent in-process state**; the
authoritative durability view is a **durable `blob_replication_state`
projection** (per-CID acked / in-flight / target counts + safety tier) updated
**in the same transaction** as assignment/liveness changes, so the tick loop
does **not** re-aggregate `pin_assignments ⨝ nodes ⨝ blobs` from scratch every
tick (prohibitively costly at millions of CIDs). A periodic full reconciliation
rebuilds the projection as a correctness audit. The per-tick SQL below is the
*logical* definition of the counts the projection maintains, not a literal
per-tick full scan.

Per-tick reads:

```sql
-- Healthy nodes: those whose state is not unreachable/evicted/revoked.
-- 'suspect' is included to avoid flapping during transient drops.
SELECT n.id, n.bandwidth_budget_bytes_per_day, n.reputation_score,
       n.policy_filters
  FROM nodes n
 WHERE n.status IN ('active', 'suspect');

-- Acked + pending pin counts per CID, only counting healthy nodes.
SELECT pa.cid,
       count(*) FILTER (WHERE pa.state = 'acked')   AS acked_count,
       count(*) FILTER (WHERE pa.state = 'pending') AS pending_count
  FROM pin_assignments pa
  JOIN nodes n ON n.id = pa.node_id
 WHERE n.status IN ('active', 'suspect')
 GROUP BY pa.cid;

-- Durability classification is ACKED-ONLY (D5; Tier-1 T1.14).
-- A pending assignment is dispatched but NOT durable: a CID with one acked
-- copy is one failure from loss no matter how many pins are pending, so
-- pending pins NEVER lift a CID out of Tier 1. pending_count is used only as
-- an in-flight reservation to avoid double-scheduling the same CID.
durable_count = acked_count
```

In-process derived state (per tick):

- `tier1[]` — CIDs with `0 < durable_count < 2` (exactly one acked copy among
  healthy nodes). Pending assignments do not change this set.
- `tier2[]` — CIDs with `2 <= durable_count < target_replication`.
- In-flight `pending_count` is consulted only to skip CIDs that already have
  enough pending reservations in progress — never to reclassify their tier.
- `node.step_capacity` — `min(remaining_daily_budget, link_speed * step_seconds)`
  per surviving node, computed from acked egress in the trailing 24h.

## Tick loop

```
EVERY tick_interval_seconds:

  reconcile_node_liveness()    # transitions active↔suspect↔unreachable↔evicted

  tier1, tier2 = compute_under_replicated()

  IF tier1 IS empty AND tier2 IS empty:
      sleep
      continue

  step_caps = compute_step_capacities()
  IF max(step_caps.values()) <= 0:
      # everyone is at their daily ceiling; do nothing this tick.
      sleep
      continue

  # Strict priority: Tier 1 first, fully, before any Tier 2 work.
  drain(tier1, target_pins_after = 2)
  IF tier1_now_empty():
      drain(tier2, target_pins_after = target_replication_for_blob(cid))

  detect_mass_casualty()
```

### Per-blob target replication

`target_replication_for_blob(cid)` is content-class-keyed:

| Content class | Default `R` | Determined by |
|---|---|---|
| `important` | 5 | `blobs.parent_cid IS NULL` AND `blobs.product = 'image'` (i.e., user-uploaded originals) |
| `normal`    | 3 | `blobs.parent_cid IS NOT NULL` AND `blobs.product = 'image'` (derivatives) |
| `cache`     | 2 | future: explicit `cache` class for transient artifacts |

Default values are operator-configurable per class:

```yaml
orchestrator:
  replication:
    factor:
      important: 5        # default; lower toward 3 only if donor capacity is tight
      normal: 3
      cache: 2
    classifier: default   # 'default' classifies by blobs.parent_cid + product
```

The default important `R=5` errs toward durability for irreplaceable
user-uploaded originals: the 6.4 % loss-on-40%-failure result from the
simulation is a function of *low* R, so the default is set high and
operators trade down deliberately. Lowering `important` below the
default is allowed but **warn-not-force** (consistent with the
privacy-preset model in `docs/PRIVACY_AUDIT.md`): the coordinator emits
a startup/admin warning when `important < 5` stating that **lower R
raises the chance of permanent data loss**, while **higher R raises the
storage burden on donor nodes**. `R=3` is the recommended practical
floor for originals when donor capacity is tight; derivatives (`normal`)
and transient artifacts (`cache`) regenerate, so they default lower
(3 and 2). Operators raise `important` toward the 20-replica ceiling as
donor capacity permits.

> **Implementation note.** The default is enforced today (config loader
> default + setup-wizard render). The warn-not-force *emission* is wired
> when the orchestrator consumes the replication factor in P2-M5, and is
> surfaced alongside the existing privacy warnings (`PrivacyWarnings`).

`drain(queue, target_pins_after)` iterates the queue (sorted by
`byte_size` ascending — small files clear faster, the order does
not affect total time-to-empty). For each CID:

1. Look up `holders` — alive nodes (`status IN ('active','suspect')`)
   with `pin_assignments.state = 'acked'` for this CID.
2. Among those, select the one whose `step_capacity >= byte_size`
   AND has the largest remaining `step_capacity * reputation_score`.
   Asymmetric selection, weighted by reputation.
3. If no holder qualifies, skip this CID for this tick (no progress
   possible until either capacity refreshes or a daily budget resets).
4. Pick a destination among alive nodes that do not already hold the CID and
   whose `policy_filters` accept the blob's size and product class, applying
   **soft failure-domain anti-affinity** (D8): prefer a node in a
   `failure_domain_id` (then `donor_principal_id` / `provider` / `asn`) not
   already holding this CID, weighted by the steady-state placement weight
   (see "Reputation and audit-aware placement"). Anti-affinity is a
   **preference, never a veto** — if no distinct domain has capacity, place into
   the best available rather than block healing. Exclude nodes whose
   `trust_state` bars them from this content class (D9). Retry on collision.
5. Insert a `pin_assignments(cid, dest, assignment_id, generation,
   state='pending')` row as a **best-effort reservation** (D11 — the donor's
   local token-bucket is authoritative and may still refuse `budget_exceeded`),
   debit the source's `step_capacity`, mint an **Ed25519** repair-transport
   token carrying `max_bytes` (see `FEDERATION_PROTOCOL.md` § "Repair
   transport"), update `blob_replication_state` in the same transaction, and
   emit a change-log entry of `kind: 'assign'` with the source designation
   embedded.
6. The donor's next `pins/changes` poll picks up the assignment;
   the donor fetches from the designated source via
   `GET /fed/v1/blob/{cid}` over Nebula, verifies the envelope CID,
   pins locally, and acks. **No Bitswap-backed pin add for repair.**

`drain` does not block on acks. It schedules the work and the tick
rate determines responsiveness.

## Why Tier 1 is strict

A CID at one acked pin is **one failure away from total loss**, regardless of
how many pins are *pending* (D5: pending assignments are not durable and never
satisfy the Tier-1 trigger). A CID at two acked pins is non-compliant but safe.
The simulation
confirms (see `simulations/README.md`) that strict Tier-1 priority is
the difference between sub-hour Tier-1 recovery at ~25 nodes versus
multi-hour partial recovery at the same scale when work is
interleaved.

The orchestrator MUST NOT process Tier-2 work in any tick where
Tier 1 is non-empty. A "fairness" override that would interleave
the two queues is explicitly rejected.

## Why bandwidth budgets are inviolable

`bandwidth_budget_bytes_per_day` is set by the donor at registration
and reflects what they have agreed to provide. A "doomsday override"
that exceeds this budget would:

- Trigger their hosting provider's commercial-use heuristics.
- Push residential donors past their ISP's fair-use thresholds.
- Be the wrong fix for the underlying problem (the federation is
  too small) and would erode donor trust at the moment the
  federation most needs it.

If the surviving capacity is insufficient to heal Tier 1 within the
operator's SLA target, the operator's options are:

1. Recruit more donors (the right fix).
2. Tolerate a longer recovery window (acceptable; the SLA is on
   Tier-1 *clearance*, not full R restoration).
3. **Not** override budgets.

## Asymmetric source selection

For each CID needing a transfer, the source is the highest-effective-
capacity holder that has at least `byte_size` capacity available.
Effective capacity weights `step_capacity` by `reputation_score` so
nodes with proven reliability carry more recovery work.

```go
func selectSource(cid string, holders []*Node, stepCap map[NodeID]int64,
                  rep map[NodeID]float64, size int64) *Node {
    var best *Node
    var bestScore float64 = 0
    for _, n := range holders {
        cap := stepCap[n.ID]
        if cap < size { continue }
        score := float64(cap) * rep[n.ID]
        if score > bestScore {
            best = n
            bestScore = score
        }
    }
    return best // nil if no one qualifies
}
```

This concentrates healing work on high-bandwidth, high-reputation
donors during recovery and shields residential or newly-onboarded
donors from egress spikes.

**Repair-source selection is the one place bandwidth/capacity weighting belongs
(D8).** Fast donors do the heavy lifting *during recovery* — but steady-state
replica *placement* is deliberately decoupled from bandwidth (see "Reputation
and audit-aware placement"), so fast nodes stop *accreting* a disproportionate
share of the corpus. The second-pass resilience analysis measured that
bandwidth-weighted placement turns a single high-bandwidth-cohort purge into a
~64 % data-loss event, versus ~7.5 % under diversity-optimized placement.

## Trickle pacing

`step_capacity = min(remaining_daily_budget, link_speed * step_seconds)`.
With `step_seconds = 60` (default), a 50 Mbps residential donor
contributes at most `50/8 * 60 = 375 MB` per tick from its link
budget alone. A 1 Gbps high-bandwidth VPS contributes up to 7.5 GB
per tick from its link, capped lower by daily budget.

This per-tick cap is what makes trickle pacing automatic. The
orchestrator never asks any donor to upload more than its link can
push during the tick window. Donors will not see saturation; their
ISPs will not see commercial-use heuristics.

## Mass-casualty event detection

The orchestrator emits a `federation.degraded` webhook when:

```
let lost = count(node status: active → unreachable within window)
let total = count(active or suspect nodes at window start)
if total > 0 and lost / total > mass_casualty_threshold_ratio:
    emit federation.degraded
```

with default `window = 1 hour`, `threshold_ratio = 0.20`.

The webhook is a notification, not a behaviour change. The
orchestrator's algorithm does not branch on this signal — it
continues to process Tier 1 strictly first, source asymmetrically,
and respect budgets. The webhook lets the operator publicly post-
mortem the event, steer donors away from the responsible provider,
and update the supporters page (if the deployment runs one).

## Slow-attrition detection

Mass-casualty detection catches the burst failure mode: a hosting
provider purge, a regional outage, an angry operator revoking
several donors at once. It does not catch the slow failure mode: a
federation that loses one donor per week for six months and quietly
crosses the threshold below which the surviving network's aggregate
daily budget is insufficient to maintain the target replication
factor across the corpus.

The orchestrator emits a `federation.shrinking` webhook when this
slow-failure threshold approaches. The metric backing it is the
**capacity runway**: how many days of full corpus re-replication
the surviving network's daily budget could sustain.

### Computation (P2-M5 amendment — corrected dimensions, D-M5-11)

The pre-M5 formula `runway_days_c = surviving_daily_budget /
desired_replicated_c` was dimensionally **`1/day`, not days** (bytes/day ÷
bytes). M5 replaces it with metrics that are honest about their units and
split across the limiting axes (attrition is not only egress). Per content
class `c` with replication factor `R_c`:

```
corpus_bytes_c        = SUM(blob_manifests.envelope_size WHERE durability_class = c
                                              AND blobs.state IN ('active','quarantined'))
desired_replicated_c  = corpus_bytes_c * R_c
surviving_daily_egress = SUM(COALESCE(nodes.last_egress_capacity_bytes,
                                      nodes.bandwidth_budget_bytes_per_day)
                              WHERE nodes.status IN ('active','suspect'))
surviving_free_bytes  = SUM(COALESCE(nodes.last_free_bytes, 0)
                              WHERE nodes.status IN ('active','suspect'))

repair_time_days_c    = desired_replicated_c / surviving_daily_egress   # bytes ÷ bytes/day = DAYS
storage_headroom_c    = surviving_free_bytes / desired_replicated_c     # ≥1 means it fits
active_node_trend     = (active+suspect now) vs the trailing-28d baseline
```

`federation.shrinking` fires for class `c` when `repair_time_days_c >
capacity_runway_floor_days` **or** `storage_headroom_c < 1` — so a
healthy-egress-but-storage-starved federation is not misread as fine. The
limiting class is identified in the payload.

### Webhook semantics

```json
{
  "event": "federation.shrinking",
  "limiting_class": "important",
  "repair_time_days": 4.2,
  "storage_headroom": 0.8,
  "active_node_trend": 18,
  "emitted_at": "2026-05-19T14:00:00Z"
}
```

Suppression: the webhook fires at most once per 24-hour window. If the
metrics recover above the floor (donors recruited, budgets/storage
expanded), the next dip below re-arms the webhook. This prevents alert
storms when budgets oscillate near the boundary.

The webhook does not change orchestrator behavior. It is a
notification so the operator can recruit, expand budgets, or
accept the longer recovery window. The orchestrator continues
processing Tier 1 strictly first, respecting budgets — slow
attrition does not justify a doomsday override any more than mass
casualty does. The corrective action is human, not algorithmic.

### Metric

```
nova_federation_capacity_runway_days{content_class="important|normal|cache"}
```

Operators graph this against `capacity_runway_floor_days` for a
visual sense of headroom over time. A federation in healthy
steady state shows runway figures comfortably in the
double-digit days; a federation approaching attrition collapse
shows the runway sliding toward the floor.

## Reputation and audit-aware placement

`reputation_score` (0.0 to 1.0) is updated by Phase 2 possession
audits (implemented in P2-M6; `internal/audit/possession` —
see `POSSESSION_AUDIT.md`). The orchestrator uses it in two places:

1. **Source selection.** Higher-reputation holders are weighted
   more heavily. A node with score 0.5 carries half as much
   recovery work as an equivalent-capacity node at 1.0.
2. **Initial / steady-state placement (D8 — decoupled from bandwidth).** When
   the orchestrator places pins for a newly-uploaded or under-replicated blob,
   the steady-state placement weight is **decoupled from donor bandwidth**:
   bandwidth governs *repair-source selection only*, never placement. The
   normative **direction** is `~sqrt(free_capacity) × trust` with **soft
   failure-domain anti-affinity**; the **exact weight formula is a Tier-2
   tunable, calibrated in P2-M5** against real donor populations and a
   steady-state-churn model (the direction is settled by the resilience
   analysis; the precise form is not). Placement MUST NOT be sampled
   proportional to bandwidth/capacity as the old `capacity * reputation` rule
   did.

**Trust state and probation (D9 — orthogonal to liveness and reputation).** A
node carries a `trust_state` ∈ {`probationary`, `trusted`, `suspended`},
separate from the 5-state liveness enum and from `reputation_score`, with a
`placement_weight` cap. New nodes enter `probationary`: capped data volume,
higher audit cadence, and **never the sole or second copy of `important`-class
data** (so a fresh Sybil cannot capture critical-data custody). Nodes graduate
to `trusted` on age + successful transfers + passed audits; a `suspended` node
takes no new placement. Audit *frequency* being probationary is not enough on
its own — placement *weight* must be capped too.

**Concentration is alerted on, not enforced.** Soft anti-affinity is a
preference; the hard signal is an operator alert. The P2-M5 healing/metrics
layer MUST emit pin-incidence Gini (per node) and per-dimension
(provider / ASN / region / principal) largest-share / top-k / normalized
entropy so the Tier-2 `federation.concentrated` / `federation.homogeneous`
webhooks (see `ARCHITECTURE_DECISIONS.md`) have data. Placement never refuses a
replica purely for homogeneity — a hard ceiling could block healing into the
only surviving capacity during a casualty.

A node whose reputation drops below an operator-configured floor
(default 0.5) is excluded from new assignments. Existing acked pins
on a below-floor node remain countable unless a pin-specific hard
audit failure invalidates the individual `pin_assignments` row; bulk
re-replication of below-floor replicas is deferred to P2-M7
(D-M6-7).

## Empirical thresholds

These thresholds, validated by `simulations/orchestrator_resilience.py`
at 2.4 TB corpus / 40 % failure / 15 % high-bandwidth-VPS mix /
capacity-weighted pin distribution, hold for `R=3`:

| Objective                                   | Within 24 h | Within 1 h | Within 5 min |
|---|---|---|---|
| Tier 1 cleared, uniform failure             | ~10 nodes   | ~25 nodes  | ~60 nodes    |
| Tier 1 cleared, worst case (provider purge) | ~25 nodes   | ~60 nodes  | ~600 nodes   |
| Full R=3, uniform failure                   | ~25 nodes   | ~40 nodes  | ~400 nodes   |
| Full R=3, worst case (provider purge)       | ~40 nodes   | ~100 nodes | ~1 500 nodes |

For `R=5`, multiply the "Full R" rows by approximately 5/3. The
Tier-1-cleared rows are unchanged because Tier 1 is one transfer
per critical CID regardless of `R`.

**The published SLA target is "Tier 1 cleared," not "Full R
restored."** Tier 1 is the safety condition; full R is administrative
cleanup that may legitimately take days at small scale.

The provider-purge gap (5–10× more nodes needed for the same
recovery window) drives a documented operational rule: the
`OPERATOR_CHECKLIST.md` recommends the high-bandwidth-VPS cohort be
distributed across at least three distinct hosting providers.

## Configuration parameters

Operator-tunable in `operator.yaml` under `orchestrator`:

| Key                                   | Default       | Notes |
|---------------------------------------|---------------|-------|
| `tick_interval_seconds`               | 60            | 5..600 |
| `step_seconds`                        | 60            | Capacity-window for per-tick caps |
| `replication.factor.important`        | 5             | Lower toward 3 only if donor capacity is tight (warn-not-force) |
| `replication.factor.normal`           | 3             | Derivatives, regenerable transforms |
| `replication.factor.cache`            | 2             | Operator-configurable per class |
| `replication.classifier`              | `default`     | Reserved for future custom classifiers |
| `mass_casualty_threshold_ratio`       | 0.20          | 0.05..0.50 |
| `mass_casualty_window_seconds`        | 3600          | 60..86400 |
| `capacity_runway_floor_days`          | 7             | 1..90 — slow-attrition webhook threshold |
| `capacity_runway_check_interval_seconds` | 3600       | 60..86400 — how often runway is recomputed |
| `priority_queue`                      | `strict`      | `strict` only; non-strict is rejected |
| `source_selection`                    | `weighted_capacity_reputation` | also accepted: `random_holder` (debug only) |
| `destination_selection`               | `diversity_anti_affinity` | **(D8)** soft failure-domain anti-affinity + bandwidth-decoupled placement weight; `random_non_holder` accepted (debug); `weighted_remaining_budget` experimental |
| `reputation_floor`                    | 0.5           | Nodes below this excluded from new assignments |
| `pending_weight`                      | 0.5           | **(D5)** orders Tier-2 scheduling only; pending pins do NOT count toward durability and cannot affect the acked-only Tier-1 trigger |
| `placement_weight.mode`               | `diversity`   | **(D8)** steady-state placement weight; `diversity` = `~sqrt(free)×trust` + soft anti-affinity (decoupled from bandwidth; formula calibrated in P2-M5). `capacity_weighted` accepted for A/B only — deprecated, manufactures purge fragility |
| `placement.anti_affinity`             | `soft`        | **(D8)** `soft` only — anti-affinity is a preference, never a veto |

## Restart behaviour

On coordinator restart:

1. Load all `nodes` and `pin_assignments` rows.
2. Recompute `tier1` and `tier2` derived state.
3. Begin the tick loop. No persistent in-flight queue is restored
   because none was persisted; donor nodes' next change-log poll
   re-syncs the assignment view.

There is no leader-election or cluster-replication of the
orchestrator in Phase 2; single-coordinator deployments are the target for the
entire 1.0 line. Multi-coordinator HA is **out of scope for 1.0** and was
reframed (Tier-1 `T1.27`) into a deliberate **post-1.0 Phase 6** —
multi-coordinator *single-authority* HA with exactly one **fenced** control-plane
leader running the orchestrator (independent writable masters remain
prohibited). Phase 2's immutable `assignment_id`/`generation` and durable
`pin_changes` log are forward-compatible prerequisites for that work; see the
phase6 resilience design.

## Out of scope

- Cross-federation replication is **out of scope for 1.0**; each federation is
  autonomous. Tier-1 `T1.28` was reframed into a deliberate **post-1.0 Phase 7**
  — opaque **ciphertext-only** inter-federation peering (peers never receive
  keys, plaintext, catalog, or assignment history; every object has exactly one
  home federation). See the phase6 resilience design.
- Hot-tier / cold-tier auto-migration. Tier classes are designed in
  but only `hot` is implemented; the cold-tier orchestrator is
  Phase 3+.
- Erasure-coded replication (Reed-Solomon) instead of simple
  replication. **Phase 8+ research**; would substantially change the
  data model and interacts badly with the failure-domain diversity this
  protocol is trying to increase.
- Formal storage proofs (PDP/POR). Phase 2 ships challenge-response
  spot-checks (`POSSESSION_AUDIT.md`); formal proofs are **Phase 8+**
  research and not on the MVP path.

## Cross-references

- Schema: `docs/specs/DATA_MODEL.sql` (`pin_assignments`, `nodes`)
- Federation: `docs/specs/FEDERATION_PROTOCOL.md` (5-state liveness, repair transport)
- Audits: `docs/specs/POSSESSION_AUDIT.md`, `docs/specs/INTEGRITY_AUDIT.md`
- Empirical: `simulations/orchestrator_resilience.py`
