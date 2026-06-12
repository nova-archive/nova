# `novasim` — calibrated-hybrid resilience simulator

A Go simulator that complements the standalone Python models in `../` by
driving Nova's **real** storage primitives for calibration, then running a
discrete-event resilience model at scale on the measured constants.

Companion to the design doc:
[`docs/superpowers/specs/phase2/2026-06-12-resilience-and-post-1.0-architecture-design.md`](../../docs/superpowers/specs/phase2/2026-06-12-resilience-and-post-1.0-architecture-design.md).

## Why a second simulator

The four `../*.py` models are self-contained analytical tools that hardcode
donor profiles and never touch Nova's code. `novasim` answers a different
question — *where does the single-operator architecture break down, on real
hardware?* — by splitting the work:

- **Calibration (`calib/`)** drives the production primitives `internal/envelope`
  (AEAD encrypt/decrypt, key wrap/unwrap) and `internal/ipfs`
  (`AddDeterministic`, real CID/chunking per `IPFS_IMPORT_RULES.md`) to measure
  per-operation costs on the host.
- **Model (`model/`)** is a discrete-event resilience model that consumes those
  constants and scales to thousands of nodes. It ports the validated
  `../orchestrator_resilience.py` core and adds failure **domains**,
  concentration metrics + alerting, a diversity-optimized placement alternative,
  a single-coordinator throughput/availability model, and peer-assisted repair.

## Build-tag isolation

The whole tree is gated behind the **`novasim`** build tag, so the default
`go build ./...` / `go vet ./...` / CI surface is unaffected and the
P2-M0-gated `internal/orchestrator` package is never touched. Without the tag,
`./simulations/go/...` matches no packages.

```sh
go build -tags novasim ./simulations/go/...
go test  -tags novasim ./simulations/go/model/...
go run   -tags novasim ./simulations/go/cmd/novasim <subcommand>
```

## Subcommands

| Command | Answers |
|---|---|
| `calibrate` | What do AEAD, key-unwrap and deterministic IPFS import actually cost on this host? Writes `calibration.json`. |
| `coordinator` | At what read egress / upload ingest does one coordinator saturate? (NIC vs CPU vs DB binding.) |
| `sweep` | How many nodes to clear Tier-1 / restore full R within {24h, 1h, 5min}? (cross-validates the Python thresholds) |
| `scenario` | One experiment end-to-end: placement → concentration + alerts → failure → healing. |
| `availability` | Multi-coordinator read availability vs the shared DB/key/ingress floor. |

### Examples

```sh
# Measure real per-op costs.
go run -tags novasim ./simulations/go/cmd/novasim calibrate --out simulations/go/calibration.json

# The single-coordinator ceiling on the measured calibration.
go run -tags novasim ./simulations/go/cmd/novasim coordinator --calib simulations/go/calibration.json --nic-gbps 1

# Capacity-weighted vs diversity-optimized placement under a provider purge.
go run -tags novasim ./simulations/go/cmd/novasim scenario --failure provider-purge --buckets 8 --placement bandwidth  --seed 3
go run -tags novasim ./simulations/go/cmd/novasim scenario --failure provider-purge --buckets 8 --placement diversity --dest anti-affinity --seed 3

# Peer custodians rescue otherwise-lost CIDs.
go run -tags novasim ./simulations/go/cmd/novasim scenario --failure provider-purge --buckets 8 --peers 2 --seed 3
```

## Failure models

`uniform` (uncorrelated), `vps-bias` (the original profile-biased provider
purge), and the new **domain-correlated** purges `provider-purge` / `asn-purge`
/ `region-purge` (`--buckets N` kills the N largest domains by capacity). The
unit of failure is the failure domain, not the node.

## Layout

```
simulations/go/
  calib/        real-primitive calibration (//go:build novasim)
  model/        discrete-event resilience model + unit tests (//go:build novasim)
  cmd/novasim/  CLI (//go:build novasim)
  calibration.json   host-measured constants (regenerate with `calibrate`)
```

## Caveats

- Calibration is host-specific; regenerate on the machine you care about.
  Reported model numbers fall back to clearly-labelled *estimates* if no
  `calibration.json` is supplied.
- The model is byte-unit-consistent, so cross-validation against the Python
  models (which mix a MiB budget with a decimal-MB/s link) is "within noise",
  not digit-identical.
- The coordinator model is first-order (NIC/CPU/DB limbs), not a queueing
  simulation with latency tails — sufficient to locate the ceiling, not to set
  an SLO.
```
