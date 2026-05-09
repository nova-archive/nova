#!/usr/bin/env python3
"""
Orchestrator Resilience Simulation

Models a Nova federation under a mass-casualty event and simulates the
bandwidth-budget-respecting healing process. The goal is to determine
the network size required for the orchestrator to restore Tier-1
safety (no CIDs at a single surviving pin) within target time windows
without ever overriding a donor node's bandwidth budget.

Improvements over a basic uniform-distribution model:
  - Bimodal node profiles (high-capacity "high-bandwidth-VPS-class" vs
    "residential-class") with realistic bandwidth budgets and link caps
  - Log-normal file size distribution (most files small, long tail to
    50 MB), seeded for reproducibility
  - Time-stepped healing process that respects per-node bandwidth
    budgets and physical link speeds at every step
  - Strict priority queue: Tier 1 (CIDs at 1 surviving pin) is fully
    drained before Tier 2 (CIDs at 2 surviving pins) is touched
  - Asymmetric source selection: transfers are pulled from the holder
    with the most remaining step capacity, shielding small nodes
  - Capacity-weighted pin assignment: high-budget nodes carry more of
    the network at steady state, mirroring how real federations look
    after natural drift
  - Network-size sweep mode to find the smallest N where Tier-1
    healing completes within {24 h, 1 h, 5 min} target windows

Architectural rule encoded in the simulator: bandwidth budgets are
inviolable. A "doomsday override" is not a feature. If healing would
take a week given the surviving capacity, the simulator reports a
week — the answer to "we are too small" is to grow the federation,
not to coerce the donors.

Usage:
  python3 orchestrator_resilience.py                    # default scenario
  python3 orchestrator_resilience.py --mode sweep       # find thresholds
  python3 orchestrator_resilience.py --nodes 250 --cids 100000
  python3 orchestrator_resilience.py --help
"""

from __future__ import annotations

import argparse
import math
import random
import statistics
import sys
from collections import defaultdict
from dataclasses import dataclass, field
from typing import Dict, List, Optional, Tuple


# =====================================================================
# Node profiles
# =====================================================================

@dataclass(frozen=True)
class Profile:
    name: str
    bandwidth_budget_mb_per_day: float   # operator-set; orchestrator must respect
    upload_link_mbps: float              # physical link cap, in megabits/sec

    @property
    def upload_mb_per_sec(self) -> float:
        # 1 byte = 8 bits; convert Mbps -> MB/s
        return self.upload_link_mbps / 8.0


HIGH_BANDWIDTH_VPS = Profile(
    name="high-bandwidth-vps",
    bandwidth_budget_mb_per_day=2_097_152.0,   # 2 TiB / day
    upload_link_mbps=1_000.0,                  # 1 Gbps symmetric
)

RESIDENTIAL = Profile(
    name="residential",
    bandwidth_budget_mb_per_day=51_200.0,      # 50 GB / day (modest residential cap)
    upload_link_mbps=50.0,                     # 50 Mbps upstream
)


@dataclass
class Node:
    id: int
    profile: Profile
    alive: bool = True
    pins: set = field(default_factory=set)
    bytes_uploaded_today: float = 0.0

    def remaining_budget_mb(self) -> float:
        return max(0.0, self.profile.bandwidth_budget_mb_per_day - self.bytes_uploaded_today)


# =====================================================================
# File size distribution
# =====================================================================

def generate_file_sizes(
    num_cids: int,
    median_mb: float = 0.5,
    sigma: float = 1.5,
    min_mb: float = 0.01,
    max_mb: float = 50.0,
    seed: int = 1,
) -> List[float]:
    """
    Log-normal distribution clamped to a realistic image-archive range.
    Median ~500 KB, long tail to 50 MB. ~10 KB lower bound for
    thumbnails and tiny avatars.
    """
    rng = random.Random(seed)
    mu = math.log(median_mb)
    out: List[float] = []
    for _ in range(num_cids):
        s = rng.lognormvariate(mu, sigma)
        out.append(max(min_mb, min(max_mb, s)))
    return out


# =====================================================================
# Network construction
# =====================================================================

