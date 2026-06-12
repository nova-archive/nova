# Phase 2 second pass — Resilience analysis & post-1.0 architecture (HA, peering)

Status: **design / analysis** (forward-looking; proposes **post-1.0** phases and
draft amendments to the Phase 0 v3 normative invariants in
`docs/specs/ARCHITECTURE_DECISIONS.md`. Nothing here is normative until Bug
accepts a proposed amendment; the current `T1.27`/`T1.28` non-goals still hold).
This is a **second pass** over the Phase 2 problem space — it does **not** change
the P2-M0…M10 plan in `2026-06-11-phase2-federation.md`, and it introduces **no
production code** (the simulator lives in an isolated, build-tag-gated tree).

Authors: Bug Plowman (operator), Claude (implementation partner).

Companion artifact: the **`novasim`** calibrated-hybrid simulator under
`simulations/go/` (build tag `novasim`). Every quantitative claim below is
reproducible with the commands in § 10.

---

## Executive summary — the thesis

Nova is not one network; it is a **multiplex** of overlapping graphs with very
different shapes. Analysing them separately resolves an apparent paradox in the
ChatGPT thread (which treated "resilience" as one property) and points at the
work that actually matters before 1.0:

> **The storage/durability plane is already robust; the fragility that matters
> is on the control/availability plane.** A donor fleet of ≥25 nodes survives
> mass casualties and heals within budget. But *every read decrypts at the one
> coordinator* (`T1.26`), so read egress, upload ingest, the Postgres authority,
> and the master key are hard single points of failure that no amount of donor
> durability can address. "Where does the single-operator architecture break
> down?" is therefore primarily an **availability/throughput** question, not a
> durability one.

Three empirical results from `novasim`, on this host's real measured crypto/IO
costs, anchor the argument:

1. **The single coordinator is the ceiling.** On a 1 Gbps uplink a coordinator
   serves **~9.8 TiB/day** of reads regardless of object size or donor count
   (NIC-bound); even 10 GbE caps at ~98 TiB/day, and a single decrypt core caps
   at ~870 MiB/s. Donor durability does not move these numbers.
2. **Capacity-weighted placement manufactures targeted-attack fragility.** Under
   the current bandwidth-weighted placement, purging the high-bandwidth-VPS
   cohort (≈24 % of nodes) destroys **~64 %** of the corpus outright. Switching
   to diversity-optimized placement (decoupled from bandwidth, with soft
   anti-affinity) cuts that to **~7.5 %** — same failure, ~8.5× less loss — and
   eliminates the concentration that would otherwise trip alerts.
3. **Adding coordinators helps, but the shared hubs are the floor.** Three
   active read-coordinators lift the coordinator tier to ~0.999999 availability,
   yet end-to-end read availability is capped at the shared Postgres/key/ingress
   floor (~0.9924, ≈66 h/yr) until those hubs are themselves replicated.

These motivate two deliberate **post-1.0** phases — **multi-coordinator,
single-authority HA** and **opaque inter-federation peering** — and one policy
change Bug asked for explicitly: treat dangerous network homogeneity as
something to **alert on, not prevent**.

---

## 1. The networks of Nova (multiplex decomposition)

