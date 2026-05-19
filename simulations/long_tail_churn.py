#!/usr/bin/env python3
"""
Long-Tail Churn Simulation

Models a federation that slowly loses donors over weeks-to-months
while uploads continue. This is the failure mode the mass-casualty
detector misses: no single failure event crosses the 20%-in-1-hour
threshold, but the federation gradually crosses the mathematical
boundary at which the surviving daily budget is insufficient to
maintain the target replication factor.

The architectural feature being stress-tested:
  - capacity_runway_floor_days in HEALING_PROTOCOL.md § "Slow-attrition
    detection". The federation.shrinking webhook should fire before the
    federation crosses into actual data loss.

The simulation tracks, week by week:
  - Network size (active donors)
  - Aggregate daily budget (TB/day)
  - Corpus size (TB, accounting for uploads since week 0)
  - Capacity runway = aggregate_daily_budget / (corpus * R)
  - Whether the federation.shrinking webhook should fire (runway < floor)
  - Whether actual data loss has begun (Tier 1 healing falling behind)

Architectural rule encoded:
  - "Bandwidth budgets are inviolable" extends to the slow-attrition
    case. The orchestrator does not paper over a structurally
    insufficient federation by overdrawing donors. The webhook is the
    correct response: notify the operator to recruit, not silently
    overdraw.

Usage:
  python3 long_tail_churn.py
  python3 long_tail_churn.py --weeks 52 --departure-rate 0.02
  python3 long_tail_churn.py --mode sweep
  python3 long_tail_churn.py --help
"""

from __future__ import annotations

import argparse
import math
import random
from dataclasses import dataclass
from typing import Dict, List, Optional


@dataclass(frozen=True)
class Profile:
    name: str
    bandwidth_budget_mb_per_day: float


HIGH_BANDWIDTH_VPS = Profile("high-bandwidth-vps", 2_097_152.0)  # 2 TB/day
RESIDENTIAL = Profile("residential", 51_200.0)                    # 50 GB/day


@dataclass
class FederationSnapshot:
    week: int
    active_nodes: int
    aggregate_daily_budget_mb: float
    corpus_mb: float
    replicated_demand_mb: float           # corpus * R
    runway_days: float
    shrinking_webhook_armed: bool
    tier1_healing_falling_behind: bool


def simulate(
    initial_nodes: int = 100,
    high_bandwidth_vps_ratio: float = 0.15,
    weekly_departure_rate: float = 0.02,        # 2 % donor loss per week
    weekly_recruitment_rate: float = 0.005,     # 0.5 % gain per week (net negative -> shrinking)
    initial_corpus_tb: float = 1.0,
    weekly_corpus_growth_rate: float = 0.005,   # 0.5 % growth per week
    replication_factor: int = 3,
    capacity_runway_floor_days: int = 7,
    simulation_weeks: int = 104,
    seed: int = 1,
) -> List[FederationSnapshot]:
    rng = random.Random(seed)

    nodes: List[Profile] = []
    for _ in range(initial_nodes):
        nodes.append(HIGH_BANDWIDTH_VPS if rng.random() < high_bandwidth_vps_ratio else RESIDENTIAL)

    corpus_mb = initial_corpus_tb * 1024 * 1024
    snapshots: List[FederationSnapshot] = []
    webhook_already_fired = False

    for week in range(simulation_weeks):
        # Departures (random selection)
        num_departing = int(len(nodes) * weekly_departure_rate)
        for _ in range(num_departing):
            if not nodes:
                break
            nodes.pop(rng.randrange(len(nodes)))

        # Recruitment (operator's success rate; net-negative if departure > recruitment)
        num_recruiting = int(len(nodes) * weekly_recruitment_rate)
        for _ in range(num_recruiting):
            nodes.append(HIGH_BANDWIDTH_VPS if rng.random() < high_bandwidth_vps_ratio else RESIDENTIAL)

        # Corpus growth
        corpus_mb *= (1 + weekly_corpus_growth_rate)

        # Compute runway
        aggregate_daily = sum(n.bandwidth_budget_mb_per_day for n in nodes)
        replicated_demand = corpus_mb * replication_factor
        runway_days = aggregate_daily / replicated_demand if replicated_demand > 0 else float("inf")

        # Webhook arm (per protocol: fire once per 24h; this simulation
        # is week-granular, so once-per-week is the analog).
        webhook_armed = runway_days < capacity_runway_floor_days and not webhook_already_fired
        if webhook_armed:
            webhook_already_fired = True
        # Re-arm if runway recovers above floor (operator recruited)
        if runway_days >= capacity_runway_floor_days * 1.5:
            webhook_already_fired = False

        # Tier 1 healing falling behind: per-week Tier 1 payload from
        # this rate of attrition exceeds this week's aggregate budget.
        # For attrition rate x with R=3, the expected fraction of CIDs
        # falling to one surviving holder is 3*x^2*(1-x).
        x = weekly_departure_rate
        weekly_tier1_payload = corpus_mb * replication_factor * x * x * (1 - x)
        tier1_falling_behind = weekly_tier1_payload > aggregate_daily * 7

        snapshots.append(FederationSnapshot(
            week=week,
            active_nodes=len(nodes),
            aggregate_daily_budget_mb=aggregate_daily,
            corpus_mb=corpus_mb,
            replicated_demand_mb=replicated_demand,
            runway_days=runway_days,
            shrinking_webhook_armed=webhook_armed,
            tier1_healing_falling_behind=tier1_falling_behind,
        ))

    return snapshots


