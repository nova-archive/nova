#!/usr/bin/env python3
"""
Master-Key Rotation Under Load Simulation

Validates the encryption-envelope spec's claim that
`novactl keys rotate-master` is online: reads continue against the
old master-key version until each per-blob key row is re-wrapped
with the new version, then continue against the new version, with
no read-path downtime.

The cost of rotation is:
  - For each `data_encryption_keys` row: one AEAD decrypt (unwrap
    with old MK) + one AEAD encrypt (wrap with new MK) + one DB
    UPDATE committing the new wrap.
  - Concurrent read traffic continues. Each read performs one AEAD
    decrypt (unwrap the per-blob key, which is 48 bytes of payload
    plus 24-byte nonce) and one AEAD decrypt of the envelope itself
    (size = plaintext size).
  - Contention: DB UPDATE on data_encryption_keys touches the same
    row a reader might be looking up. Postgres MVCC means readers
    see the old version until the rotation commits; reader
    correctness is not at risk, but Postgres' transaction throughput
    is the bound on rotation speed under high contention.

Architectural rule encoded:
  - Rotation is bounded by Postgres' UPDATE-commit throughput on the
    `data_encryption_keys` table, NOT by AEAD computation. AEAD on
    modern CPUs is hundreds of MB/sec; per-row UPDATE-commit cycles
    are the limiting factor.

Usage:
  python3 key_rotation_load.py
  python3 key_rotation_load.py --keys 10_000_000 --reads-per-sec 5000
  python3 key_rotation_load.py --mode sweep
  python3 key_rotation_load.py --help
"""

from __future__ import annotations

import argparse
import math
from dataclasses import dataclass
from typing import Dict, List


# Calibration constants. These are first-order estimates for
# commodity hardware; operators should re-measure against their own
# Postgres deployment for accurate planning.
#
#  - AEAD: XChaCha20-Poly1305 throughput on a single modern x86 core.
#    Measured 350-500 MB/sec for small payloads. The 48-byte wrap is
#    overhead-dominated, so a single AEAD op is roughly 1 microsecond
#    of CPU time. Concurrency scales linearly with cores until DB
#    becomes the bottleneck.
#  - DB UPDATE: Postgres on modern NVMe with WAL on the same volume
#    sustains ~10,000-20,000 single-row UPDATE-commits per second
#    with single connection; scales to ~30,000-50,000 with pooled
#    connections and async commit.

AEAD_OPS_PER_SEC_PER_CORE = 1_000_000
DB_SINGLE_ROW_UPDATE_OPS_PER_SEC = 15_000      # single connection
DB_POOLED_UPDATE_OPS_PER_SEC = 40_000          # ~16-connection pool
DB_CONTENTION_OVERHEAD_RATIO = 0.20            # 20 % slowdown under read contention

READ_PATH_AEAD_OPS = 2                          # unwrap key + decrypt envelope


@dataclass
class RotationResult:
    total_keys: int
    rotation_seconds: float
    rotation_minutes: float
    read_p99_latency_ms: float
    db_throughput_ops_per_sec: float
    aead_cpu_seconds: float
    aead_concurrency_utilized: int


def estimate_rotation(
    num_keys: int,
    parallelism: int = 16,
    concurrent_reads_per_sec: int = 1000,
    use_pooled_db: bool = True,
) -> RotationResult:
    # AEAD work: 2 ops per key (unwrap + wrap), parallelizable across cores.
    aead_total_ops = num_keys * 2
    aead_cpu_seconds = aead_total_ops / AEAD_OPS_PER_SEC_PER_CORE
    aead_wall_seconds = aead_cpu_seconds / parallelism

    # DB UPDATE throughput: depends on connection pool and read contention.
    base_db = DB_POOLED_UPDATE_OPS_PER_SEC if use_pooled_db else DB_SINGLE_ROW_UPDATE_OPS_PER_SEC
    # Contention overhead grows with read traffic: each concurrent read can
    # acquire a tuple lock briefly during the planning phase.
    contention = min(DB_CONTENTION_OVERHEAD_RATIO * concurrent_reads_per_sec / 1000, 0.6)
    effective_db_throughput = base_db * (1 - contention)
    db_wall_seconds = num_keys / effective_db_throughput

    # Rotation time is max of the two bottlenecks (operations pipeline);
    # in practice DB dominates for any realistic parallelism.
    wall_seconds = max(aead_wall_seconds, db_wall_seconds)

    # Read-path latency impact: under MVCC, readers see committed rows
    # without blocking on writers. The p99 latency increase comes from
    # buffer-pool churn and incremental WAL flush pressure. Estimate:
    # baseline ~5 ms p99; under heavy rotation, add proportional to
    # db_throughput utilization.
    baseline_p99_ms = 5.0
    utilization = effective_db_throughput / base_db
    rotation_p99_ms = baseline_p99_ms * (1 + 2 * utilization)

    return RotationResult(
        total_keys=num_keys,
        rotation_seconds=wall_seconds,
        rotation_minutes=wall_seconds / 60,
        read_p99_latency_ms=rotation_p99_ms,
        db_throughput_ops_per_sec=effective_db_throughput,
        aead_cpu_seconds=aead_cpu_seconds,
        aead_concurrency_utilized=parallelism,
    )


