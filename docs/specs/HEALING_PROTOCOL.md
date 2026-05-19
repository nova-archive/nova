# Healing Protocol

Status: **Phase 0 v2 — normative.** Specifies the orchestrator's
bandwidth-aware healing algorithm. `internal/orchestrator` (Phase 2)
implements this protocol. The simulation under
`simulations/orchestrator_resilience.py` is the empirical reference;
its conclusions (network-size thresholds, priority-queue
effectiveness) are baked into the parameters below.

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
the storage core's database. It does not maintain any persistent
in-process state; on restart, it rebuilds everything from
`pin_assignments`, `nodes`, and `blobs`.

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

-- Effective replication count (used for tier classification).
-- Pending pins count partially because they are dispatched but not durable.
effective_count = acked_count + 0.5 * pending_count
```

In-process derived state (per tick):

- `tier1[]` — CIDs with `0 < effective_count < 2`.
- `tier2[]` — CIDs with `2 <= effective_count < target_replication`.
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
      important: 3        # operator can raise to 5+ as donor capacity permits
      normal: 3
      cache: 2
    classifier: default   # 'default' classifies by blobs.parent_cid + product
```

The default important `R=3` reflects realistic donor budgets at
launch. Operators with sufficient donor capacity should raise it to
5 for archival-grade durability; the 6.4 % loss-on-40%-failure
result from the simulation is a function of low R, and operators who
care about durability for irreplaceable user content should plan to
move to R=5 once their network has the capacity.

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
4. Pick a destination: a uniformly random alive node that does not
   already hold the CID, whose `policy_filters` accept the blob's
   size and product class. Retry on collision.
5. Insert a `pin_assignments(cid, dest, state='pending')` row, debit
   the source's `step_capacity`, mint a repair-transport token (see
   `FEDERATION_PROTOCOL.md` § "Repair transport"), and emit a change-
   log entry of `kind: 'assign'` with the source designation embedded.
6. The donor's next `pins/changes` poll picks up the assignment;
   the donor fetches from the designated source via
   `GET /fed/v1/blob/{cid}` over Nebula, verifies the envelope CID,
   pins locally, and acks. **No Bitswap-backed pin add for repair.**

`drain` does not block on acks. It schedules the work and the tick
rate determines responsiveness.

## Why Tier 1 is strict

A CID at one acked pin is **one failure away from total loss**. A
CID at two acked pins is non-compliant but safe. The simulation
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

### Computation

Per content class `c` with replication factor `R_c`:

```
corpus_bytes_c       = SUM(blobs.byte_size WHERE content_class = c
                                              AND blobs.state IN ('active','quarantined'))
desired_replicated_c = corpus_bytes_c * R_c
surviving_daily_budget = SUM(nodes.bandwidth_budget_bytes_per_day - nodes.bytes_uploaded_today
                              WHERE nodes.status IN ('active','suspect'))
runway_days_c        = surviving_daily_budget / desired_replicated_c
```

The federation's overall runway is `min(runway_days_c)` across all
classes with non-zero corpus. The minimum-runway class is the
limiting factor and is identified in the webhook payload.

### Webhook semantics

```json
{
  "event": "federation.shrinking",
  "limiting_class": "important",
  "runway_days": 4.2,
  "floor_days": 7,
  "active_nodes": 18,
  "active_nodes_trailing_28d_p50": 27,
  "emitted_at": "2026-05-19T14:00:00Z"
}
```

Suppression: the webhook fires at most once per 24-hour window. If
the runway recovers above the floor (donors recruited, budgets
expanded), the next dip below re-arms the webhook. This prevents
alert storms when budgets oscillate near the boundary.

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
audits (see `POSSESSION_AUDIT.md`). The orchestrator uses it in two
places:

1. **Source selection.** Higher-reputation holders are weighted
   more heavily. A node with score 0.5 carries half as much
   recovery work as an equivalent-capacity node at 1.0.
2. **Initial placement.** When the orchestrator places pins for a
   newly-uploaded blob, candidates are sampled with probability
   proportional to `capacity * reputation`. New nodes start at
   reputation 1.0 (probationary trust); failed audits decrement.

A node whose reputation drops below an operator-configured floor
(default 0.5) is excluded from new assignments and any acked pins
are scheduled for re-replication.

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
| `replication.factor.important`        | 3             | Bump to 5 for archival-grade durability |
| `replication.factor.normal`           | 3             | Derivatives, regenerable transforms |
| `replication.factor.cache`            | 2             | Operator-configurable per class |
| `replication.classifier`              | `default`     | Reserved for future custom classifiers |
| `mass_casualty_threshold_ratio`       | 0.20          | 0.05..0.50 |
| `mass_casualty_window_seconds`        | 3600          | 60..86400 |
| `capacity_runway_floor_days`          | 7             | 1..90 — slow-attrition webhook threshold |
| `capacity_runway_check_interval_seconds` | 3600       | 60..86400 — how often runway is recomputed |
| `priority_queue`                      | `strict`      | `strict` only; non-strict is rejected |
| `source_selection`                    | `weighted_capacity_reputation` | also accepted: `random_holder` (debug only) |
| `destination_selection`               | `random_non_holder` | also accepted: `weighted_remaining_budget` (Phase 2+ experiment) |
| `reputation_floor`                    | 0.5           | Nodes below this excluded from new assignments |
| `pending_weight`                      | 0.5           | Weight applied to pending pins in effective_count |

## Restart behaviour

On coordinator restart:

1. Load all `nodes` and `pin_assignments` rows.
2. Recompute `tier1` and `tier2` derived state.
3. Begin the tick loop. No persistent in-flight queue is restored
   because none was persisted; donor nodes' next change-log poll
   re-syncs the assignment view.

There is no leader-election or cluster-replication of the
orchestrator in Phase 2. Single-coordinator deployments are the
target. Multi-coordinator HA is an explicit non-goal.

## Out of scope

- Cross-federation replication (peering between independent
  operators). Each federation is autonomous.
- Hot-tier / cold-tier auto-migration. Tier classes are designed in
  but only `hot` is implemented; the cold-tier orchestrator is
  Phase 3+.
- Erasure-coded replication (Reed-Solomon) instead of simple
  replication. Phase 6+ research; would substantially change the
  data model.
- Formal storage proofs (PDP/POR). Phase 2 ships challenge-response
  spot-checks (`POSSESSION_AUDIT.md`); formal proofs are Phase 6+
  research and not on the MVP path.

## Cross-references

- Schema: `docs/specs/DATA_MODEL.sql` (`pin_assignments`, `nodes`)
- Federation: `docs/specs/FEDERATION_PROTOCOL.md` (5-state liveness, repair transport)
- Audits: `docs/specs/POSSESSION_AUDIT.md`, `docs/specs/INTEGRITY_AUDIT.md`
- Empirical: `simulations/orchestrator_resilience.py`
