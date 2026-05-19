# Simulations

Analytical tools that stress-test architectural decisions before they
calcify in production code. Each script is self-contained and uses
only the Python standard library, so it can be checked into the repo
and re-run by anyone.

## Files

| File | Phase 0 spec it validates | What it answers |
|---|---|---|
| `orchestrator_resilience.py` | `HEALING_PROTOCOL.md` § "Empirical thresholds" | How big does the network need to be to clear Tier 1 within target windows? |
| `sybil_concentration.py` | `THREAT_MODEL.md` § A, `POSSESSION_AUDIT.md` | At what sybil ratio does coordinated withdrawal cause data loss? |
| `long_tail_churn.py` | `HEALING_PROTOCOL.md` § "Slow-attrition detection" | Does the `federation.shrinking` webhook provide useful warning lead? |
| `key_rotation_load.py` | `ENCRYPTION_ENVELOPE.md` § "Master key versioning" | How long does `novactl keys rotate-master` take under realistic load? |

### `orchestrator_resilience.py`

Models a federation under a mass-casualty event (a hosting provider
abruptly terminating a fraction of donor nodes) and simulates the
orchestrator's bandwidth-budget-respecting healing process.

The architectural rule the simulator encodes is that **bandwidth
budgets are never overridden, even in doomsday scenarios.** A node
configured for 50 GB/day stays at 50 GB/day even if the network is
hemorrhaging. The simulator therefore answers: *given that constraint,
how big does the network need to be for healing to complete inside
target time windows?*

#### Modeled quantities

| Aspect | Approach |
|---|---|
| Node profiles | Bimodal: "high-bandwidth-VPS-class" (2 TB/day, 1 Gbps link) and "residential-class" (50 GB/day, 50 Mbps link). Mix configurable; default 15 % high-bandwidth VPS. |
| File sizes | Log-normal distribution clamped to `[10 KB, 50 MB]`. Median ~500 KB (configurable). The long tail to 50 MB models high-resolution scans / raw archival uploads. |
| Pin assignment | Capacity-weighted by default — high-budget nodes hold more pins, mirroring how a federation looks after long-term natural drift. Set `--unweighted-pins` for uniform random. |
| Failure event | Removes a configurable fraction of nodes. By default uniform random. With `--profile-bias high-bandwidth-vps` the failure preferentially targets that profile, modelling a single-provider purge of high-bandwidth-VPS-class hosting. |
| Healing | Discrete-time simulation. Each step: (1) reset budgets at day boundaries, (2) compute per-node step capacity = min(remaining-budget, link × step-duration), (3) drain Tier 1 (1-pin CIDs) by selecting the highest-step-capacity holder as source and a random surviving non-holder as destination, (4) only after Tier 1 is empty, drain Tier 2 (2-pin CIDs) the same way. |

#### Reported metrics

- **CIDs lost forever** — every replica died with the failure event.
- **Tier 1 at start / Tier 1 cleared** — number of CIDs reduced to a
  single surviving pin, and the elapsed simulated time before the
  orchestrator restored every Tier 1 CID to ≥ 2 pins. This is the
  "moment the network stops being one-failure-from-loss."
- **Full R=3 restored** — when every CID is back to the configured
  replication factor.
- **Egress consumed** — aggregate bytes the surviving network had to
  upload to perform the healing.

#### Usage

```sh
# Default scenario: 100 nodes, 50K CIDs, 40% failure, 15% high-bandwidth VPS mix.
python3 orchestrator_resilience.py

# Larger network, larger archive.
python3 orchestrator_resilience.py --nodes 250 --cids 200000 --median-mb 4

# Sweep network sizes to find self-healing thresholds.
python3 orchestrator_resilience.py --mode sweep --cids 200000 --median-mb 4

# Worst-case provider-purge model (failure targets high-bandwidth-VPS-class first).
python3 orchestrator_resilience.py --mode sweep --profile-bias high-bandwidth-vps

# Multi-seed comparison.
python3 orchestrator_resilience.py --mode sweep --seed 2
```