Nova layers several graphs with distinct topologies. (This refines the ChatGPT
thread's decomposition and corrects a few of its claims against the repo.)

| Layer | Nodes / edges | Shape | Failure character |
|---|---|---|---|
| Public request path | client → nginx → coordinator → {Postgres, Kubo} | rooted tree | coordinator is an **articulation vertex** (vertex-connectivity 1) |
| Federation control plane | donors → coordinator (poll/heartbeat/ack) | **directed star** | single hub; loss = no assignments/audits/healing |
| Nebula overlay | coordinator + donors + lighthouse(s) | mesh-capable, **centralised membership** | lighthouse is a discovery/recovery dependency |
| Donor→donor repair | coordinator-authorised source→dest transfers | dynamic, capacity-driven **temporal core-periphery** | repair "core" = high-budget donors |
| Blob↔donor storage | CIDs × donors, edge = acked pin | **weighted constrained-random bipartite** | the durability graph; analysed below |
| Private Kubo swarm | coordinator-bootstrapped peers | private bootstrapped graph | not load-bearing (Bitswap repair disabled, `T1.11`) |
| Federation-of-federations | one component per operator | **disjoint union** | `T1.28`: no cross-operator edges today |

The decomposition's payoff: these layers fail and scale **independently**, and
they pull in opposite directions. The storage bipartite graph is *more* robust
when its degree/incidence distribution is *flatter* (less hub-like); the control
star is *more* robust when it is *less* star-like (more coordinators). Two of the
three headline findings are just these two statements made quantitative.

## 2. Is it scale-free? Robust-yet-fragile, and the plane that matters

Nova is **not** scale-free in the strict sense. The CID side of the storage
graph has bounded degree `R` (3, or 5 for archival) — there is no
preferential-attachment power law. The donor side is **bimodal** (two resource
classes), not power-law. ChatGPT reached the same conclusion; that part stands.

But the load-bearing result from scale-free network theory is not "is the degree
distribution a power law?" It is Albert–Barabási **robust-yet-fragile**: networks
with concentrated hubs tolerate *random* failure superbly and *targeted hub
removal* catastrophically. The relevant question for Nova is therefore:

> Does any Nova policy *manufacture* hub concentration on the durability plane,
> importing the targeted-attack fragility even though the graph isn't formally
> scale-free?

**Yes** — capacity-weighted placement. With a 40.96× budget ratio
(2 TiB/day VPS vs 50 GiB/day residential) and a 15 % VPS mix, the first replica
lands on a VPS node with probability `(15·40.96)/(15·40.96 + 85) ≈ 0.878`, and
`novasim` measures that **~64–66 % of CIDs place all R replicas entirely within
the VPS cohort** (§ 4.3). A provider/cohort purge is exactly the "targeted hub
removal" that such a load distribution is fragile to — which is why the existing
`orchestrator_resilience.py` already reports provider-purge recovery as 5–10×
worse than uniform.

So the durability plane is robust *by default but fragile by policy*, and the
fix is to stop manufacturing the hub (§ 4.3, § 7). Meanwhile the **control plane
is a literal star** with an articulation vertex — fragile by construction, not
by policy — which is what HA addresses (§ 6.1).

## 3. Simulation methodology — the calibrated hybrid

The four existing `simulations/*.py` are standalone analytical models that
hardcode donor profiles and never touch Nova's real code. To answer "where does
it break down" with grounded numbers, `novasim` is a **calibrated hybrid**:

- **Calibration (`simulations/go/calib`)** drives the *real* production
  primitives — `internal/envelope` (`Codec.Encrypt/Decrypt`, `WrapKey/UnwrapKey`)
  and `internal/ipfs` (`EmbeddedBackend.AddDeterministic`, real CID/chunking per
  `IPFS_IMPORT_RULES.md`) — to measure per-operation costs on the actual host.
- **Model (`simulations/go/model`)** is a discrete-event resilience model that
  consumes those constants and runs at thousands-of-nodes scale. It ports the
  validated `orchestrator_resilience.py` core (bimodal profiles, log-normal
  sizes, step-capacity `min(remaining budget, link×step)`, strict Tier-1
  priority, asymmetric source selection, **inviolable budgets**) and adds:
  failure **domains** (principal/provider/ASN/region/host) with soft
  anti-affinity, **concentration metrics** (Gini, entropy, top-k share),
  a **diversity-optimized** placement alternative, a **single-coordinator
  throughput/availability** model, and **peer-assisted repair**.

The whole tree is gated behind the `novasim` build tag, so the default
`go build ./...` / CI surface — and the P2-M0-gated `internal/orchestrator`,
which this never touches — are unaffected (`./simulations/go/...` matches **no
packages** without the tag). Model invariants (Tier-1-before-Tier-2, budgets
never exceeded in any day, anti-affinity, exact concentration values) are locked
by unit tests.

**Calibration on this host** (`bugbear`, Ryzen 7 4800H, 16 logical cores):

| Operation | Measured | Note |
|---|---|---|
| AEAD encrypt (XChaCha20-Poly1305) | **918 MiB/s** / core | golang.org/x/crypto on Zen2 (no AES-NI shortcut) |
| AEAD decrypt | **870 MiB/s** / core | the read-path crypto cost |
| Per-blob key unwrap | **0.44 µs** / op | negligible vs decrypt |
| Deterministic IPFS import | **355 MiB/s** | chunk + hash + blockstore write + pin |

Measuring mattered: AEAD came in well below a naïve ≥1.4 GB/s guess, and import
came in *above* a naïve 150 MB/s guess. Both feed the ceilings in § 4.2.

## 4. Empirical findings

### 4.1 The Go port faithfully reproduces the Python thresholds

`novasim sweep` vs the published `orchestrator_resilience.py` table (200k CIDs,
4 MiB median, R=3, 40 % failure):

| Objective / window | Python | novasim (Go) |
|---|---|---|
| Tier-1, uniform — 24h · 1h · 5min | 10 · 25 · 60 | 10 · 10 · 40 |
| Full-R, uniform — 24h · 1h · 5min | 25 · 40 · 400 | 10 · 40 · **400** |
| Tier-1, cohort purge — 24h · 1h · 5min | 25 · 60 · 600 | **25 · 60 · 600** |
| Full-R, cohort purge — 24h · 1h · 5min | 40 · 100 · 1500 | 60 · 100 · 1500 |

The cohort-purge Tier-1 row matches **exactly** (25/60/600) and Full-R at 1h/5min
match (100/1500); small differences at the cheap end are within the unit
convention (novasim is byte-consistent; the Python mixes a MiB budget with a
decimal-MB/s link). The **≥25-node operational floor** reproduces. This validates
reusing the model for the new scenarios.

### 4.2 The single coordinator is the availability ceiling (headline)

`novasim coordinator` on the measured calibration. Because every read decrypts
at the coordinator (`T1.26`), this ceiling is independent of donor count:

| Uplink | Read egress ceiling | Per day | Binding |
|---|---|---|---|
| 1 Gbps | 119 MiB/s | **9.82 TiB/day** | NIC (all object sizes) |
| 10 GbE | 1.16 GiB/s | **98.2 TiB/day** | NIC |
| ∞ network, 1 decrypt core | 870 MiB/s | 71.6 TiB/day | CPU (decrypt) |

For a media workload the **NIC binds at every object size** — a popular archive
that needs to serve more than ~10 TiB/day cannot, on one coordinator with a
1 Gbps link, no matter how durable the donor fleet is. Upload ingest is bound by
the same uplink (and, network aside, by the 355 MiB/s import path). This is the
concrete "where it breaks down": **read/serve availability, not durability**, and
it is exactly what a read-replica HA tier (§ 6.1) relieves.

### 4.3 Capacity → centrality: the provider-purge cliff, and the fix

Identical seed-3 networks, provider purge of the VPS cohort (kills ≈24 % of
nodes), 50k CIDs:

| Placement | Pin-incidence Gini (node) | Provider Gini | **Lost forever** | Concentration alerts |
|---|---|---|---|---|
| **Bandwidth-weighted** (status quo) | 0.725 | 0.773 | **31,847 / 50,000 (64 %)** | fires |
| **Diversity-optimized** + anti-affinity dest | 0.259 | 0.422 | **3,727 (7.5 %)** | none |

Same failure event; the only change is *how replicas were placed*. Decoupling
placement weight from bandwidth (`~sqrt(free) × trust`, with soft failure-domain
anti-affinity) is **the single highest-leverage durability change available** and
costs nothing structural. It is ChatGPT's "Priority 2", here validated rather
than asserted. Note bandwidth still governs repair *source* selection — fast
nodes still do the heavy lifting during recovery; they just stop *accreting* a
disproportionate share of the archive.

### 4.4 Concentration is a signal to alert on, not an invariant to enforce

The same runs show the **alert, not prevent** mechanism working: the
bandwidth-weighted (fragile) configuration trips `federation.concentrated`-style
alerts on the provider/ASN/region dimensions (largest-share > 0.30 and/or
collapsed entropy); the diversity-optimized configuration trips none — **without
either strategy ever refusing to place a replica.** Placement keeps soft
anti-affinity as a *preference*; the hard signal is an operator alert, mirroring
the existing `federation.degraded` / `federation.shrinking` webhooks. This is the
deliberate divergence from ChatGPT, which proposed hard enforceable ceilings
("no provider > 25–30 %"): Nova warns, the operator decides (§ 7).

### 4.5 Peer-assisted recovery converts "lost" into "recoverable"

Peering's value is not faster healing — it is **eliminating permanent loss**.
Same provider purge, bandwidth-weighted (so 31,847 CIDs hit zero *local* holders):

| Configuration | Lost forever | Tier-1 clear | Healing egress |
|---|---|---|---|
| no peers | 31,847 | 2 min | 48 GiB |
| **+ 2 opaque peer custodians** | **0** | 8 min | 189 GiB |

Opaque cross-federation custodians (peers hold ciphertext only — no keys, no
catalog) act as always-surviving repair *sources* that re-seed otherwise-lost
CIDs. They cost more recovery bandwidth and time, and they do **not** count
toward local durability — but they turn a 64 % data-loss event into a full
recovery. This is the Phase-7 case (§ 6.2).

### 4.6 Multi-coordinator availability and the shared-hub floor

`novasim availability` (per-coordinator 0.99, DB 0.995, key 0.9999, ingress
0.9995, 20 % correlated coordinator failures):

| Coordinators | Coordinator tier | End-to-end read | Downtime/yr |
|---|---|---|---|
| 1 | 0.990000 | 0.984459 | 136 h |
| 2 | 0.997920 | 0.992335 | 67 h |
| 3 | 0.997999 | 0.992413 | 66.5 h |
| 5 | 0.998000 | 0.992414 | 66.5 h |

Adding coordinators saturates by k≈2–3: the **shared Postgres/key/ingress floor**
dominates. The lesson for Phase 6: multi-coordinator HA is necessary but
insufficient on its own — it must be paired with a replicated, fenced Postgres
authority and redundant ingress, or it just relocates the single point of
failure to the database.

## 5. Challenging ChatGPT — repo-grounded corrections

The ChatGPT thread is broadly sound; the corrections below are where it should be
sharpened, confirmed, or overridden against the actual repository.

- **CONFIRMED — origin copy sits outside `R`.** The Phase-2 design keeps the
  coordinator's local Kubo copy as read origin / repair fallback, *not counted*
  in donor `R`. So `novasim`'s `CIDsLostForever` is "all *donor* redundancy
  lost", not "permanent loss" unless origin + Postgres + key are also lost.
  ChatGPT's caveat is correct and is honoured (the model reports zero-holder
  counts as a donor-plane metric).