def build_network(
    num_nodes: int,
    high_bandwidth_vps_ratio: float = 0.15,
    seed: int = 1,
) -> List[Node]:
    rng = random.Random(seed * 7919)  # decorrelate from file-size seed
    nodes: List[Node] = []
    for i in range(num_nodes):
        profile = HIGH_BANDWIDTH_VPS if rng.random() < high_bandwidth_vps_ratio else RESIDENTIAL
        nodes.append(Node(id=i, profile=profile))
    return nodes


def assign_pins(
    nodes: List[Node],
    num_cids: int,
    replication_factor: int,
    weighted: bool = True,
    seed: int = 1,
) -> Dict[int, List[int]]:
    """
    Assigns each CID to R distinct nodes. With `weighted=True`, the
    selection is biased by bandwidth budget so that high-capacity
    nodes carry proportionally more of the network — which is what
    natural drift produces in a long-running federation.
    """
    rng = random.Random(seed * 31337)
    pins: Dict[int, List[int]] = {}
    base_weights = [n.profile.bandwidth_budget_mb_per_day for n in nodes] if weighted else [1.0] * len(nodes)

    for cid in range(num_cids):
        chosen: List[int] = []
        candidate_idx = list(range(len(nodes)))
        local_w = list(base_weights)
        for _ in range(replication_factor):
            if not candidate_idx:
                break
            total = sum(local_w)
            if total <= 0:
                pos = rng.randrange(len(candidate_idx))
            else:
                pick = rng.uniform(0.0, total)
                cum = 0.0
                pos = 0
                for j, w in enumerate(local_w):
                    cum += w
                    if cum >= pick:
                        pos = j
                        break
            node_idx = candidate_idx[pos]
            chosen.append(node_idx)
            nodes[node_idx].pins.add(cid)
            candidate_idx.pop(pos)
            local_w.pop(pos)
        pins[cid] = chosen
    return pins


# =====================================================================
# Failure event
# =====================================================================

def trigger_failure(
    nodes: List[Node],
    failure_rate: float = 0.40,
    profile_bias: Optional[str] = None,   # "high-bandwidth-vps" or "residential" or None
    seed: int = 1,
) -> List[int]:
    """
    Vaporizes a fraction of nodes. With `profile_bias` set, the
    failure preferentially targets that profile (modelling a single
    hosting provider purging high-bandwidth VPSes or an ISP throttling residential
    connections out of usefulness).
    """
    rng = random.Random(seed * 104729)
    num_to_kill = int(len(nodes) * failure_rate)
    candidates = list(range(len(nodes)))
    rng.shuffle(candidates)
    if profile_bias is not None:
        candidates.sort(key=lambda i: 0 if nodes[i].profile.name == profile_bias else 1)
    dead = candidates[:num_to_kill]
    for idx in dead:
        nodes[idx].alive = False
    return dead


# =====================================================================
# Healing simulator
# =====================================================================