#### Empirical thresholds

Run conditions: 200 000 CIDs, log-normal sizes (median 4 MB, mean
~12 MB, total ~2.4 TB raw / ~7 TB stored at R=3), 40 % failure,
15 % high-bandwidth-VPS-class nodes, capacity-weighted pin assignment.

| Objective                                   | Within 24 h | Within 1 h | Within 5 min |
|---|---|---|---|
| Tier 1 cleared, uniform failure             | ~10 nodes   | ~25 nodes  | ~60 nodes    |
| Tier 1 cleared, worst case (provider purge) | ~25 nodes   | ~60 nodes  | ~600 nodes   |
| Full R=3, uniform failure                   | ~25 nodes   | ~40 nodes  | ~400 nodes   |
| Full R=3, worst case (provider purge)       | ~40 nodes   | ~100 nodes | ~1 500 nodes |

The "uniform failure" rows use `seed=2` with uniform-random node loss
— this approximates a hardware datacenter issue or a regional outage,
where node loss is uncorrelated with profile. The "worst case" rows
use `--profile-bias high-bandwidth-vps`, modelling a single provider
abruptly terminating every donor's account, which removes the highest-
capacity nodes first and forces residential-class nodes to do recovery
work on links that are ~20× slower. **Plan against the worst-case
rows; the uniform-failure rows are a best-case lower bound.**

#### Architectural takeaways

1. **Tier-1 healing is far cheaper than full R=3 restoration.** It
   needs only one transfer per critical CID (1 → 2 pins), not two.
   The orchestrator's strict priority queue exploits this: the
   network exits the "one-failure-from-loss" zone in minutes at
   modest scale even when full R=3 restoration takes much longer.
   **The SLA worth committing to publicly is "Tier 1 cleared," not
   "full R=3 restored."**
2. **High-bandwidth VPS survival dominates the result at small N.** With a 15 %
   high-bandwidth VPS ratio and only 10–20 nodes total, whether 0, 1, or 2
   high-bandwidth VPSes survive a 40 % uniform-random failure swings recovery
   time by an order of magnitude. The simulation's `seed=1` and
   `seed=2` runs disagreed sharply at N≤25 for this reason.
3. **Capacity-weighted pin assignment is double-edged.** It
   concentrates load on high-bandwidth VPSes during steady state (efficient),
   but also concentrates risk there. A high-bandwidth-VPS-biased purge produces
   a much larger Tier 1 queue than uniform pinning. The orchestrator
   must tolerate both modes.
4. **Bandwidth-budget enforcement is not the binding constraint** at
   realistic network sizes. The aggregate daily budget of even 25
   nodes (≈ 8 TB/day under default profiles) dwarfs the healing
   payload (~2 TB). The binding constraint is *link speed*, which is
   why the sub-5-minute thresholds are sharply higher than the sub-
   24-hour thresholds — daily budget headroom is plenty, the network
   simply cannot push the bytes through small pipes fast enough.
5. **The minimum operational floor is around 25 nodes.** Below this,
   sub-day full-R=3 healing becomes uncertain because the surviving
   network's daily budget is comparable to the healing payload, and
   the high-bandwidth-VPS-survival lottery dominates.

These thresholds inform the orchestrator implementation in
`internal/orchestrator/` (Phase 2) and are referenced from the plan's
"Mass-casualty resilience" section.

### `sybil_concentration.py`

Models an adversary that controls a coordinated subset of donor nodes
and tries to absorb enough shards of the target corpus to cause data
loss when they simultaneously withdraw. The simulation answers: for
a given sybil ratio and replication factor, what fraction of CIDs
lose all their replicas?

#### Architectural rule encoded

Reputation-weighted placement does **not** defend against
honest-during-acquisition adversaries — sybils that respond truthfully
to possession audits during the acquisition phase maintain the
default reputation. The defense is the replication factor itself:
loss probability scales as `sybil_ratio ^ R`.

#### Usage