- **CONFIRMED — the cold-standby recipe exists.** ChatGPT repeatedly cited "the
  repository's cold-standby recipe"; this is real (`docs/recipes/COLD_STANDBY.md`
  — Postgres streaming replica, shared master-key versions, manual fenced
  failover, split-brain warning, `change_seq` resume). Phase 6 should be framed
  as *automating and hardening* this existing pattern, not inventing it.
- **SHARPENED — "~66 % all-three-on-VPS" is measured, not estimated.** ChatGPT
  computed ~66 % by hand. `novasim` measures **64–66 %** of CIDs placing all R
  replicas in the VPS cohort under bandwidth-weighted placement (§ 4.3). The
  estimate was right; we now have it from the real weighted sampler.
- **CONFIRMED against source — the job-queue fencing gap is real.** ChatGPT
  claimed `jobs.Queue.Complete` guards only on lease state, enabling a stale
  worker to complete a reclaimed-and-re-leased job. Verified in
  `internal/jobs/queue.go:130-147`: `UPDATE … WHERE id=$1 AND state='leased'`
  with **no lease-owner/generation token** (`Fail` likewise). Today this is
  contained — the queue is explicitly documented as *one per coordinator
  process* and handlers must be idempotent — but it is a genuine gap that
  multi-coordinator HA must close with a fencing token (§ 6.1).