def heal(
    nodes: List[Node],
    pins: Dict[int, List[int]],
    file_sizes_mb: List[float],
    target_replication: int = 3,
    time_step_seconds: int = 60,
    max_time_seconds: int = 14 * 86_400,
) -> dict:
    """
    Discrete-time simulation of the orchestrator healing a network.

    Per step:
      1. If a day boundary has passed, reset per-node bandwidth budgets.
      2. Compute each surviving node's per-step capacity =
         min(remaining_budget, link_speed * time_step).
      3. Drain Tier 1 (CIDs at 1 alive pin), in increasing file-size
         order, pulling each from its highest-step-capacity holder to
         a random surviving non-holder. Bandwidth budgets are never
         overridden.
      4. Only after Tier 1 is empty do we touch Tier 2 (2 alive pins).
      5. If no transfers were possible this step (everyone exhausted
         their daily budget), fast-forward to the next budget reset.

    Performance notes:
      - holders_of[cid] tracks alive holders. Source selection iterates
        only that small set (size <= R), not all surviving nodes.
      - Destination is picked by uniform random sampling with retry on
        the rare collision against an existing holder. Amortized O(1).
    """
    rng = random.Random(20269)  # deterministic destination selection

    # Alive-holder map and pin counts.
    holders_of: Dict[int, set] = {}
    pin_count: Dict[int, int] = {}
    for cid, all_holders in pins.items():
        alive = {nidx for nidx in all_holders if nodes[nidx].alive}
        holders_of[cid] = alive
        pin_count[cid] = len(alive)

    cids_lost = sum(1 for p in pin_count.values() if p == 0)
    initial_tier1 = sum(1 for p in pin_count.values() if p == 1)
    initial_tier2 = sum(1 for p in pin_count.values() if p == 2)

    surviving = [n for n in nodes if n.alive]
    surviving_lookup = {n.id: n for n in surviving}

    if not surviving:
        return _result(0, None, None, initial_tier1, initial_tier2,
                       initial_tier1, initial_tier2, cids_lost, total_egress_mb=0.0)

    # Initial queues, sorted by file size (small first).
    tier1_queue = sorted([c for c, p in pin_count.items() if p == 1],
                         key=lambda c: file_sizes_mb[c])
    tier2_queue = sorted([c for c, p in pin_count.items() if 1 < p < target_replication],
                         key=lambda c: file_sizes_mb[c])

    if not tier1_queue and not tier2_queue:
        return _result(0, 0, 0, initial_tier1, initial_tier2, 0, 0,
                       cids_lost, total_egress_mb=0.0)

    tier1_clear_at: Optional[int] = 0 if not tier1_queue else None
    full_clear_at: Optional[int] = None
    elapsed = 0
    next_reset = 86_400
    total_egress = 0.0

    while elapsed < max_time_seconds:
        if elapsed >= next_reset:
            for n in surviving:
                n.bytes_uploaded_today = 0.0
            next_reset += 86_400

        step_cap: Dict[int, float] = {}
        for n in surviving:
            link_cap = n.profile.upload_mb_per_sec * time_step_seconds
            step_cap[n.id] = min(n.remaining_budget_mb(), link_cap)

        if max(step_cap.values()) <= 0:
            elapsed = next_reset
            continue

        progress = 0

        def pick_destination(cid: int) -> Optional[Node]:
            current = holders_of[cid]
            for _ in range(8):
                cand = rng.choice(surviving)
                if cand.id not in current:
                    return cand
            # Rare fallback: linear scan.
            for cand in surviving:
                if cand.id not in current:
                    return cand
            return None

        def attempt(cid: int) -> bool:
            nonlocal progress, total_egress
            size = file_sizes_mb[cid]
            best_src_id = None
            best_src_cap = 0.0
            for nid in holders_of[cid]:
                cap = step_cap.get(nid, 0.0)
                if cap >= size and cap > best_src_cap:
                    best_src_id = nid
                    best_src_cap = cap
            if best_src_id is None:
                return False
            dest = pick_destination(cid)
            if dest is None:
                return False
            src = surviving_lookup[best_src_id]
            src.bytes_uploaded_today += size
            step_cap[best_src_id] -= size
            dest.pins.add(cid)
            holders_of[cid].add(dest.id)
            pins[cid].append(dest.id)
            pin_count[cid] += 1
            total_egress += size
            progress += 1
            return True

        # Drain Tier 1 strictly first; on success demote to Tier 2 backlog.
        new_tier1: List[int] = []
        for cid in tier1_queue:
            if pin_count[cid] >= 2:
                tier2_queue.append(cid)
                continue
            if attempt(cid):
                tier2_queue.append(cid)
            else:
                new_tier1.append(cid)
        tier1_queue = new_tier1

        if not tier1_queue and tier1_clear_at is None:
            tier1_clear_at = elapsed + time_step_seconds

        if not tier1_queue:
            new_tier2: List[int] = []
            for cid in tier2_queue:
                if pin_count[cid] >= target_replication:
                    continue
                if attempt(cid):
                    if pin_count[cid] < target_replication:
                        new_tier2.append(cid)
                else:
                    new_tier2.append(cid)
            tier2_queue = new_tier2

        if not tier1_queue and not tier2_queue:
            full_clear_at = elapsed + time_step_seconds
            break

        if progress == 0:
            elapsed = next_reset
            continue

        elapsed += time_step_seconds

    return _result(
        elapsed_seconds=elapsed,
        time_to_tier1_clear_s=tier1_clear_at,
        time_to_full_healed_s=full_clear_at,
        initial_tier1=initial_tier1,
        initial_tier2=initial_tier2,
        tier1_remaining=len(tier1_queue),
        tier2_remaining=len(tier2_queue),
        cids_lost_forever=cids_lost,
        total_egress_mb=total_egress,
    )


