#!/usr/bin/env python3
"""
Sybil Concentration Simulation

Models an adversary that controls a coordinated subset of donor nodes
and tries to absorb enough shards of a target corpus to cause data
loss when they simultaneously withdraw.

The architectural defenses being stress-tested:
  - Reputation-weighted placement (HEALING_PROTOCOL.md): new
    assignments favor high-reputation nodes, so an adversary that
    fails possession audits gets fewer new shards.
  - Possession audits (POSSESSION_AUDIT.md): challenge-response
    spot-checks. Donors that fail audits decay their reputation.
  - Replication factor R (HEALING_PROTOCOL.md): a CID is lost only
    if all R holders simultaneously go dark.

The adversary's strategy modeled here is:
  1. Onboard `sybil_ratio` of the donor pool over the acquisition window.
  2. Behave honestly during the acquisition phase (pass possession audits).
  3. After acquiring shards across the network, simultaneously go dark.

The simulation answers: for a given sybil_ratio and R, what fraction of
CIDs lose all their replicas?

Architectural rule encoded:
  - Reputation-weighted placement does NOT defend against honest-during-
    acquisition adversaries. The audits pass because the bytes are
    present. The defense is the replication factor itself: an adversary
    must control more than (R-1)/R of the network to be statistically
    sure of capturing all R replicas of an average CID.

Usage:
  python3 sybil_concentration.py
  python3 sybil_concentration.py --sybil-ratio 0.20 --replication 3
  python3 sybil_concentration.py --mode sweep
  python3 sybil_concentration.py --help
"""

from __future__ import annotations

import argparse
import math
import random
from dataclasses import dataclass, field
from typing import Dict, List, Optional, Tuple


@dataclass
class Node:
    id: int
    is_sybil: bool
    alive: bool = True
    pins: set = field(default_factory=set)


def build_network(num_nodes: int, sybil_ratio: float, seed: int) -> List[Node]:
    rng = random.Random(seed * 7919)
    num_sybil = int(num_nodes * sybil_ratio)
    is_sybil = [True] * num_sybil + [False] * (num_nodes - num_sybil)
    rng.shuffle(is_sybil)
    return [Node(id=i, is_sybil=is_sybil[i]) for i in range(num_nodes)]


def assign_pins(
    nodes: List[Node],
    num_cids: int,
    replication_factor: int,
    placement_strategy: str,
    seed: int,
) -> Dict[int, List[int]]:
    """
    Assign each CID to R distinct nodes.

    Strategies:
      - 'uniform': uniform random over all nodes
      - 'reputation_weighted': new nodes start at reputation 1.0 (the
        default for the orchestrator's reputation-weighted placement);
        the adversary passes audits during acquisition so its reputation
        stays at 1.0. Reputation-weighted placement therefore behaves
        identically to uniform for honest-during-acquisition adversaries.
        This is the negative result the simulation demonstrates.
    """
    rng = random.Random(seed * 31337)
    pins: Dict[int, List[int]] = {}
    for cid in range(num_cids):
        # Both strategies collapse to uniform sampling here, because
        # acquisition-honest sybils maintain the default reputation.
        chosen = rng.sample(range(len(nodes)), replication_factor)
        for nidx in chosen:
            nodes[nidx].pins.add(cid)
        pins[cid] = chosen
    return pins


def execute_simultaneous_withdrawal(nodes: List[Node]) -> List[int]:
    """The adversary pulls all sybils offline at once. Returns dead IDs."""
    dead: List[int] = []
    for n in nodes:
        if n.is_sybil:
            n.alive = False
            dead.append(n.id)
    return dead


def measure_loss(
    nodes: List[Node],
    pins: Dict[int, List[int]],
    replication_factor: int,
) -> Dict[str, int]:
    cids_lost = 0
    tier1 = 0  # 1 surviving replica
    tier2 = 0  # 2 surviving replicas
    healthy = 0  # >= R surviving replicas

    for cid, holders in pins.items():
        survivors = sum(1 for nidx in holders if nodes[nidx].alive)
        if survivors == 0:
            cids_lost += 1
        elif survivors == 1:
            tier1 += 1
        elif survivors == 2:
            tier2 += 1
        else:
            healthy += 1

    return dict(
        cids_lost=cids_lost,
        tier1=tier1,
        tier2=tier2,
        healthy=healthy,
        total=len(pins),
    )