- **OVERRIDDEN — concentration limits should be alerts, not hard ceilings.**
  ChatGPT proposed enforceable caps ("no provider > 25–30 %", "no ASN > 30 %").
  Per Bug's steer, Nova **alerts** on dangerous homogeneity and keeps placement
  anti-affinity *soft*; it never refuses to place a replica purely for
  homogeneity. § 4.4 shows the alert path is sufficient to distinguish fragile
  from healthy configurations. Rationale: a hard ceiling can *block healing* into
  the only available capacity during a casualty — precisely when you least want a
  placement veto.
- **MINOR — erasure coding is not actually "deferred" in-repo.** ChatGPT
  described EC as already-deferred; the repo's deferral lists (ROADMAP Phase 6+,
  THREAT_MODEL out-of-scope) don't mention it. It belongs in Phase 8+ research if
  pursued, and replication remains operationally safer for small mixed-media
  objects (EC's repair fan-out interacts badly with the failure-domain diversity
  we're trying to *increase*).
- **AFFIRMED — multi-master with independent writable databases is a reject.**
  ChatGPT's strongest conclusion holds: the only safe HA shape is **one logical
  authoritative history** with multiple serving replicas and exactly one fenced
  control leader. Postgres logical-replication conflict semantics are unacceptable
  for crypto-shred / legal-hold / assignment ordering. This frames the `T1.27`
  amendment (§ 7), not its abandonment.

## 6. Proposed post-1.0 phases

These promote two items out of the `Phase 6+` research grab-bag into deliberate,
scoped phases. Both are **post-1.0** (1.0 ships at the end of Phase 5); the
single-coordinator architecture remains correct and sufficient for early
adopters and the entire 1.0 line.

### 6.1 Phase 6 — Multi-coordinator, single-authority HA

**Goal:** remove the coordinator as an availability articulation point (§ 4.2,
§ 4.6) without ever allowing two authorities to diverge.

**Shape:** several active coordinators behind redundant ingress, all reading one
**strongly-consistent Postgres authority** (primary + fenced streaming
standbys); **exactly one fenced control-plane leader** runs the
orchestrator / liveness transitions / audits / lifecycle sweeps / master-key
rotation / cert revocation. Reads and donor-API traffic are active-active; global
state mutations carry a **monotonic control-term fencing token**.

**Required groundwork (the tech-debt this second pass is meant to surface
early):**
- **Fencing tokens on the job queue and control plane.** Add
  `lease_id`/`lease_generation` to `jobs` and a `coordinator_leases(subsystem,
  holder, term, expires_at)` table; every control mutation and every issued
  repair token carries the current term. (Closes the § 5 / `queue.go:130` gap.)
- **Origin location tracking.** The local-Kubo origin becomes a replicated tier:
  an `origin_locations(cid, coordinator_id, state, failure_domain)` table so any
  coordinator can fetch ciphertext it didn't import, with a transactional outbox
  for the non-atomic Kubo-pin/Postgres-commit boundary.
- **Multi-endpoint donors.** Donor config carries a coordinator *set*; donors
  fail over between endpoints and preserve their `since_seq` cursor. (The Phase-2
  immutable `assignment_id`/`generation` + durable pin-change log are exactly the
  prerequisites.)
- **Replicated/shared upload staging**, cross-instance signed-URL revocation,
  and shared/ingress rate-limiting (today all per-process).
- **Redundant Nebula lighthouses and Kubo bootstrap peers.**

**Builds directly on** `docs/recipes/COLD_STANDBY.md` (automating its manual
failover with mechanical fencing). **Explicitly rejects** independent writable
masters and asynchronous conflict reconciliation.

### 6.2 Phase 7 — Opaque inter-federation replica peering

**Goal:** off-site durability and disaster recovery across operators (§ 4.5)
**without merging trust domains.**

**Shape:** a `peer/v1` protocol distinct from donor `fed/v1`. A peer stores
**opaque ciphertext only** for another federation's objects — never keys,
plaintext, catalog, moderation state, or assignment history. Invariants:
- **Every object has exactly one home federation** (immutable `home_federation_id`).
- **Peers count as at most one failure domain each** (not their claimed internal
  copy count), and only while their lease is current and last audit recent.
- **No transit / no re-export** without explicit home authorization (prevents
  superpeer formation and unbounded custody chains).
- **Signed, generation-ordered tombstones** propagate crypto-shred; a peer
  persists tombstones even for objects it no longer holds (defeats replay).
- Optional **encrypted DR packages** (Postgres base backup + WAL + manifests,
  encrypted under a recovery key the peer does not hold) turn peering from
  ciphertext durability into full federation-reconstruction capability.

**Peering replicates bytes, not authority** — the single most important framing,
and the reason it is safe where multi-master is not.

Remaining `Phase 6+` research (WASM pinning, FFI, additional product modules,
PDP/POR, hot/cold tiering, S3 read adapter) renumbers to **Phase 8+**.

## 7. Proposed amendments to the Tier-1 invariants

Drafts for Bug's review. **Not applied** to `ARCHITECTURE_DECISIONS.md` until
accepted; if accepted, they become FED/protocol-versioned amendments like the
Phase-2 `D*` corrections.

**`T1.27` (proposed reframe).** *Was:* "Single-coordinator topology; multi-master
HA is an explicit non-goal." *Proposed:*

> A Nova federation has exactly **one logical authoritative state history**.
> Multiple coordinator replicas MAY serve requests concurrently when connected to
> that one strongly-consistent authority. Global control-plane operations require
> a **fenced leader term**. Independent concurrent authoritative histories
> (multi-master with divergent writable databases) remain prohibited.

**`T1.28` (proposed reframe).** *Was:* "Each operator runs an independent
federation; cross-federation peering is not a Phase 1–5 goal." *Proposed:*

> Federations remain independently governed and independently keyed. Optional
> **inter-federation peering** MAY replicate **opaque ciphertext** and encrypted
> recovery packages, but never merges user catalogs, moderation authority, legal
> holds, master keys, or assignment histories. **Every object has exactly one home
> federation**; peers never receive plaintext or active DEKs and cannot re-export.

**New Tier-2 (operator-tunable) — concentration alerting (alert, not prevent).**
A diversity/concentration alerting set, parallel to the existing mass-casualty /
slow-attrition webhooks:
- Metrics: pin-incidence Gini (per node), and per-dimension (provider/ASN/region/
  principal) normalized entropy, largest-share, and top-k share.
- Webhooks: `federation.concentrated` (a domain's largest-share exceeds a warn
  threshold, default 0.30) and `federation.homogeneous` (a dimension's normalized
  entropy collapses below a warn threshold, default 0.50).
- Placement gains **soft** failure-domain anti-affinity (the Phase-2 `D8`
  direction) but **MUST NOT** refuse to place a replica solely for homogeneity.
- Recommended companion change (durability, not a Tier-1 amendment): make
  steady-state placement weight `~sqrt(free_capacity) × trust` with soft
  anti-affinity, and keep **bandwidth for repair-source selection only** (§ 4.3).

## 8. Compute feasibility and VPS sizing

**Sufficient on Bug's laptop** (Ryzen 7 4800H, 16 threads, 16 GB RAM, ~420 GB
free): calibration (small real-byte runs through envelope + embedded Kubo) and
the headline discrete-event sweeps (≤4000 nodes, 50k–200k CIDs) all run in
seconds-to-~1 min each. The full cross-validation sweep (28 scenarios at 200k
CIDs) completes in ~50 s on one core; the model is memory-light (a few hundred MB
at 200k CIDs).

