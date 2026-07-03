# P2-M7 — Production hardening & donor release

Design for the seventh Phase-2 milestone. P2-M7 does **not** add a new federation
subsystem and **adds no new replication policy**. It takes the donor-federation
vertical slice that M1–M6 built — build separation, identity/registration,
assignment sync, opaque replication, storage/read redirect, liveness/healing, and
possession audits — and makes it a **volunteer-ready, signed, digest-pinned release
that a fresh host can verify, join, serve, survive failure modes, and decommission
cleanly.** The load-bearing work is *operational*: observability (Prometheus
`/metrics`), a corpus-scale performance proof, N−1 / mixed-version compatibility
tests, failure drills with runbooks, and the final volunteer documentation. The
**only new federation-lifecycle primitive** is `novactl node drain` (with its tiny
inverse, `undrain`) — it exists because the existing revoke path is a safe
*involuntary-loss* response but an unsafe *voluntary* decommission primitive.

Normative floor: the P2-M0-amended specs in `docs/specs/` (`FEDERATION_PROTOCOL.md`,
`HEALING_PROTOCOL.md`, `POSSESSION_AUDIT.md`, `DATA_MODEL.sql`,
`ARCHITECTURE_DECISIONS.md`), `docs/VERSIONING.md`, `docs/THREAT_MODEL.md`, and the
Phase-2 master design
(`docs/superpowers/specs/phase2/2026-06-11-phase2-federation-design.md` — milestone
table row P2-M7 "Production hardening & donor release", phase exit criteria #1/#4).
This milestone is where the deferrals accumulated across M4.1/M5/M6 — Prometheus
`/metrics`, the corpus-scale benchmark gate — come due, and where the volunteer
walkthrough that M1 stubbed (`docs/quickstart/donor.md`) is finally written.

## Context

By the end of M6 the federation stack is functionally complete: a donor joins over
mTLS-on-Nebula, syncs assignments, replicates opaque v1 ciphertext and verifies by
root-CID, serves donor-backed reads, is healed after loss under failure-domain
anti-affinity, and is continuously possession-audited with evidence flowing into
reputation and trust graduation. What is **missing is everything that makes that
system operable, observable, and provably shippable to a stranger**:

- **Supply chain is already done — at the CI level.** M1 stood up the donor image
  pipeline (`donor-build` + `donor-sbom-sign`: syft SPDX SBOM + cosign keyless/OIDC
  + GitHub provenance attestation, push-by-digest to
  `ghcr.io/nova-archive/nova-node`). M2.1 added the mirror `coordinator-sbom-sign`
  job plus `govulncheck` in `.github/workflows/ci.yml`, and separate CodeQL /
  OpenSSF Scorecard workflows in `.github/workflows/codeql.yml` and
  `.github/workflows/scorecard.yml`. So "signed images + SBOM + provenance for both
  images" **exists**. What M1 explicitly parked for M7 is the *human* layer:
  `docs/quickstart/donor.md` is still a stub whose own text says "the exact `cosign
  verify` invocation + identity/issuer policy is documented in the P2-M7 walkthrough."
- **The slog evidence sets have no scrape surface.** M4.1 (D-M4.1-15), M5 (D-M5-13),
  and M6 (D-M6-11) each emit a deliberately metric-shaped `slog` signal set
  (`audit.possession.{challenged,passed,failed,skipped}`, `audit.reputation.moved`,
  `audit.trust.{graduated,demoted}`, `audit.governor.exhausted`,
  `orchestrator.liveness.*`, reconcile-drain and donor-fetch events) and each design
  names the same follow-up: "Prometheus `/metrics` — **P2-M7** (this set is its
  blueprint)." No `/metrics` endpoint exists yet. Promotion is not free: a slog line
  is not a durable metric store, so each family must be classified by *source* —
  DB-derived at scrape time, or instrumented process-locally at the event site
  (D-M7-1a).
- **Scale is unproven.** M6 shipped only "fixture + EXPLAIN-oriented query tests and
  an optional non-blocking local bench script, not a release gate" (M6 design D-M6-1).
  The corpus-scale benchmark **gate** at ~9.8 M `blob_blocks` rows is deferred here.
- **`revoke` is abrupt.** The 5-state liveness machine
  (`internal/orchestrator/liveness.go`) re-replicates a departing node's blobs on
  `unreachable`/`evicted`/`revoked` via `enqueueNodeCIDs` → `EnqueueReconcileForNode`.
  But the recompute filter counts only `active`/`suspect` holders
  (`RecomputeReplicationCounts`, `internal/db/queries/replication.sql`), so a
  revoked node **instantly stops counting toward durability and is excluded as a
  repair/read source.** That is correct for hostile/involuntary loss and unsafe for a
  *voluntary* decommission: a drained donor holding the last healthy copy of a
  concentrated blob, torn down before healing completes — with its origin copy pruned
  under `bounded_cache`/`transient` — is a data-loss path.
- **Operator docs have drifted.** `README.md` still describes Phase 2 as "through
  M1/M2, M3 next," which is actively misleading for a volunteer-facing release. For a
  volunteer-hosted system, docs drift is an operational failure mode, not cosmetics.

M7 makes the release **operable and provable**: release proof, visibility, failure
drills, and one safe voluntary-exit mechanism — no new replication policy. Its
thesis, stated as its exit criterion: **a fresh volunteer host can verify a signed
image, join, serve, survive each failure drill, and decommission cleanly, and an
N−1 coordinator/donor interoperate.**

## Decisions (ratified with Bug, 2026-07-01)

### D-M7-1 — Prometheus `/metrics` is coordinator-only, on a dedicated operator-bound listener.

The observability surface promotes the M4.1/M5/M6 slog evidence sets to a
`prometheus/client_golang` registry served at `GET /metrics` on a **new listener**,
**separate** from the public API plane, the operator/admin API, and the fed/Nebula
plane. Plane separation is deliberate: metrics scraping is neither user traffic nor
federation control traffic, and must not be reachable from either.

**Listener contract (exact behavior):**

- Config key `metrics_listen_addr` (operator `operator.yaml`; env-overridable via
  the existing config precedence). **Default `127.0.0.1:2112`** — explicit loopback,
  not merely "loopback".
- **Explicitly empty (`metrics_listen_addr: ""`) disables the metrics listener**;
  an absent key means the default (enabled on loopback). Disabling is a deliberate
  operator act, never an accident of omission.
- The listener starts and stops with the coordinator process (same signal-driven
  lifecycle), but is its own `net/http` server on its own plane — never mounted on
  the public, admin, or federation mux.
- **A bind failure is startup-fatal when metrics are enabled** (matching the
  refuse-to-start posture of the rest of the config floor). A coordinator that
  silently runs without its observability surface is worse than one that fails
  loudly.

**The donor stays `prometheus`-free.** M7 exposes **no** donor-local metrics — the
minimal, dependency-boundary-enforced `nova-node` artifact (P2-M1) is not widened,
and `internal/federation/wire` (deliberately dependency-free, one of the only
packages both binaries import) gains no metrics dependency. Every donor-adjacent
fact an operator needs is already observable from the coordinator's vantage
(heartbeat/liveness, read-source success/failure and circuit-open, audit outcomes,
egress refusals, reputation/trust transitions, sourceability, below-floor debt,
drain debt). Donor-local metrics are a later opt-in, once the donor artifact is
stable. **The `scripts/check_node_deps.sh` boundary gate is extended** so any
`github.com/prometheus/*` package (client_golang, promhttp, common, procfs) in the
`cmd/node` dependency graph fails CI — the guard is the enforcement, not a
convention.

**Bounded labels only — a metric is a minimal sufficient statistic, not a state
dump.** Labels are drawn from a small bounded set: `node_id` (tens–hundreds),
`tier`, `class`, `status`, `state`, `trust_state`, `result`, `reason`, `direction`,
`from`, `to`. **Never** per-CID / per-blob / per-filename / per-path labels — those
explode cardinality and leak object identity. The operator needs "is this node safe
to turn off?", not a time series per blob. Label discipline is enforced by an
automated scrape test (D-M7-8 #2).

#### D-M7-1a — Every metric family is classified by type, source, and reset semantics.

A slog line is not a durable metric store. Each family is one of three kinds, and
the distinction is part of the contract (exact names/help text pinned by the
implementation plan):

| Metric family | Type | Source | Reset semantics | Labels |
|---|---|---|---|---|
| `nova_replication_cids` | gauge | DB at scrape (`blob_replication_state`) | none (projection) | `tier`, `class` |
| `nova_reconcile_queue_depth` / `nova_reconcile_queue_oldest_seconds` | gauge | DB at scrape (`blob_replication_reconcile_queue`) | none | `reason` |
| `nova_nodes` | gauge | DB at scrape (`nodes`) | none | `status`, `trust_state`, `assignment_sync_state` |
| `nova_below_floor_replica_debt` / `nova_node_below_floor_replicas` | gauge | DB at scrape (`pin_assignments ⨝ nodes`) | none | — / `node_id` |
| `nova_node_draining` / `nova_node_drain_pending_cids` / `nova_node_drain_pending_oldest_seconds` / `nova_node_drain_inflight_cids` / `nova_node_drain_ready` | gauge | DB at scrape (drain-debt queries, D-M7-6f) | none | `node_id` (except the aggregate) |
| `nova_audit_results_total` | counter | DB at scrape (durable `pin_audits` rows) | restart-stable (durable rows) | `result`, `reason` |
| `nova_audit_latency_seconds` | histogram | process-local, instrumented at the audit outcome site | **resets on coordinator restart** | — |
| `nova_donor_fetch_total` / `nova_donor_fetch_latency_seconds` | counter / histogram | process-local (donor read-source fetch path) | **resets on restart** | `result`, `reason` / — |
| `nova_donor_egress_refusals_total` | counter | process-local (refusal observation site) | **resets on restart** | `reason` |
| `nova_source_selection_failures_total` | counter | process-local (selection path) | **resets on restart** | `reason` |
| `nova_compat_registration_failures_total` | counter | process-local (register handler rejections) | **resets on restart** | `reason` (`incompatible_protocol`, `missing_capability`) |
| `nova_trust_transitions_total` / `nova_reputation_moved_total` | counter | process-local (trust state machine; there is no durable transition log) | **resets on restart** | `from`,`to`,`reason` / `direction` |

Process-local counters resetting on restart is normal Prometheus counter behavior
(`rate()`/`increase()` handle it); the table exists so nobody later "fixes" a reset
by inventing a durable event table M7 never promised. DB-derived families reuse the
existing `internal/db/queries/metrics.sql` queries where they already exist
(`ListAckedNodeDimensions`, `SumCorpusBytesByClass`, `SurvivingCapacity`,
`CountRecentlyUnreachable`) and add scrape queries over the projections otherwise.

**Below-floor replica debt**, precisely: the count of **acked** replicas held on
nodes whose `reputation_score` is below the configured `reputation_floor` but whose
pins have **not** hard-failed — replicas that therefore remain present and countable
until the P2-M6.1 queue lands. Making this debt *visible* (metric + runbook) while
deferring its *automated replacement* is the honest form of D-M6-7's narrowing: the
operator can see the debt, decide, and act, without M7 shipping the bulk-replacement
policy.

### D-M7-2 — Corpus-scale benchmark: a local milestone-exit gate plus a cheap CI regression signal.

The full ~9.8 M-`blob_blocks` benchmark is a reproducible **local exit gate**, not a
per-push CI tax (a full-scale replica is economically unrealistic in CI, and QA-scale
environments hide graceful-degradation behavior — so the release proof runs at scale
locally, and CI guards against *regressions*).

- **`make bench-corpus`** — the authoritative gate. Seeds a large corpus
  (parametrizable N, full ~9.8 M rows at release), runs the hot paths, and emits a
  **structured artifact** under `reports/benchmarks/` (`.json` + `.md`): hardware,
  Postgres + Go versions, row counts, random seed, relevant DB settings, p50/p95/p99
  per hot path, `EXPLAIN (ANALYZE, BUFFERS)` summaries, and pass/fail thresholds.
- **`make bench-corpus-ci`** — a smaller representative run (~100 k–1 M rows) as a
  fast CI regression signal.
- **`make bench-corpus-explain`** — a cheap, deterministic query-plan/index gate
  (asserts the hot queries keep their expected index paths; catches a dropped index
  or an accidental seq-scan without timing noise).

**Safety guard (explicit):** the bench runs against a **scratch database only** — a
dedicated testcontainers instance or an explicitly-provided scratch DSN. It **refuses
to run** against a DSN that looks like a production database (any DSN not explicitly
marked scratch) unless an explicit `--i-know-this-is-scratch`-style override is
passed. It never commits generated corpus data to the repo; `reports/benchmarks/*`
are lightweight result artifacts (JSON/Markdown), never DB dumps.

**Two threshold classes**, so CI never pretends to enforce the release gate:
**release thresholds** apply to the local full-scale run and gate the milestone;
**regression thresholds** apply to the CI run and gate on **asymptotic shape and
query plans**, not absolute wall-clock (hardware-dependent). **Hot paths covered:**
holder selection, source selection (read + repair), reconcile-queue drain, random
audit-block selection, per-node audit selection, delete cascades, projection
rebuild, and backup/restore. **Distributions are skewed** (hub donors holding
disproportionate shares), not uniform — real donor populations have hubs, and
uniform fixtures hide the cardinality that drives performance.

### D-M7-3 — N−1 / mixed-version compatibility: a synthetic capability matrix plus a real cross-version e2e.

Nova's true interop contract is `fed/v1` **capability negotiation**, which is
fail-closed — but "required" is a *configured* set, not an intrinsic property. The
register handler checks `SupportedProtocols` for `fed/v1`, then calls
`wire.NegotiateCapabilities(offered, required)` against
**`ServerConfig.RequiredCapabilities`** (`internal/federation/coordinator/server.go`
— `[]` in M2) and rejects with `missing_capability` only for capabilities in that
configured set. **N−1 ≜ the immediately-prior Phase-2 milestone tag** — verified
present in the repo at authoring time: `p2-m6-possession-audits`.

- **(i) Capability matrix (integration tests).** Drive the negotiation layer with a
  donor advertising a **narrower** capability set, per `fed/v1` capability
  (`internal/federation/wire/messages.go`), classified by how absence fails:
  - **registration-required** (present in the test profile's
    `RequiredCapabilities`; absence ⇒ clean register rejection):
    `pin-change-log/v1` (assignment sync), `snapshot/v1` (recovery from a pruned
    cursor);
  - **route/scheduler-gated** (NOT in `RequiredCapabilities`; absence ⇒ the
    coordinator simply never selects that donor for that path, no crash):
    `blob-transfer/v1` (source-bearing assignment fetch), `read-source/v1`
    (donor-backed reads), `repair-stream/v1` (donor-as-source repair),
    `audit-block-hash/v1` (possession-audit participation).

  The tests **configure `RequiredCapabilities` intentionally per case**, and the
  matrix must test both the configured global-required set and the route-gated
  selection behavior. A capability cannot be claimed "route-gated" in this spec
  while also being present in the coordinator's global `RequiredCapabilities` for
  that test profile — that would only prove registration rejection, not graceful
  route skipping.

- **(ii) Cross-version e2e (external script, not a Go unit test).** Using a
  `git worktree` (or temporary clone) of the `p2-m6-possession-audits` tag: build the
  prior-tag `nova-node`, build the HEAD coordinator, boot a clean test Postgres, and
  run the matrix. **All pairings must prove `join → serve → audit`.** Graceful
  drain/decommission is **required only for HEAD-coordinator pairings** — drain is an
  M7 coordinator/`novactl` primitive, and an N−1 coordinator cannot be asked to
  implement it; in the `N−1-coordinator × HEAD-donor` pairing the donor must
  interoperate without relying on any M7-only lifecycle semantics. Matrix:
  `HEAD×HEAD` (+ drain), `HEAD-coordinator × N−1-donor` (+ drain),
  `N−1-coordinator × HEAD-donor` (join/serve/audit only), and
  `HEAD-coordinator × synthetic-caps-missing-X`. A real prior-tag binary catches
  what a capability mock cannot: changed config defaults, TLS/cert parsing, on-disk
  state formats, CLI flags, and compose assumptions.

- **Cross-version binary tests are NOT schema-downgrade tests.** Each coordinator
  runs against the schema produced by **its own** migration set — HEAD coordinator →
  HEAD schema (with `0016`); N−1 coordinator → N−1 schema (through `0015`). An old
  coordinator binary is never pointed at a HEAD-migrated database (its generated code
  may not tolerate a new column). **DB-upgrade tests are a separate suite** that
  validate forward migration `0015 → 0016` + HEAD startup, not old-coordinator
  behavior after a HEAD migration. Keeping the two apart keeps every failure
  attributable.

### D-M7-4 — Operational drills: each release-critical failure mode gets both an automated proof and a prose runbook.

No general failure-injection framework is built (that would be a milestone of its
own); M7 adds concrete, named drills that prove the release thesis, each with an
operator runbook. Test harnesses are themselves a stability pattern; whole-system
failure behavior is emergent and must be exercised, not asserted. Each drill is
specified as *precondition → fault → expected signal → success condition → what it
does not prove*, so the implementation plan can turn it into tests mechanically.

**Revocation** (involuntary/hostile departure — explicitly *not* the graceful path):
- *Precondition:* a registered, acked donor holding replicas; enough surviving
  holders/capacity for healing.
- *Fault:* `novactl node revoke <node_id>`.
- *Expected signal:* the sweeper observes the revocation
  (`SelectUnsignaledRevoked` → `applyRevokeFallout`), enqueues the node's CIDs,
  emits `federation.node_revoked` once; subsequent register/heartbeat with the
  revoked cert fingerprint is rejected.
- *Success:* affected CIDs return to `healthy` via reconcile/heal; no traffic
  accepted from the revoked identity.
- *Not proven:* graceful exit — revoked nodes are instantly non-countable and
  non-sourceable, which is exactly why drain (D-M7-6) exists.
- *Runbook:* when to revoke vs suspend vs drain; how to verify no traffic is
  accepted post-revoke.

**Provider loss:**
- *Precondition:* nodes spread across ≥ 2 operator-verified failure domains, with
  **enough non-lost capacity and sources for repair** — the fixture must not
  degenerate into a "no available destination/source" case, which would prove
  starvation, not provider-loss recovery.
- *Fault:* stop all nodes in one verified provider/failure domain.
- *Expected signal:* liveness transitions (`suspect → unreachable`), concentration/
  `donor_lost`-tier signals, reconcile enqueue.
- *Success:* strict-Tier-1 healing restores durability using surviving-domain
  holders.
- *Not proven:* recovery when the lost domain held the only copies (that is a
  backup/restore scenario, not a healing drill).
- *Runbook:* provider-outage triage; egress-budget expectations during mass
  healing; when *not* to panic.

**Disk full:**
- *Precondition:* a donor near its capacity limit.
- *Fault:* a targeted **error-path** test — the donor's write/pin path returns the
  normalized `out_of_space` fail reason (`wire.FailReasonOutOfSpace`,
  `internal/federation/wire/messages.go`); not a real filesystem fill.
- *Expected signal:* assignment fail with reason `out_of_space`; the coordinator
  requeues/replaces per existing policy; no donor state corruption.
- *Success:* the donor keeps serving its existing replicas; after space is freed it
  resumes accepting assignments.
- *Not proven:* kernel/filesystem behavior at literal 100 % disk (impractical to
  automate cleanly; the value is the refusal path).
- *Runbook:* how the donor frees space, rejoins, and avoids corrupting local state.

**Corrupt donor state:**
- *Precondition:* a registered donor with synced assignments and local state:
  registration state (`registration.json`), the sync cursor, and the
  assignment/progress store (`internal/node/state`).
- *Fault:* corrupt each state class in turn.
- *Expected signal:* the donor **fails safe** on unparseable state — it must not
  serve or ack against garbage. Cursor/progress corruption triggers the
  `snapshot_required` → snapshot recovery path; registration-state corruption is a
  fail-fast refusal (re-registration is an operator decision, since identity/cert
  material is involved).
- *Success:* snapshot recovery converges the donor back to `current` with no
  spurious acks; the fail-fast cases exit non-zero with a clear diagnostic.
- *Not proven:* recovery from corrupted *ciphertext* (that is the possession-audit /
  healing loop's job, already covered by M6).
- *Runbook:* which files are safe to delete (cursor/progress — recoverable via
  snapshot); which must never be casually regenerated (cert/key material,
  registration identity).

**Graceful decommission** (voluntary departure): drain → replicas rebuilt elsewhere
→ drain debt reaches zero → revoke → node exits (D-M7-6). *Runbook:* the volunteer
leaves cleanly; the operator verifies zero drain debt and no below-floor debt
remains attributable to the departing node.

**Below-floor debt** (observability drill — metric + runbook only, no automated
re-replication in M7): the D-M7-1a metric surfaces non-zero debt when a node drops
below the floor; the runbook explains why present-but-untrusted replicas remain
countable until the P2-M6.1 queue lands, and when to leave the debt alone, drain, or
revoke.

Automated proofs extend the existing loopback-mTLS e2e capstones in
`internal/federation/e2e/` (the M5 healing + M6 possession capstones are the
pattern); disk-full is the one drill exercised as a targeted error-path
unit/integration test rather than a full-stack e2e.

### D-M7-5 — Deferrals, named with their owning milestone.

- **Below-floor *bulk* re-replication queue** (hysteresis around the floor, rate
  limits, a separate "untrusted replica replacement" queue) — **P2-M6.1**. M7 makes
  the debt *observable* (D-M7-1a) and *actionable by runbook* (D-M7-4) but ships no
  automated whole-federation replacement policy. `novactl node drain` (D-M7-6) is
  **not** this queue: it is explicit, operator-initiated, node-scoped, one-shot, and
  non-hysteretic.
- **`envelope_round_trip` whole-blob audit kind** + the two-call
  `/fed/v1/audit/response` form — **P2-M8+**. Its cost model is tied to
  streaming-AEAD / CAR / Range semantics (`ARCHITECTURE_DECISIONS.md` marks v2
  record/block mapping + AAD construction authoritative in P2-M8); landing it in M7
  would create protocol debt just before the envelope design becomes authoritative.
- **Multi-coordinator observability/leader fencing** — **Phase 6**.
- **A public registry publish / release automation beyond the existing
  push-by-digest-on-main CI** — out of scope; M7 documents the release-trust model
  and the operator's manual publish/verify steps, it does not change the
  no-remote-push development posture.

### D-M7-6 — Graceful drain is a bounded, node-scoped operator primitive.

M7 adds **`novactl node drain <node_id>`** (and its inverse, **`undrain`**,
D-M7-6e) — explicit, operator-initiated, one-shot, reusing the existing M5
dirty/reconcile machinery (`MarkReplicationDirtyForNode` +
`EnqueueReconcileForNode`, the same bulk-transition contract the liveness sweeper
uses in `enqueueNodeCIDs`). It is **distinct from the deferred P2-M6.1 below-floor
queue**: no reputation trigger, no hysteresis, no background scanning of all
below-floor nodes, no automatic whole-federation replacement policy. It is the
missing *lifecycle* primitive for donor participation — a system that invites
volunteers must offer a safe voluntary exit.

#### D-M7-6a — State marker: a dedicated `nodes.draining_at` column, not a `placement_weight` overload.

Migration `0016` adds `nodes.draining_at timestamptz` (NULL = not draining) plus an
index supporting the drain-debt queries. **`draining_at` is the authoritative drain
marker.** The `placement_weight = 0` sentinel was considered and **rejected**:
`placement_weight` (a real 0.0–1.0 column, migration `0014`) has a legitimate
independent meaning — "do not choose this node for *new* placement" — which does
*not* imply "this node is leaving." Overloading `weight = 0` for drain would make a
mere placement throttle silently drop the node's replicas from the durability
projection and trigger spurious healing. Drain must be explicit and unambiguous.
`draining_at` also carries its own timestamp for the drain-debt "oldest pending"
metric and for operator forensics. Drain does **not** mutate `placement_weight`;
placement exclusion keys off `draining_at IS NULL` (D-M7-6c), leaving an operator's
independent weight tuning intact. The two markers are semantically distinct
**everywhere**: `placement_weight = 0` without `draining_at` still counts toward
durability; `draining_at` excludes countability even when `placement_weight > 0`
(both are tested, D-M7-8).

#### D-M7-6b — "Sourceable" splits into a safety count and a selection candidate.

The central correctness point. A draining donor is a **temporary source** but **not
a long-term durability guarantee**. These two meanings must not be conflated:

| Property | Draining node |
|---|---|
| eligible for **new placement** | **no** |
| **durability-countable** (`healthy_acked_count`) | **no** |
| counted for **prune/commit safety** (`sourceable_acked_count`, `CountSourceableHolders`) | **no** |
| usable as a **temporary repair/read source** while `active`/`current` | **yes** (deprioritized) |

So a draining node is removed from every *count* that a long-term safety decision
consumes, but remains a *selection candidate* (sorted last) so it can source its own
replacements while it is still alive. This mirrors revocation semantics (a revoked
node stops being countable/sourceable because the recompute filters count only
`active`/`suspect`), but is gentler: the node stays alive and useful during the
drain window instead of being cut off at `t=0`.

#### D-M7-6c — Query changes, classified as safety-count vs placement vs selection.

The implementation plan must audit each sourceability/repair-source query's callers
and classify it before editing; the invariant:

- **Safety-count queries — add `draining_at IS NULL` to the count predicates:**
  - `RecomputeReplicationCounts` (`internal/db/queries/replication.sql`): **both**
    `healthy_acked` **and** `sourceable_acked` exclude draining nodes. These
    projection counts remain the authoritative long-term durability / read-health /
    prune / commit safety counts — a draining donor must never inflate them.
  - `CountSourceableHolders` (`internal/db/queries/storage_state.sql`) and any
    other count feeding commit/prune safety: exclude draining.
- **Placement — add `n.draining_at IS NULL`:**
  - `ListPlacementCandidates`: a draining node is never a new-placement destination.
- **Selection queries — allow draining while `active`/`current`, ordered *after*
  non-draining (fallback only).** M7 **prepends the sort key
  `(n.draining_at IS NOT NULL)`** and otherwise changes no selection policy:
  - donor-backed **read**-source selection (`ListSourceableHolders`, feeding the
    read path in `pkg/coordinator/storage/readsource.go`; currently
    `ORDER BY reputation_score DESC, id`) becomes
    `ORDER BY (n.draining_at IS NOT NULL), reputation_score DESC, id`;
  - donor↔donor **repair**-source selection (`ListRepairSourceHolders`,
    `GetRepairSource`, `IsRepairSourceableForCID`; currently
    `ORDER BY (remaining × reputation) DESC, reputation DESC, id`) prepends the same
    key — the leaving donor can be the source for its own replacement without being
    the *preferred* source.
- **Drain-debt queries (new, D-M7-6f):** direct queries over
  `pin_assignments ⨝ nodes` — do **not** overload
  `blob_replication_state.sourceable_acked_count` for drain state.

#### D-M7-6d — The command is intentionally boring; its edge cases are defined.

`novactl node drain <node_id>`:

1. **Refuse** if the node is not `active` or `suspect` (nothing to drain from a
   revoked/evicted node — that path is `revoke`).
2. **Refuse by default** if `assignment_sync_state <> 'current'` (a mid-reconcile
   node has an unstable CID set); allow with an explicit `--force`.
3. Set `draining_at = now()`.
4. **Fail pending assignments** targeting the node so it stops being the destination
   of new or in-flight work.
5. Mark dirty + `EnqueueReconcileForNode` the node's CIDs with reason
   `node_draining`.
6. Print the next step: *keep the donor running, watch the drain metrics, and
   `revoke` only once drain debt is zero.*

Steps 3–5 execute in a **single DB-direct operator transaction** (the same shape as
the liveness sweeper's per-node fallout transaction); the enqueue can touch many
CIDs, but that is the same bulk-transition contract `enqueueNodeCIDs` already
honors, with recompute deferred to the orchestrator's bounded async drain.

**Idempotency:** running `drain` on an already-draining node is idempotent — it does
**not** reset `draining_at` (the original timestamp is the debt-age anchor), but it
re-enqueues the node's CIDs (harmless; the queue dedupes per reason) and reprints
the current drain debt.

**Re-register / heartbeat must not clear `draining_at`.** `RegisterNode`'s
`ON CONFLICT` update (which re-activates a returning node and refreshes many
columns) **must not touch `draining_at`** — the implementation plan must verify the
conflict-update set excludes it. Only the explicit operator `undrain` clears the
marker. A draining donor that restarts or re-registers is still draining.

**There is no auto-revoke.** Drain and revoke are two deliberate operator steps. A
later `--wait --revoke-after-drain` convenience can be added, but the release does
not need it.

#### D-M7-6e — `undrain` is the tiny, explicit inverse.

`novactl node undrain <node_id>`: clears `draining_at` (sets NULL), marks dirty +
enqueues the node's CIDs for projection recompute (reason `node_undrained`), and —
provided the node is still `active`/`current` — the node resumes counting toward
durability and placement on the next recompute. Without `undrain`, a mistaken drain
would leave a healthy volunteer permanently non-countable until manual SQL surgery.
`undrain` is part of the same lifecycle primitive, **not** a new replication policy:
it writes one column and enqueues one node's CIDs, exactly like `drain`.

#### D-M7-6f — Drain debt, defined exactly; observability and the "safe to revoke" gate.

**Drain debt** for a draining node N: the CIDs with an assignment on N (acked) whose
count of **acked, healthy (`active`/`suspect`), sync-`current`, non-draining**
holders is **below that CID's `target_count`**. **Pending reservations do not count
as safe** — safe-to-revoke requires *acked* replacement holders. Pending progress is
reported separately as `nova_node_drain_inflight_cids{node_id}` so an operator can
distinguish "stuck — no work happening" from "replacement in progress but not yet
acked."

The drain metrics (D-M7-1a) make the two-step UX operable: `nova_node_draining`,
`nova_node_drain_pending_cids{node_id}`,
`nova_node_drain_pending_oldest_seconds{node_id}`,
`nova_node_drain_inflight_cids{node_id}`, `nova_node_drain_ready{node_id}`. The
runbook defines **safe to revoke** as **all** of:

1. `drain_pending_cids == 0` for the node;
2. no `tier1` / `tier2` / `donor_lost` CID depends on the draining node as its
   **only usable temporary source**;
3. the reconcile queue age is bounded / not stuck;

— then the operator runs `novactl node revoke <node_id>` and tears down the donor
host.

### D-M7-7 — Release documentation and operator-doc drift fixes.

- **`docs/quickstart/donor.md`** — replace the M1 stub with the full volunteer
  walkthrough: **digest pinning** (`docker pull …@sha256:<digest>`); the **exact
  `cosign verify` invocation with the identity/issuer policy, copied from the actual
  signing workflow, never invented** — the expected GitHub OIDC issuer and the
  `--certificate-identity`/`--certificate-identity-regexp` value must match the
  trusted main-branch `donor-sbom-sign` job in `.github/workflows/ci.yml` (the
  implementation plan extracts the real values from that workflow); **SBOM
  attestation verification** (`cosign verify-attestation --type spdxjson`);
  **provenance attestation verification**; Nebula enrollment; an annotated
  `node.yaml`; **rollback by digest repin**; **how to confirm the node is
  registered and `current`**; and **how to drain before leaving** (the D-M7-6
  two-step exit).
- **Operational runbooks** — the D-M7-4 drills (revocation, provider loss, disk
  full, corrupt state, graceful decommission/drain, below-floor debt), each written
  as an operator-executable procedure with the "safe to revoke" gate (D-M7-6f).
- **Drift fixes** — `README.md` Phase-2 status (stop saying "M3 next"); the
  `docs/VERSIONING.md` release checklist; `docs/VOLUNTEER_DEPLOYMENT_GUIDANCE.md`
  (point at the now-real `quickstart/donor.md`). Docs drift is an operational
  failure mode for a volunteer-hosted system.

### D-M7-8 — Failure-mode acceptance criteria.

Explicit, test-backed exit conditions (not aspirations):

1. **Metrics plane isolation.** `/metrics` serves on `metrics_listen_addr`
   (default `127.0.0.1:2112`), is not reachable on the public API or fed/Nebula
   planes, `""` disables it, and a bind failure when enabled is startup-fatal
   (D-M7-1).
2. **Label discipline is enforced by test.** An automated test scrapes `/metrics`
   and **fails if any label name is `cid`, `blob`, `path`, `filename`, `url`, or
   `collection`** (or any per-object identifier) (D-M7-1).
3. **Donor stays minimal.** `scripts/check_node_deps.sh` rejects
   `github.com/prometheus/*` in the `cmd/node` graph — demonstrated red against an
   injected import, then green (D-M7-1); the donor image inventory is unchanged.
4. **Below-floor debt is visible, not repaired.** A node dropped below the
   reputation floor surfaces non-zero `nova_below_floor_replica_debt` and **no
   automated re-replication fires** (D-M7-1a, D-M7-5).
5. **Scale proof recorded.** `make bench-corpus` produces a `reports/benchmarks/`
   artifact meeting the release thresholds; `make bench-corpus-explain` fails on a
   dropped index; the bench refuses a non-scratch DSN without the explicit override
   (D-M7-2).
6. **Fail-closed compat, both halves.** A donor missing a capability in the test
   profile's `RequiredCapabilities` is cleanly rejected at register; a donor missing
   a route-gated capability (not in `RequiredCapabilities`) registers fine and is
   simply never selected for that path (D-M7-3).
7. **Cross-version interop.** `HEAD-coordinator × N−1-donor` and
   `N−1-coordinator × HEAD-donor` complete `join → serve → audit`, each coordinator
   against its own schema ceiling; **drain/decommission is exercised only under the
   HEAD coordinator** (D-M7-3).
8. **Every drill has a test and a runbook.** Revocation, provider loss, disk-full
   error path (`out_of_space`), corrupt-state recovery (snapshot vs fail-fast), and
   decommission each have an automated proof and a documented procedure (D-M7-4).
9. **Drain is loss-safe.** A drained node stays a (deprioritized) repair/read source
   while its replicas are rebuilt; it is excluded from `healthy_acked_count`,
   `sourceable_acked_count`, `CountSourceableHolders`, and placement; drain debt
   (acked non-draining holders vs `target_count`, pending not counted) reaches zero
   before the operator revokes (D-M7-6b, D-M7-6f).
10. **Drain ≠ throttle, in both directions.** (a) `drain` does not mutate
    `placement_weight`; (b) `placement_weight = 0` without `draining_at` still
    counts toward durability; (c) `draining_at` excludes countability even when
    `placement_weight > 0` (D-M7-6a).
11. **Drain lifecycle edges.** Draining is idempotent (timestamp preserved);
    re-register/heartbeat do not clear `draining_at`; `undrain` restores
    countability/placement on recompute; `drain` never auto-revokes (D-M7-6d,
    D-M7-6e).
12. **Fresh-host walkthrough.** A host following `docs/quickstart/donor.md` can
    digest-pin, `cosign verify` against the documented (workflow-extracted)
    identity/issuer, verify the SBOM + provenance attestations, enroll, join, and
    serve (D-M7-7).
13. **No frozen-migration or boundary regressions.** `migrations-frozen` and
    `donor-deps-boundary` stay green; `0016` is a forward-only ALTER appended to
    `MANIFEST.sha256` (D-M7-6a).

## Scope

**In.** Migration `0016` (forward-only ALTER: `nodes.draining_at` + drain-debt
index) and the D-M7-6c query classification (D-M7-6); `novactl node drain` +
`undrain` with the defined idempotency/re-register/undrain edges (D-M7-6d/6e) and
the drain-debt metrics + queries (D-M7-6f); the coordinator-only Prometheus
`/metrics` listener with the exact D-M7-1 listener contract, the D-M7-1a
type/source/reset classification, + the extended `check_node_deps.sh` guard
(D-M7-1); the below-floor replica-debt metric (D-M7-1a); the corpus-scale bench
harness — `bench-corpus` / `bench-corpus-ci` / `bench-corpus-explain`, the
scratch-DB safety guard, the two threshold classes, the `reports/benchmarks/`
artifact, and the CI regression job (D-M7-2); the capability-matrix integration
tests (with per-case `RequiredCapabilities` profiles) + the cross-version e2e script
(drain only under HEAD coordinator), with DB-upgrade tests kept separate (D-M7-3);
the drill tests extending `internal/federation/e2e/` + the disk-full
(`out_of_space`) error-path test (D-M7-4); the volunteer `quickstart/donor.md`
walkthrough + operational runbooks + doc-drift fixes (D-M7-7); the failure-mode
acceptance criteria (D-M7-8); spec amendments at the owning milestone (see
Cross-references).

**Drain scope invariant (repeated deliberately):** drain is node-scoped and
operator-initiated. It does not scan all below-floor nodes, does not trigger on
reputation movement, does not add hysteresis, and does not implement the P2-M6.1
untrusted-replica replacement queue.

**Out (deferred, owning milestone named).**

- Below-floor **bulk** re-replication queue (hysteresis / rate-limit / untrusted-
  replacement) — **P2-M6.1** (D-M7-5).
- `envelope_round_trip` whole-blob audit kind + two-call `/fed/v1/audit/response` —
  **P2-M8+** (D-M7-5).
- Donor-local metrics surface — **later opt-in**, once the donor artifact is stable
  (D-M7-1).
- `drain --wait --revoke-after-drain` convenience + any auto-revoke — **not in M7**
  (D-M7-6d).
- Public-registry release automation / changing the no-remote-push posture —
  **out of scope** (D-M7-5).
- Multi-coordinator observability / leader fencing — **Phase 6** (D-M7-5).

## Component ownership

| Concern | Coordinator (operator) | Donor (`nova-node`) | Notes |
|---|---|---|---|
| Prometheus `/metrics` listener | ✓ | — | dedicated `metrics_listen_addr` (`127.0.0.1:2112` default); donor stays `prometheus`-free (D-M7-1) |
| Metric collection (DB-derived + process-local) | ✓ | — | D-M7-1a table; reads `blob_replication_state` / reconcile queue / `nodes` / `pin_audits` |
| `draining_at` marker + drain-debt queries | ✓ | — | migration `0016`; authority is coordinator DB (D-M7-6) |
| `novactl node drain` / `undrain` | ✓ (operator CLI) | — | DB-direct, like `revoke`; donor is unaware it is draining |
| Draining node still sources reads/repairs | ✓ (selects it, sorted last) | ✓ (serves while live) | donor keeps serving `read-source`/`repair-stream` until revoked (D-M7-6b/6c) |
| Corpus-scale bench harness | ✓ | — | coordinator-DB-side, scratch DB only; no donor participation |
| Capability-matrix tests | ✓ (register handler + `RequiredCapabilities` profiles) | — (synthetic donor) | negotiation is coordinator-enforced, fail-closed (D-M7-3) |
| Cross-version e2e | ✓ + ✓ | ✓ + ✓ | real prior-tag donor × HEAD coordinator and the reverse; drain only under HEAD coordinator |
| Failure drills | ✓ (revoke/heal/observe) | ✓ (loss/full/corrupt) | mixed; runbooks are operator-facing |
| Volunteer walkthrough / runbooks | ✓ (authors) | — | `docs/quickstart/donor.md`, workflow-extracted cosign policy |

## Trust-model notes (Phase-2 adversaries; canonical text in `THREAT_MODEL.md`)

- **Metrics endpoint exposure.** `/metrics` can leak operational posture (corpus
  size class, node identities, failure counts). Binding it to explicit loopback by
  default and keeping it off the public/fed planes (D-M7-1) mitigates external
  scraping; label discipline (no per-CID labels, enforced by test) prevents
  object-identity leakage even to an authorized scraper.
- **Supply-chain trust (release).** The one release-trust path is cosign
  keyless/OIDC; the walkthrough pins the **exact identity/issuer policy extracted
  from the signing workflow** so a volunteer verifies provenance rather than
  trusting a tag (threat-model D-sign donor signing; phase exit #4 — the donor build
  graph provably excludes operator-only packages, enforced by `donor-deps-boundary`,
  extended here to `github.com/prometheus/*`).
- **Voluntary vs involuntary departure.** `revoke` is the response to a compromised
  or hostile donor (cut off immediately, retain rows as forensic evidence). `drain`
  is the response to a *trusted* donor leaving on good terms (keep it sourcing until
  its data is safely replicated). Conflating them — draining via revoke — creates a
  last-copy data-loss window under origin pruning; D-M7-6 separates them.
- **Draining node as a temporary source.** A draining node remains a repair/read
  source (deprioritized via the prepended sort key) but is excluded from every
  safety count, so a compromised "I'm draining" claim cannot inflate durability
  accounting — it can only *offer* bytes that are still root-CID/possession-verified
  on use, never *count* as durability. And since `drain` is operator-CLI-only
  (DB-direct, like `revoke`), a donor cannot set its own drain state over the wire.

## Exit criterion

P2-M7 is done when, end to end and test-backed:

1. **Observable.** The coordinator serves `/metrics` per the D-M7-1 listener
   contract with the D-M7-1a families, sources, and reset semantics; label
   discipline is scrape-test-enforced; the donor graph is still `prometheus`-free
   (guarded); below-floor debt and drain debt are visible.
2. **Proven at scale.** `make bench-corpus` records a passing `reports/benchmarks/`
   artifact at corpus scale against a scratch DB; `make bench-corpus-explain` guards
   the hot-path query plans in CI; release vs regression thresholds are distinct.
3. **Compatible.** The capability matrix passes fail-closed and route-gated halves
   under intentional `RequiredCapabilities` profiles, and a real
   `N−1-donor × HEAD-coordinator` (and the reverse) interoperate through
   `join → serve → audit`, each coordinator on its own schema ceiling, with
   drain/decommission exercised only under the HEAD coordinator.
4. **Survivable.** Each D-M7-4 drill has an automated proof and a runbook: a revoked
   or lost donor's blobs heal; a disk-full (`out_of_space`) or corrupt-state donor
   fails safe and recovers; a **drained** donor is decommissioned with **zero
   durability loss** and zero residual drain debt before revoke; a mistaken drain is
   reversible via `undrain`. Drain remains node-scoped and operator-initiated — no
   scanning, no hysteresis, no P2-M6.1 queue.
5. **Shippable to a stranger.** A fresh host following `docs/quickstart/donor.md`
   digest-pins, `cosign verify`s against the workflow-extracted identity/issuer,
   verifies SBOM + provenance attestations, enrolls in the mesh, joins, serves, and
   can later drain and decommission cleanly. `README.md` and the release checklist
   no longer misstate Phase-2 status.
6. **Green gates.** `go build ./...`, `go test ./...`, `donor-deps-boundary`,
   `migrations-frozen`, `gofmt`, and `codegen-check` all pass; `0016` is a forward-
   only ALTER appended to `MANIFEST.sha256`.

## Cross-references

- Master federation design (milestone table P2-M7; phase exit #1/#4):
  `docs/superpowers/specs/phase2/2026-06-11-phase2-federation-design.md`.
- Deferral sources (this milestone's carve-outs): M4.1 design (D-M4.1-15,
  `/metrics` blueprint), M5 design (D-M5-13, `/metrics` + corpus bench), M6 design
  (D-M6-7 below-floor deferral, D-M6-11 slog set, D-M6-1 `envelope_round_trip`
  deferral) under `docs/superpowers/specs/phase2/`.
- Reused machinery (by stable name): `internal/orchestrator` (liveness sweep +
  reconcile drain; `enqueueNodeCIDs`, `EnqueueReconcileForNode`,
  `MarkReplicationDirtyForNode`), `pkg/coordinator/storage/readsource.go`
  (donor-backed read selection), `wire.NegotiateCapabilities` +
  `ServerConfig.RequiredCapabilities` (capability negotiation),
  `wire.FailReasonOutOfSpace` / `NormalizeFailReason` (fail-reason domain),
  `internal/federation/e2e` (loopback-mTLS capstones), queries
  `RecomputeReplicationCounts`, `ListPlacementCandidates`,
  `ListRepairSourceHolders`, `GetRepairSource`, `IsRepairSourceableForCID`,
  `CountSourceableHolders`, `ListSourceableHolders`, `RegisterNode` (whose
  `ON CONFLICT` set must not touch `draining_at`), and the `metrics.sql` set
  (`ListAckedNodeDimensions`, `SumCorpusBytesByClass`, `SurvivingCapacity`,
  `CountRecentlyUnreachable`); `cmd/novactl/node.go` (+ `node_db.go` `revoke` as
  the sibling of `drain`/`undrain`).
- Supply chain: `.github/workflows/{ci.yml,codeql.yml,scorecard.yml}`,
  `docker/{coordinator,node}.Dockerfile`, `scripts/check_node_deps.sh`.
- Normative specs amended at this milestone: `FEDERATION_PROTOCOL.md` (drain +
  capability-compat notes), `HEALING_PROTOCOL.md` (draining-node source/count
  semantics), `DATA_MODEL.sql` (`nodes.draining_at`), `ARCHITECTURE_DECISIONS.md`
  (metrics plane + drain-vs-revoke lifecycle), `docs/THREAT_MODEL.md` (metrics
  exposure; voluntary vs involuntary departure), `docs/VERSIONING.md`,
  `docs/VOLUNTEER_DEPLOYMENT_GUIDANCE.md`, `docs/quickstart/donor.md`, `README.md`,
  and `docs/ROADMAP.md` (the P2-M7 row + master-plan status flip).
