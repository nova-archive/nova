# Simulations

Analytical tools that stress-test architectural decisions before they
calcify in production code. Each script is self-contained and uses
only the Python standard library, so it can be checked into the repo
and re-run by anyone.

## Files

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
