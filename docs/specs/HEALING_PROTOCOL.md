# Healing Protocol

Status: **Phase 0 — normative.** Specifies the orchestrator's
bandwidth-aware healing algorithm. `internal/orchestrator` (Phase 2)
implements this protocol. The simulation under
`simulations/orchestrator_resilience.py` is the empirical reference;
its conclusions (network-size thresholds, priority-queue effectiveness)
are baked into the parameters below.

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
7. Configuration parameters with defaults.

## Inputs and state

The orchestrator runs in the coordinator process and reads only from
the storage core's database. It does not maintain any persistent
in-process state; on restart, it rebuilds everything from
`pin_assignments`, `nodes`, and `blobs`.

Per-tick reads:

- `nodes` rows where `status IN ('active', 'degraded')`.
- `pin_assignments` aggregated as
  `SELECT cid, count(*) FILTER (WHERE state = 'acked') AS acked_count
   FROM pin_assignments JOIN nodes USING (node_id)
   WHERE nodes.status IN ('active', 'degraded')
   GROUP BY cid;`
- `blobs.byte_size` for each CID needing healing (used for capacity
  accounting).

In-process derived state (per tick):

- `tier1[]` — CIDs with `0 < acked_count < 2`.
- `tier2[]` — CIDs with `2 <= acked_count < replication_factor`.
- `node.step_capacity` — `min(remaining_daily_budget, link_speed * step_seconds)`
  per surviving node.

## Tick loop

```
EVERY tick_interval_seconds:

  reconcile_node_liveness()
  # missing heartbeats > max_offline_window → status = 'offline'
  # offline → status = 'active' if heartbeat resumes (within window)

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
      drain(tier2, target_pins_after = replication_factor)

  detect_mass_casualty()
```

`drain(queue, target_pins_after)` iterates the queue (sorted by
`byte_size` ascending — small files clear faster, the order does
not affect total time-to-empty). For each CID:

1. Look up `holders` — alive nodes with `pin_assignments.state = 'acked'`
   for this CID.
2. From those, select the one whose `step_capacity >= byte_size` and
   has the largest remaining `step_capacity`. Asymmetric selection.
3. If no holder qualifies, skip this CID for this tick (no progress
   possible until either capacity refreshes or a daily budget resets).
4. Pick a destination: a uniformly random alive node that does not
   already hold the CID. Retry on collision.
5. Insert a `pin_assignments(cid, dest, state='pending')` row, debit
   the source's `step_capacity`, and increment a metric counter.
6. The donor will fetch and ack on its next poll cycle. The transition
   to `state = 'acked'` causes the next tick to re-derive tiers and
   skip the now-healed CID.

`drain` does not block on acks. It schedules the work and the
tick rate determines responsiveness.

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
   Tier-1 *clearance*, not full R=3 *restoration*).
3. **Not** override budgets.

The simulation's empirical data (below) tells the operator how large
the federation needs to be to hit their preferred SLA.

## Asymmetric source selection

For each CID needing a transfer, the source is the highest-step-
capacity holder that has at least `byte_size` capacity available.
This concentrates healing work on high-bandwidth donors during
recovery and shields residential donors from egress spikes.

A pseudocode reference:

```go
func selectSource(cid string, holders []*Node, stepCap map[NodeID]int64, size int64) *Node {
    var best *Node
    var bestCap int64 = 0
    for _, n := range holders {
        if cap := stepCap[n.ID]; cap >= size && cap > bestCap {
            best = n
            bestCap = cap
        }
    }
    return best // nil if no one qualifies
}
```

The destination, by contrast, is uniformly random among non-holders.
This load-balances received bytes (which become tomorrow's source
capacity) and avoids hot-spotting any single high-budget node.

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
let lost = count(node status: active → offline within window)
let total = count(active or degraded nodes at window start)
if total > 0 and lost / total > mass_casualty_threshold_ratio:
    emit federation.degraded
```

with default `window = 1 hour`, `threshold_ratio = 0.20`.

The webhook is a notification, not a behaviour change. The
orchestrator's algorithm does not branch on this signal — it
continues to process Tier 1 strictly first, source asymmetrically,
and respect budgets. The webhook lets the operator publicly post-
mortem the event, steer donors away from the responsible provider,
and update the supporters page.

## Empirical thresholds

These thresholds, validated by `simulations/orchestrator_resilience.py`
at 2.4 TB corpus / 40 % failure / 15 % high-bandwidth-VPS mix /
capacity-weighted pin distribution:

| Objective                                   | Within 24 h | Within 1 h | Within 5 min |
|---|---|---|---|
| Tier 1 cleared, uniform failure             | ~10 nodes   | ~25 nodes  | ~60 nodes    |
| Tier 1 cleared, worst case (provider purge) | ~25 nodes   | ~60 nodes  | ~600 nodes   |
| Full R=3, uniform failure                   | ~25 nodes   | ~40 nodes  | ~400 nodes   |
| Full R=3, worst case (provider purge)       | ~40 nodes   | ~100 nodes | ~1 500 nodes |

**The published SLA target is "Tier 1 cleared," not "Full R=3
restored."** Tier 1 is the safety condition; full R=3 is
administrative cleanup that may legitimately take days at small
scale.

The provider-purge gap (5–10× more nodes needed for the same
recovery window) drives a documented operational rule: the
`OPERATOR_CHECKLIST.md` requires the high-bandwidth-VPS cohort to be
distributed across at least three distinct hosting providers.

## Configuration parameters

Operator-tunable in `operator.yaml` under `orchestrator`:

| Key                                   | Default       | Notes                                           |
|---------------------------------------|---------------|-------------------------------------------------|
| `tick_interval_seconds`               | 60            | 5..600                                          |
| `step_seconds`                        | 60            | Capacity-window for per-tick caps; usually equals tick_interval. |
| `replication_factor`                  | 3             | 2..7. Increase only with ample donor capacity.  |
| `mass_casualty_threshold_ratio`       | 0.20          | 0.05..0.50                                      |
| `mass_casualty_window_seconds`        | 3600          | 60..86400                                       |
| `priority_queue`                      | `strict`      | `strict` only; non-strict is rejected.          |
| `source_selection`                    | `asymmetric_max_capacity` | also accepted: `random_holder` (debug) |
| `destination_selection`               | `random_non_holder` | also accepted: `weighted_remaining_budget` (Phase 2+ experiment) |

The two `_selection` parameters are exposed as feature flags so
the orchestrator implementation can be empirically compared against
the simulation.

## Restart behaviour

On coordinator restart:

1. Load all `nodes` and `pin_assignments` rows.
2. Recompute `tier1` and `tier2` derived state.
3. Begin the tick loop. No persistent in-flight queue is restored
   because none was persisted; donor nodes' next poll re-syncs the
   assignment view.

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
- Storage-proof challenges (donor proves it actually holds the
  bytes). Acks are taken at face value; periodic random-spot-check
  is Phase 6+ research.