```sh
# Default scenario: 100 nodes, 10 % sybil, R=3, 50K CIDs.
python3 sybil_concentration.py

# How loss scales with sybil_ratio at a fixed R.
python3 sybil_concentration.py --mode sweep-ratio

# How loss scales with R at a fixed sybil_ratio.
python3 sybil_concentration.py --mode sweep-r --sybil-ratio 0.20
```

#### Architectural takeaways

1. **Loss is bounded by `sybil_ratio ^ R`.** An adversary controlling
   10 % of the network can cause ~0.1 % loss at R=3, ~0.01 % loss at
   R=4. Raising R for irreplaceable content is the right defense.
2. **Reputation isn't an early-warning signal.** Sybils playing the
   long game pass audits during acquisition. The audit subsystem
   catches *lying* donors, not *patient* ones.
3. **The Tier 1 fraction is roughly `3 * sybil_ratio² * (1 - sybil_ratio)`
   at R=3.** Even when total loss is small, the post-withdrawal Tier 1
   queue is meaningfully larger; operators should plan for the
   recovery payload to exceed the lost-CID count by orders of magnitude.

### `long_tail_churn.py`

Models slow-but-steady donor attrition without a single failure event
that would trigger mass-casualty detection. Tracks per-week network
size, aggregate daily budget, corpus growth, capacity runway, and
whether the `federation.shrinking` webhook should fire.

#### Architectural rule encoded

"Bandwidth budgets are inviolable" extends to the slow-attrition
case. The orchestrator does not paper over a structurally
insufficient federation by overdrawing donors. The webhook is the
correct response: notify the operator to recruit, not silently
overdraw.

#### Usage

```sh
# Default scenario: 100 initial nodes, 2 % weekly departure, 0.5 % weekly
# recruitment, 1 TB corpus, 0.5 % weekly corpus growth, R=3, 104 weeks.
python3 long_tail_churn.py

# Faster attrition.
python3 long_tail_churn.py --departure-rate 0.05

# Sweep attrition rates against recruitment rates.
python3 long_tail_churn.py --mode sweep
```

#### Architectural takeaways

1. **The `federation.shrinking` webhook provides weeks-to-months of
   lead time** before sustained Tier 1 healing becomes infeasible at
   typical attrition rates. Operators with active recruitment
   pipelines have time to respond.
2. **At small federation sizes (<100 active donors), recruitment must
   keep pace with departures** because the int-truncated recruitment
   floor effectively stops at very small N. Federations that fall
   below the recruitment-viable size enter a slow death spiral.
3. **Lower R magnifies the slow-attrition impact** disproportionately
   because the Tier 1 recovery payload scales nonlinearly with
   attrition rate.

### `key_rotation_load.py`

Estimates `novactl keys rotate-master` wall time and read-path
latency impact under realistic load. First-order model: the
bottleneck is Postgres' UPDATE-commit throughput, not AEAD CPU.

#### Architectural rule encoded

Rotation is online; readers see the old master-key version until
each row commits the rewrap, then transparently move to the new
version. There is no read-path downtime, only a measurable p99
latency increase during the rotation window.

#### Usage

```sh
# Default scenario: 1 M keys, 1 K reads/sec, pooled DB.
python3 key_rotation_load.py

# Larger corpus.
python3 key_rotation_load.py --keys 50_000_000

# Sweep corpus sizes.
python3 key_rotation_load.py --mode sweep-corpus

# Sweep concurrent read load.
python3 key_rotation_load.py --mode sweep-reads
```

#### Architectural takeaways

1. **For 1 M keys: ~30 seconds** with a 16-connection pool and modest
   read load. Within the noise of normal operations.
2. **For 100 M keys: tens of minutes.** Operators with very large
   deployments should run rotations during low-traffic windows.
3. **Read-path p99 increases ~2-3×** during rotation. Detectable in
   metrics but not user-visible.
4. **AEAD CPU is not the bottleneck.** Even at 100 M keys, total AEAD
   CPU is ~200 seconds across 16 cores; Postgres dominates.