**A VPS is warranted only for** three specific extensions, none required for the
findings above:
1. **Million-CID corpora** for tail behaviour, and **high-seed Monte-Carlo**
   (hundreds of seeds × the full parameter grid) for tight confidence intervals —
   embarrassingly parallel, CPU-bound.
2. **Full real-bytes validation at scale** (actually importing/encrypting a
   multi-TB corpus through embedded Kubo) — disk- and wall-time-bound; ~420 GB
   free is the limiter, and at 355 MiB/s import a 2 TB corpus is ~1.6 h of pure
   import.
3. Long unattended sweep matrices.

**Suggested spec** (justified, not asserted): a **compute-optimized 16–32 vCPU,
32–64 GB RAM, 500 GB–1 TB NVMe** instance. The 16–32 vCPU serves the parallel
Monte-Carlo (near-linear speedup); 32–64 GB covers million-CID in-memory state
across parallel workers; 500 GB–1 TB NVMe is sized for option (2)'s real-byte
corpora at the measured import throughput. None of this changes the conclusions —
it tightens error bars and pushes the corpus tail.

## 9. What this does not settle

- **Exact placement-weight formula.** § 4.3 shows the *direction* (decouple from
  bandwidth) is decisive; the precise `sqrt(free) × trust × diversity` form needs
  tuning against real donor populations and a steady-state-churn model, not just
  a single failure event.