def _print_timeline(snapshots: List[FederationSnapshot], width: int = 88) -> None:
    print(f"  {'week':>4}  {'nodes':>5}  {'budget GB/d':>11}  {'corpus GB':>9}  {'runway d':>9}  {'webhook':>8}  {'tier1':>6}")
    for s in snapshots:
        budget_gb = s.aggregate_daily_budget_mb / 1024
        corpus_gb = s.corpus_mb / 1024
        runway_str = f"{s.runway_days:.1f}" if s.runway_days < 1000 else "∞"
        webhook_str = "FIRE" if s.shrinking_webhook_armed else "-"
        tier1_str = "FALLING" if s.tier1_healing_falling_behind else "ok"
        print(f"  {s.week:>4}  {s.active_nodes:>5}  {budget_gb:>11.0f}  {corpus_gb:>9.0f}  {runway_str:>9}  {webhook_str:>8}  {tier1_str:>6}")


def find_warning_lead(snapshots: List[FederationSnapshot]) -> Optional[Dict[str, int]]:
    """How many weeks of warning the webhook provides before Tier 1 fails."""
    webhook_week = next((s.week for s in snapshots if s.shrinking_webhook_armed), None)
    failure_week = next((s.week for s in snapshots if s.tier1_healing_falling_behind), None)
    if webhook_week is None or failure_week is None:
        return None
    return dict(
        webhook_fired_week=webhook_week,
        tier1_failed_week=failure_week,
        warning_lead_weeks=failure_week - webhook_week,
    )


def run_default_scenario() -> None:
    snapshots = simulate()
    print("=== Long-tail churn (default scenario, 104 weeks) ===")
    _print_timeline(snapshots)
    lead = find_warning_lead(snapshots)
    if lead:
        print()
        print(f"  Webhook armed at week {lead['webhook_fired_week']}")
        print(f"  Tier-1 healing fell behind at week {lead['tier1_failed_week']}")
        print(f"  Warning lead: {lead['warning_lead_weeks']} weeks")
    else:
        print()
        print("  Federation survived 52 weeks without crossing the threshold.")


def sweep_attrition_rates() -> None:
    """How warning lead varies with attrition rate."""
    print("=== Attrition-rate sweep (initial 50 nodes, 1 TB corpus, R=3) ===")
    print(f"  {'departure/week':>14}  {'recruitment/week':>16}  {'webhook week':>12}  {'fail week':>9}  {'lead':>6}")
    for dep in [0.01, 0.02, 0.03, 0.05, 0.08]:
        for rec in [0.0, 0.005, 0.01]:
            snapshots = simulate(
                weekly_departure_rate=dep,
                weekly_recruitment_rate=rec,
                simulation_weeks=104,
            )
            lead = find_warning_lead(snapshots)
            if lead is None:
                print(f"  {dep:>14.1%}  {rec:>16.1%}  {'—':>12}  {'—':>9}  {'—':>6}")
            else:
                print(f"  {dep:>14.1%}  {rec:>16.1%}  {lead['webhook_fired_week']:>12}  {lead['tier1_failed_week']:>9}  {lead['warning_lead_weeks']:>6}")


def main() -> None:
    p = argparse.ArgumentParser(description="Long-tail churn simulation")
    p.add_argument("--nodes", type=int, default=100)
    p.add_argument("--weeks", type=int, default=104)
    p.add_argument("--departure-rate", type=float, default=0.02)
    p.add_argument("--recruitment-rate", type=float, default=0.005)
    p.add_argument("--corpus-tb", type=float, default=1.0)
    p.add_argument("--replication", type=int, default=3)
    p.add_argument("--runway-floor-days", type=int, default=7)
    p.add_argument("--mode", choices=["default", "sweep"], default="default")
    p.add_argument("--seed", type=int, default=1)
    args = p.parse_args()

    if args.mode == "default":
        snapshots = simulate(
            initial_nodes=args.nodes,
            weekly_departure_rate=args.departure_rate,
            weekly_recruitment_rate=args.recruitment_rate,
            initial_corpus_tb=args.corpus_tb,
            replication_factor=args.replication,
            capacity_runway_floor_days=args.runway_floor_days,
            simulation_weeks=args.weeks,
            seed=args.seed,
        )
        _print_timeline(snapshots)
        lead = find_warning_lead(snapshots)
        if lead:
            print()
            print(f"  Warning lead: {lead['warning_lead_weeks']} weeks "
                  f"(webhook armed at week {lead['webhook_fired_week']}, "
                  f"Tier-1 failed at week {lead['tier1_failed_week']})")
    elif args.mode == "sweep":
        sweep_attrition_rates()


if __name__ == "__main__":
    main()