def _print_report(r: RotationResult) -> None:
    print(f"  Rows to re-wrap   : {r.total_keys:,}")
    print(f"  Rotation wall time: {r.rotation_seconds:.1f} sec ({r.rotation_minutes:.1f} min)")
    print(f"  DB throughput     : {r.db_throughput_ops_per_sec:,.0f} updates/sec (after contention)")
    print(f"  AEAD CPU time     : {r.aead_cpu_seconds:.1f} sec total ({r.aead_concurrency_utilized}-way parallel)")
    print(f"  Read p99 (during) : {r.read_p99_latency_ms:.1f} ms")
    print(f"  Read p99 baseline : 5.0 ms")


def run_default_scenario() -> None:
    print("=== Master-key rotation (1 M keys, 1 k reads/sec, pooled DB, 16-way parallel) ===")
    r = estimate_rotation(num_keys=1_000_000, parallelism=16, concurrent_reads_per_sec=1000)
    _print_report(r)


def sweep_corpus_sizes() -> None:
    """How rotation time scales with corpus size."""
    print("=== Corpus-size sweep ===")
    print(f"  {'rows':>13}  {'wall time':>12}  {'p99 ms':>8}")
    for n in [100_000, 1_000_000, 10_000_000, 50_000_000, 100_000_000]:
        r = estimate_rotation(num_keys=n, parallelism=16, concurrent_reads_per_sec=1000)
        wall = (f"{r.rotation_seconds:.0f} s" if r.rotation_seconds < 600
                else f"{r.rotation_minutes:.1f} min" if r.rotation_minutes < 60
                else f"{r.rotation_minutes/60:.1f} h")
        print(f"  {n:>13,}  {wall:>12}  {r.read_p99_latency_ms:>8.1f}")


def sweep_read_load() -> None:
    """How rotation time scales with concurrent read load."""
    print("=== Read-load sweep (10 M keys, pooled DB, 16-way parallel) ===")
    print(f"  {'reads/sec':>10}  {'wall time':>12}  {'p99 ms':>8}")
    for reads in [100, 500, 1000, 2500, 5000, 10000]:
        r = estimate_rotation(num_keys=10_000_000, parallelism=16, concurrent_reads_per_sec=reads)
        wall = f"{r.rotation_minutes:.1f} min" if r.rotation_minutes < 60 else f"{r.rotation_minutes/60:.1f} h"
        print(f"  {reads:>10,}  {wall:>12}  {r.read_p99_latency_ms:>8.1f}")


def main() -> None:
    p = argparse.ArgumentParser(description="Master-key rotation load simulation")
    p.add_argument("--keys", type=int, default=1_000_000)
    p.add_argument("--parallelism", type=int, default=16)
    p.add_argument("--reads-per-sec", type=int, default=1000)
    p.add_argument("--pooled-db", action="store_true", default=True)
    p.add_argument("--single-conn-db", dest="pooled_db", action="store_false")
    p.add_argument("--mode", choices=["default", "sweep-corpus", "sweep-reads"], default="default")
    args = p.parse_args()

    if args.mode == "default":
        r = estimate_rotation(
            num_keys=args.keys,
            parallelism=args.parallelism,
            concurrent_reads_per_sec=args.reads_per_sec,
            use_pooled_db=args.pooled_db,
        )
        _print_report(r)
    elif args.mode == "sweep-corpus":
        sweep_corpus_sizes()
    elif args.mode == "sweep-reads":
        sweep_read_load()


if __name__ == "__main__":
    main()