- **Postgres HA mechanics.** The availability model (§ 4.6) argues the DB must be
  replicated and fenced; it does not pick Patroni vs. cloud-managed vs. manual,
  or the synchronous-commit policy per operation class. That is Phase-6 design.
- **Peering economics, jurisdiction, and abuse.** § 6.2 is a technical sketch;
  retention/jurisdiction conflict rules and anti-abuse accounting are policy work.
- **Coordinator model fidelity.** § 4.2 is a first-order throughput/availability
  model (NIC/CPU/DB limbs), not a queueing simulation with latency tails; good
  enough to locate the ceiling, not to set an SLO.

## 10. Reproducing the results

```sh
# Calibrate against the real envelope + IPFS primitives (writes calibration.json):
go run -tags novasim ./simulations/go/cmd/novasim calibrate --out simulations/go/calibration.json

# Single-coordinator ceilings (§ 4.2):
go run -tags novasim ./simulations/go/cmd/novasim coordinator --calib simulations/go/calibration.json --cores 8 --nic-gbps 1

# Cross-validation vs the Python thresholds (§ 4.1):
go run -tags novasim ./simulations/go/cmd/novasim sweep --cids 200000 --median-mb 4 --failure uniform --seed 2
go run -tags novasim ./simulations/go/cmd/novasim sweep --cids 200000 --median-mb 4 --failure vps-bias --seed 1

# Capacity→centrality and the diversity fix (§ 4.3, § 4.4):
go run -tags novasim ./simulations/go/cmd/novasim scenario --failure provider-purge --buckets 8 --placement bandwidth  --seed 3
go run -tags novasim ./simulations/go/cmd/novasim scenario --failure provider-purge --buckets 8 --placement diversity --dest anti-affinity --seed 3

# Peer-assisted recovery (§ 4.5) and HA availability (§ 4.6):
go run -tags novasim ./simulations/go/cmd/novasim scenario --failure provider-purge --buckets 8 --placement bandwidth --peers 2 --seed 3
go run -tags novasim ./simulations/go/cmd/novasim availability

# Model invariants (Tier-1 priority, inviolable budgets, anti-affinity, exact metrics):
go test -tags novasim ./simulations/go/model/...
```