def _result(elapsed_seconds, time_to_tier1_clear_s, time_to_full_healed_s,
            initial_tier1, initial_tier2, tier1_remaining, tier2_remaining,
            cids_lost_forever, total_egress_mb):
    return dict(
        elapsed_seconds=elapsed_seconds,
        time_to_tier1_clear_s=time_to_tier1_clear_s,
        time_to_full_healed_s=time_to_full_healed_s,
        initial_tier1=initial_tier1,
        initial_tier2=initial_tier2,
        tier1_remaining=tier1_remaining,
        tier2_remaining=tier2_remaining,
        cids_lost_forever=cids_lost_forever,
        total_egress_mb=total_egress_mb,
    )


# =====================================================================
# Top-level scenario and sweep
# =====================================================================

def run_scenario(
    num_nodes: int,
    num_cids: int = 50_000,
    replication_factor: int = 3,
    median_file_mb: float = 0.5,
    failure_rate: float = 0.40,
    high_bandwidth_vps_ratio: float = 0.15,
    weighted_pins: bool = True,
    profile_bias: Optional[str] = None,
    time_step_seconds: int = 60,
    seed: int = 1,
    quiet: bool = False,
) -> dict:
    nodes = build_network(num_nodes, high_bandwidth_vps_ratio=high_bandwidth_vps_ratio, seed=seed)
    file_sizes = generate_file_sizes(num_cids, median_mb=median_file_mb, seed=seed)
    pins = assign_pins(nodes, num_cids, replication_factor, weighted=weighted_pins, seed=seed)
    dead = trigger_failure(nodes, failure_rate=failure_rate, profile_bias=profile_bias, seed=seed)

    metrics = heal(
        nodes, pins, file_sizes,
        target_replication=replication_factor,
        time_step_seconds=time_step_seconds,
    )

    metrics.update(
        num_nodes=num_nodes,
        num_alive=num_nodes - len(dead),
        num_dead=len(dead),
        high_bandwidth_vps_count_alive=sum(1 for n in nodes if n.profile.name == "high-bandwidth-vps" and n.alive),
        residential_count_alive=sum(1 for n in nodes if n.profile.name == "residential" and n.alive),
        total_data_mb=sum(file_sizes),
        median_file_mb=statistics.median(file_sizes),
        failure_rate=failure_rate,
        replication_factor=replication_factor,
    )

    if not quiet:
        _print_report(metrics)
    return metrics


def _format_time(seconds: Optional[int]) -> str:
    if seconds is None:
        return "—"
    if seconds < 600:
        return f"{seconds} s"
    if seconds < 86_400:
        return f"{seconds // 60} min"
    return f"{seconds / 86_400:.1f} days"


def _print_report(m: dict) -> None:
    print(f"  Network          : {m['num_nodes']} nodes "
          f"({m['high_bandwidth_vps_count_alive']} high-bandwidth VPS + {m['residential_count_alive']} residential survived)")
    print(f"  Failure          : {m['num_dead']} nodes vaporized ({m['failure_rate']:.0%})")
    print(f"  Total data       : {m['total_data_mb']/1024:.1f} GB across {m['initial_tier1']+m['initial_tier2']:,} surviving CIDs")
    print(f"  Median file size : {m['median_file_mb']:.2f} MB")
    print(f"  CIDs lost forever: {m['cids_lost_forever']:,}")
    print(f"  Tier 1 at start  : {m['initial_tier1']:,}  (CIDs at 1 alive pin — critical)")
    print(f"  Tier 2 at start  : {m['initial_tier2']:,}  (CIDs at 2 alive pins — degraded)")
    print(f"  --- Healing ---")
    print(f"  Tier 1 cleared   : {_format_time(m['time_to_tier1_clear_s'])}")
    print(f"  Full R={m['replication_factor']} restored: {_format_time(m['time_to_full_healed_s'])}")
    print(f"  Egress consumed  : {m['total_egress_mb']/1024:.1f} GB")
    if m['tier1_remaining']:
        print(f"  ! Tier 1 unfinished after {_format_time(m['elapsed_seconds'])}: {m['tier1_remaining']:,} CIDs")
    if m['tier2_remaining'] and not m['tier1_remaining']:
        print(f"  ! Tier 2 unfinished after {_format_time(m['elapsed_seconds'])}: {m['tier2_remaining']:,} CIDs")