def run_scenario(
    num_nodes: int = 100,
    sybil_ratio: float = 0.10,
    num_cids: int = 50_000,
    replication_factor: int = 3,
    placement_strategy: str = "reputation_weighted",
    seed: int = 1,
    quiet: bool = False,
) -> Dict[str, float]:
    nodes = build_network(num_nodes, sybil_ratio, seed)
    pins = assign_pins(nodes, num_cids, replication_factor, placement_strategy, seed)
    dead = execute_simultaneous_withdrawal(nodes)
    result = measure_loss(nodes, pins, replication_factor)

    result.update(
        num_nodes=num_nodes,
        num_sybil=len(dead),
        sybil_ratio=sybil_ratio,
        replication_factor=replication_factor,
        loss_pct=result["cids_lost"] / result["total"] * 100,
        tier1_pct=result["tier1"] / result["total"] * 100,
        # Theoretical baseline for uniform random sampling:
        # P(all R replicas were sybil) = sybil_ratio^R.
        theoretical_loss_pct=sybil_ratio**replication_factor * 100,
    )

    if not quiet:
        _print_report(result)
    return result


def _print_report(m: Dict[str, float]) -> None:
    print(f"  Network          : {m['num_nodes']} nodes ({m['num_sybil']} sybil, {m['sybil_ratio']:.0%} of network)")
    print(f"  Replication      : R = {m['replication_factor']}")
    print(f"  Corpus           : {m['total']:,} CIDs")
    print(f"  --- Outcome after simultaneous sybil withdrawal ---")
    print(f"  CIDs lost forever: {m['cids_lost']:,} ({m['loss_pct']:.2f} %)")
    print(f"  Tier 1 (at 1 pin): {m['tier1']:,} ({m['tier1_pct']:.2f} %)")
    print(f"  Theoretical loss : {m['theoretical_loss_pct']:.2f} %  (sybil_ratio ^ R)")


def sweep_sybil_ratio(replication_factor: int = 3, num_cids: int = 50_000) -> None:
    """How loss scales with sybil_ratio at a fixed R."""
    print(f"=== Sybil concentration sweep (R={replication_factor}, {num_cids:,} CIDs) ===")
    print(f"  {'sybil_ratio':>12}  {'lost':>8}  {'tier1':>8}  {'theoretical':>12}")
    for ratio in [0.05, 0.10, 0.15, 0.20, 0.25, 0.30, 0.40, 0.50]:
        m = run_scenario(
            num_nodes=200,
            sybil_ratio=ratio,
            num_cids=num_cids,
            replication_factor=replication_factor,
            quiet=True,
        )
        print(f"  {ratio:>12.0%}  {m['loss_pct']:>7.2f}%  {m['tier1_pct']:>7.2f}%  {m['theoretical_loss_pct']:>11.2f}%")


def sweep_replication(sybil_ratio: float = 0.20, num_cids: int = 50_000) -> None:
    """How loss scales with R at a fixed sybil_ratio."""
    print(f"=== Replication factor sweep (sybil_ratio={sybil_ratio:.0%}, {num_cids:,} CIDs) ===")
    print(f"  {'R':>3}  {'lost':>8}  {'tier1':>8}  {'theoretical':>12}")
    for R in [2, 3, 4, 5, 7, 10]:
        m = run_scenario(
            num_nodes=200,
            sybil_ratio=sybil_ratio,
            num_cids=num_cids,
            replication_factor=R,
            quiet=True,
        )
        print(f"  {R:>3}  {m['loss_pct']:>7.2f}%  {m['tier1_pct']:>7.2f}%  {m['theoretical_loss_pct']:>11.2f}%")


def main() -> None:
    p = argparse.ArgumentParser(description="Sybil concentration simulation")
    p.add_argument("--nodes", type=int, default=100)
    p.add_argument("--cids", type=int, default=50_000)
    p.add_argument("--sybil-ratio", type=float, default=0.10)
    p.add_argument("--replication", type=int, default=3)
    p.add_argument("--mode", choices=["default", "sweep-ratio", "sweep-r"], default="default")
    p.add_argument("--seed", type=int, default=1)
    args = p.parse_args()

    if args.mode == "default":
        run_scenario(
            num_nodes=args.nodes,
            num_cids=args.cids,
            sybil_ratio=args.sybil_ratio,
            replication_factor=args.replication,
            seed=args.seed,
        )
    elif args.mode == "sweep-ratio":
        sweep_sybil_ratio(replication_factor=args.replication, num_cids=args.cids)
    elif args.mode == "sweep-r":
        sweep_replication(sybil_ratio=args.sybil_ratio, num_cids=args.cids)


if __name__ == "__main__":
    main()