def find_threshold(
    target_seconds: int,
    sizes: List[int],
    objective: str = "tier1",   # 'tier1' or 'full'
    **kwargs,
) -> Optional[int]:
    """Smallest N (from `sizes`) where the chosen objective completes within target."""
    key = "time_to_tier1_clear_s" if objective == "tier1" else "time_to_full_healed_s"
    for n in sizes:
        m = run_scenario(num_nodes=n, quiet=True, **kwargs)
        t = m[key]
        if t is not None and t <= target_seconds:
            return n
    return None


def sweep(
    objectives=(("Tier 1", "tier1"), ("Full R=3", "full")),
    sizes: Optional[List[int]] = None,
    **kwargs,
) -> None:
    if sizes is None:
        sizes = [10, 15, 25, 40, 60, 100, 150, 250, 400, 600, 1000, 1500, 2500, 4000]
    print("=== Network-size thresholds (no budget overrides) ===")
    print(f"  Sizes tested : {sizes}")
    print(f"  CIDs         : {kwargs.get('num_cids', 50_000):,}")
    print(f"  Failure rate : {kwargs.get('failure_rate', 0.40):.0%}")
    print(f"  High-bandwidth VPS mix  : {kwargs.get('high_bandwidth_vps_ratio', 0.15):.0%}")
    print(f"  Median file  : {kwargs.get('median_file_mb', 0.5):.2f} MB")
    print()
    targets = [
        ("24 hours", 86_400),
        ("1 hour",   3_600),
        ("5 minutes",  300),
    ]
    for obj_label, obj_key in objectives:
        print(f"  --- Objective: {obj_label} healed within ... ---")
        for label, seconds in targets:
            n = find_threshold(seconds, sizes=sizes, objective=obj_key, **kwargs)
            if n is None:
                print(f"    {label:<10} : not achieved at any tested size (try larger)")
            else:
                print(f"    {label:<10} : ~{n} nodes")
        print()


# =====================================================================
# CLI
# =====================================================================

def main(argv: Optional[List[str]] = None) -> int:
    p = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--mode", choices=["scenario", "sweep"], default="scenario")
    p.add_argument("--nodes", type=int, default=100)
    p.add_argument("--cids", type=int, default=50_000)
    p.add_argument("--replication", type=int, default=3)
    p.add_argument("--median-mb", type=float, default=0.5)
    p.add_argument("--failure-rate", type=float, default=0.40)
    p.add_argument("--high-bandwidth-vps-ratio", type=float, default=0.15)
    p.add_argument("--unweighted-pins", action="store_true",
                   help="distribute pins uniformly instead of capacity-weighted")
    p.add_argument("--profile-bias", choices=["high-bandwidth-vps", "residential"], default=None,
                   help="bias the failure event toward one profile (provider-purge model)")
    p.add_argument("--time-step", type=int, default=60)
    p.add_argument("--seed", type=int, default=1)
    args = p.parse_args(argv)

    common = dict(
        num_cids=args.cids,
        replication_factor=args.replication,
        median_file_mb=args.median_mb,
        failure_rate=args.failure_rate,
        high_bandwidth_vps_ratio=args.high_bandwidth_vps_ratio,
        weighted_pins=not args.unweighted_pins,
        profile_bias=args.profile_bias,
        time_step_seconds=args.time_step,
        seed=args.seed,
    )

    if args.mode == "scenario":
        print(f"=== Scenario : N={args.nodes}, CIDs={args.cids}, R={args.replication}, "
              f"failure={args.failure_rate:.0%}, high-bandwidth-vps-ratio={args.high_bandwidth_vps_ratio:.0%} ===")
        run_scenario(num_nodes=args.nodes, **common)
    else:
        sweep(**common)
    return 0


if __name__ == "__main__":
    sys.exit(main())
