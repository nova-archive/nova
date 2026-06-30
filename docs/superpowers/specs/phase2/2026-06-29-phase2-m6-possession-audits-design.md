# P2-M6 — Possession audits & reputation

Design for the sixth Phase-2 milestone. P2-M6 turns the trust scaffolding that
M4 / M4.1 / M5 *consume but never move* into **evidence-driven policy**: the
coordinator stops trusting donor `ack` messages at face value and continuously
spot-checks that an acked donor still holds the bytes it claimed, then lets that
evidence flow into reputation, trust graduation, durability correction, and
source-selection preference (placement is affected only **indirectly**, through
`reputation_score` / `trust_state` / `reputation_floor` — M6 adds no direct
audit-recency term to the M5 placement engine; D-M6-9).

Normative contract: `docs/specs/POSSESSION_AUDIT.md` (v2, already P2-M0-amended
for D10). Reinforced by the master federation design's D9/D10 corrections
(`docs/superpowers/specs/phase2/2026-06-11-phase2-federation-design.md`) and the
M5 design's explicit hand-off notes
(`docs/superpowers/specs/phase2/2026-06-28-phase2-m5-liveness-healing-design.md`
§§ "What M5 consumes vs. what it produces", "Deferrals").

## Context

By the end of M5 Nova can replicate over the v1 envelope, read from donors,
heal after node loss under failure-domain anti-affinity, and place replicas
under trust caps. But the trust signals that machinery reads are **inert**:

- `nodes.reputation_score` is a real column (`0001_init`: `real` 0.0–1.0,
  default 1.0, indexed `nodes_reputation_idx`). M4.1 read-source selection and
  M5 healing/placement already *rank by it and apply a `reputation_floor`* — it
  just sits pinned at 1.0 because **nothing writes it**.
- `nodes.trust_state` exists (`0011`: `probationary` | `trusted` | `suspended`,
  default `probationary`). The placement engine already caps by it
  (`pkg/coordinator/admission/placement.go` `trustWeight`: trusted = full,
  probationary = ½, suspended/unknown = 0; a probationary node may not be the
  sole or second copy of `important` data). But **nothing graduates**
  probationary → trusted.
- `pin_audits` and the `audit_result` enum (`pass`/`fail`/`skip`) already exist
  in frozen `0001_init`, with `pin_audits_node_result_idx (node_id, result,
  challenged_at DESC)` and `pin_audits_blob_idx`. The **only** D10 gap is
  `received_at` (the coordinator-receive-time deadline basis); M6 adds that plus
  `decided_at` and the `nodes` trust-epoch columns for robustness (D-M6-2).

M6 makes those inputs truthful. It plugs into machinery that already exists:

- the **donor inbound mTLS server** (`internal/node/source/server.go`), a clean
  `ServeMux` that already verifies peer-cert role + `wire.Verify`;
- the **coordinator→donor mTLS client** + donor addressing
  (`nodes.source_nebula_addr`, the `read-source/v1` capability) from M4.1;
- the **M5 orchestrator** (`internal/orchestrator`) and the **webhook
  dispatcher** (`internal/notify`) with scoped suppression;
- the **Phase-1 integrity-audit scheduler** (`internal/audit/integrity`) as the
  established in-process-audit-scheduler pattern to mirror;
- `blob_blocks` (Phase 1) for per-CID random-block selection, and
  `blob_manifests.envelope_size` (`0001_init`) for size-weighted selection.

This milestone is intentionally **Core only**: a low-cost, high-value pragmatic
possession check, not formal PDP / PoR / Filecoin-style proof machinery
(`POSSESSION_AUDIT.md` § "What this is not").

## Decisions (ratified with Bug, 2026-06-29)

### D-M6-1 — Scope is possession audits & reputation, as one coherent unit.

M6 ships the complete `acked → challenged → verified → reputation moved → trust
graduated → selection biased` loop over the **`block_hash`** challenge kind.
Audits, reputation movement, trust graduation, and audit-aware selection land
together because they form one reviewable unit (an audit that does not move
reputation, or reputation that nothing graduates on, is a broken half-state).

**Out of scope (deferred):** the rarer `envelope_round_trip` whole-blob
challenge kind → **P2-M7** (revisit in P2-M8 once streaming-AEAD / CAR / Range
semantics settle — whole-envelope audits break the ~256 KiB-per-audit cost
model); the corpus-scale benchmark **gate** + Prometheus `/metrics` → **P2-M7**
(M6 ships fixture + EXPLAIN-oriented query tests and an optional non-blocking
local bench script, not a release gate); automatic **suspension** — `suspended`
stays an operator/security judgment, never set by a single audit (D-M6-8).

### D-M6-2 — Migration `0015` is a forward-only ALTER / reconciliation, not a CREATE.

`pin_audits` and `audit_result` already exist in frozen `0001_init`; `0015` is a
new forward-only file (Phase-1 migrations untouched, appended to
`MANIFEST.sha256`, `migrations-frozen` stays green; sqlc regenerated via
`make sqlc-generate`). It adds exactly:

```sql
ALTER TABLE pin_audits
  ADD COLUMN received_at     timestamptz,        -- response receive-time (NULL on timeout)
  ADD COLUMN decided_at      timestamptz,        -- when the coordinator decided the outcome
  ADD COLUMN transcript_hash bytea;              -- domain-separated digest (D-M6-3a)

ALTER TABLE nodes
  ADD COLUMN trust_epoch_started_at   timestamptz NOT NULL DEFAULT now(),
  ADD COLUMN trust_review_required_at timestamptz,
  ADD COLUMN trust_review_reason      text;

-- Backfill existing nodes' trust epoch to their registration time (nodes.joined_at)
-- so already-tenured donors are not forced to restart the age clock at M6 deploy.
UPDATE nodes SET trust_epoch_started_at = joined_at;
```

#### D-M6-2a — `received_at` and `decided_at` are distinct; `received_at` is the deadline basis (D10).

The coordinator stamps the response **receive-time** (`received_at`) itself —
**after the full bounded response body has been read** (D-M6-3, D-M6-4) — and
the pass/fail deadline decision uses it, never the donor-supplied advisory
`completed_at` (a lying donor backdates it). `received_at` is **NULL** for
timeouts / no-response failures, so it cannot anchor failure history.
`decided_at` is **always** set (pass, fail, skip, timeout, late-response) and is
the authoritative "when did the coordinator decide" column for indexing and
operator queries. `completed_at` is retained only for the documented two-call
fallback and is never load-bearing.

#### D-M6-2b — A trust epoch makes graduation evidence unambiguous.

Graduation (D-M6-8) requires "≥ N passed audits AND 0 hash-mismatches" — which
is meaningless without an anchor. `trust_epoch_started_at` is that anchor:
graduation counts only audit/transfer evidence with timestamp ≥
`trust_epoch_started_at`, and the **age gate is epoch age** (`now -
trust_epoch_started_at`), *not* `now - joined_at`. A hash-mismatch (the
lying-donor case) **resets the epoch** (`trust_epoch_started_at = now()`) and
sets `trust_review_required_at` / `trust_review_reason`, so the trust clock
restarts and an operator sees *why* a node is gated. Graduation additionally
requires `trust_review_required_at IS NULL` — otherwise "operator review
required" would be decorative; a node under review cannot auto-graduate until an
operator clears the marker. The node is **not** auto-suspended (D-M6-8).

#### D-M6-2c — Indexes are EXPLAIN-gated; the existing ones do not serve M6's hot paths.

The existing audit indexes serve "audits for a node by result" and "audits for a
blob", not "most-recent pass per (node, CID)" or "freshly acked within 15 min".
`0015` adds (validated with `EXPLAIN`; dropped if redundant):

```sql
CREATE INDEX pin_assignments_acked_at_idx
  ON pin_assignments (acked_at) WHERE state = 'acked';            -- new-ack fast lane
CREATE INDEX pin_audits_recent_pass_node_blob_idx
  ON pin_audits (node_id, blob_cid, received_at DESC) WHERE result = 'pass';
CREATE INDEX pin_audits_recent_fail_node_idx
  ON pin_audits (node_id, decided_at DESC) WHERE result = 'fail';  -- decided_at: set even on timeout
```

### D-M6-3 — Challenge protocol: synchronous, donor returns the block **bytes**, coordinator verifies by CID recomputation.

Synchronous single round-trip is the primary design (D10). The two-call form in
`POSSESSION_AUDIT.md` is **not implemented**.

**Blocking correction (M4.1): a digest-only response is not verifiable.** M4.1
removed the guarantee that the coordinator holds a local copy — its Kubo is a
prunable cache, not the canonical origin. A donor returning only
`sha256(… block_bytes)` would be **unverifiable** whenever the coordinator has
pruned that block, and "re-fetch to verify" would mean auditing one donor by
asking another — circular and semantically muddy. So for `block_hash` the donor
returns the **challenged block's bytes** (bounded by the recorded
`blob_blocks.block_size` and `possession_audit.max_block_bytes`), and the
coordinator verifies by **content addressing** — recomputing the block CID from
the returned bytes and comparing to `block_cid`, which needs no prior local copy:

```
POST /fed/v1/audit/challenge          (coordinator → donor, mTLS)
{ "challenge_id": "<uuid>", "challenge_kind": "block_hash",
  "blob_cid": "...", "assignment_id": "<uuid>", "generation": 7,
  "block_index": 17, "block_cid": "...", "block_size": 262144, "nonce": "<b64>" }

200  <raw block bytes>                 # block present, length-capped
404                                    # block absent / assignment-stale (clean fail)
```

The coordinator (it is *not* `go-cid`-free; only the donor is) verifies by
**reconstructing the CID from the stored prefix**, which is codec/version/mhtype-
agnostic and avoids assuming the root manifest codec:

```
stored := cid.Decode(block_cid)
recomputed, _ := stored.Prefix().Sum(returnedBytes)   // version, codec, mhtype, length from the stored CID
pass := recomputed == stored                          // byte-for-byte
```

(This is the verifier the Phase-1 integrity audit already uses.)
`assignment_id` / `generation` make the challenge **assignment-bound**
(D-M6-4-BIND); `block_size` lets the donor length-check (D-M6-4-BIND #6). **The
scheduler never issues a challenge for a block whose `block_size >
possession_audit.max_block_bytes`** — such a block is skipped internally
(`max_block_too_large`), never sent, so a conservative operator cap cannot be
turned into a donor failure. (All `importspec` leaves are ≤ 256 KiB, so the cap
is a misconfiguration guard, not a normal path.)

#### D-M6-3a — A domain-separated transcript digest, length-prefixed.

CID recomputation from the returned bytes is the **primary verifier**. A
domain-separated digest is *also* computed over the returned bytes + nonce for
the audit transcript and stable test vectors (defense-in-depth), and is recorded
durably in `pin_audits.transcript_hash` (with the verified byte count in
`bytes_verified`). It is length-prefixed and canonically encoded — not raw
concatenation of variable-length strings:

```
lp(x)       = uint32be(len(x)) || x
result_hash = sha256(
    "NOVA-POSSESSION-AUDIT-v1" || 0x00 ||
    lp(challenge_id) || lp(blob_cid) || lp(assignment_id) || uint64be(generation) ||
    lp(block_cid) || uint64be(block_index) || uint64be(block_size) ||
    lp(nonce) || lp(block_bytes)
)
```

The digest commits the **full challenge semantics** (assignment id/generation and
block size, now that the audit is assignment-bound) — not security-critical given
`challenge_id` uniqueness, but it makes the transcript and test vectors total.

This amends `POSSESSION_AUDIT.md` § "Challenge protocol"/"Verification" (D-M6-13):
the response carries block bytes (not a digest), the challenge carries
`assignment_id`/`generation`, and verification is CID-recomputation-first.

#### D-M6-3b — Insert-before-dispatch; `challenge_id` reuses `pin_audits.id`.

The coordinator generates the `pin_audits` UUID app-side, then:

1. **INSERT** `pin_audits(id, blob_cid, node_id, challenge_kind, nonce, deadline,
   challenged_at, result=NULL)` **before** dispatch (the `result` column is
   already nullable in `0001_init`).
2. Dispatch the challenge; read the bounded body under the deadline.
3. **UPDATE** the row with `result`, `received_at`, `decided_at`, `latency_ms`,
   `bytes_verified`, `error`.
4. **Startup reconcile:** rows still `result IS NULL AND deadline < now() -
   grace` are an interrupted attempt (crash after dispatch); resolve them
   deterministically to **`skip`** with `error='coordinator_crash_or_timeout'`
   and `decided_at=now()` — a crash after dispatch cannot prove the donor
   failed, and `skip` carries no reputation movement (D-M6-7).

This keeps the "no durable job queue" design while making the audit log honest
about attempts that crashed mid-flight. `challenge_id == pin_audits.id` gives
replay/correlation tests a stable id with no new column.

### D-M6-4 — The donor audit endpoint is stricter than blob serving.

`POST /fed/v1/audit/challenge` is added to the existing
`internal/node/source/server.go` `ServeMux`. Unlike the blob route — which
accepts `RoleCoordinator` **or** `RoleNode` because donor↔donor repair needs
donor callers — the audit route is **coordinator control traffic** and is
strict:

- peer cert role MUST be `RoleCoordinator` (reject `RoleNode`);
- **no repair token** required or accepted (mTLS coordinator identity is the
  authorization; this is not a budgeted byte transfer);
- bounded request body; the response is the **single challenged block's bytes**,
  hard-capped at `min(blob_blocks.block_size, possession_audit.max_block_bytes)`;
- a handler-level timeout **shorter** than the coordinator's deadline (so the
  coordinator's receive-time decision is the binding one);
- a per-node single-flight / concurrency cap so audits cannot be amplified;
- the egress governor (D-M6-6) gates the read before any block bytes are read.

The donor advertises **`audit-block-hash/v1`** in its capability set (alongside
`read-source/v1` / `repair-stream/v1`), so the coordinator only challenges donors
that speak this protocol; the envelope-audit capability is deliberately **not**
advertised in M6.

#### D-M6-4-BIND — The audit is assignment-bound, not merely block-present.

`BlockGetLocal(block_cid)` succeeding proves only that *some* bytes sit in the
local blockstore — it does not prove a valid current assignment, a recursive
pin, or non-stale progress. M4.1 already learned "possession alone is
insufficient": its read-source endpoint verifies assignment id, generation,
ack-delivered progress, and a recursive pin before serving. The audit reuses
exactly that chain. With the challenge's `blob_cid` / `assignment_id` /
`generation`, the donor verifies **all** of:

1. local `FileProgressStore` entry for `blob_cid` exists and is `AckDelivered`;
2. progress `assignment_id == challenge.assignment_id`;
3. progress `generation == challenge.generation`;
4. `ipfsclient.Has(blob_cid) == true` (recursive **pin**, not stray block residue);
5. `BlockGetLocal(block_cid)` succeeds;
6. returned block length `== blob_blocks.block_size` (carried in the challenge).

Block present but any of 1–4 failing ⇒ a **fail** for that pin (not a pass on
garbage-collected remnants or stale residue). Any clean local-absence ⇒ `404`.

#### D-M6-4a — `BlockGetLocal` is a new local-only donor primitive.

The donor `ipfsclient` today has `Get` (`/api/v0/cat`, whole-object UnixFS) and
`Has` (`/api/v0/pin/ls`, recursive pinset) — there is **no** raw-block read
(`client.go:121-128` documents that `cat` sufficed for both import paths). M6
adds `BlockGetLocal(ctx, blockCID) ([]byte, error)` over `/api/v0/block/get`
with **local-only / offline** semantics: it MUST read from the local Kubo
blockstore and MUST NOT trigger a Bitswap fetch. The implementation plan must
**pin the exact Kubo mechanism** that guarantees this (e.g. the `offline=true`
RPC parameter and/or a daemon offline/no-peers posture) rather than relying on
the endpoint name — `KUBO_HARDENING.md` disables the public DHT/providership and
constrains the API to loopback, but M6 still owns a test that the primitive
**cannot fetch from peers during an audit** (run against a donor with a peer that
holds the block but the local blockstore does not → must `404`, not fetch). A
block the donor does not hold returns a not-present sentinel → the handler
responds `404` (the clean lying-donor indication). The coordinator side already
has parity via `EmbeddedBackend.BlockGet`.

### D-M6-5 — Coordinator audit scheduler: `internal/audit/possession`, standalone.

A new package mirroring the Phase-1 `internal/audit/integrity` scheduler: an
in-process goroutine on its own cadence, **no `jobs.Queue`**, started in
`cmd/coordinator` next to the M5 orchestrator. Single-coordinator Phase 2 ⇒ no
fencing (multi-coordinator → Phase 6). It owns no durable state beyond the rows
it writes. **Resuming from natural cadence on restart** is concrete: on startup
the scheduler seeds its in-memory per-node `lastRun` from `MAX(decided_at)` per
node over resolved `pin_audits` rows, so a restart does not immediately re-audit
every due node (matching the integrity scheduler's restart behavior).

#### D-M6-5a — Sampling is two-stage; never `ORDER BY random()` over the corpus.

1. **Select due nodes** by cadence modulation (D-M6-5b) over node-level pressure
   inputs — stored bytes, acked pin count, age factor, risk factor — all derived
   from existing `nodes` / `pin_assignments` counts (no new sampling columns,
   per the `POSSESSION_AUDIT.md` D10 note).
2. **Select one acked pin** for that node, preferably weighted by
   `blob_manifests.envelope_size`, with the new-ack fast lane (D-M6-5b) taking
   priority.
3. **Select one `blob_blocks` row** uniformly at random for that CID (single-
   block blobs use the only block).

This avoids double-counting `pin_count` and avoids the Phase-1 integrity
scheduler's `ORDER BY random()` shortcut, which that design explicitly accepted
only for Phase-1 scale and flagged for revisit at donor-federation scale.

#### D-M6-5b — Cadence modulation and the new-ack fast lane.

Base per-node challenge interval 1 h, modulated (`POSSESSION_AUDIT.md` §
"Schedule and sampling"): `trusted` & reputation ≥ 0.95 → ~25 % fewer
challenges; `probationary` or reputation < 0.5 → ~4× more. A **newly-acked pin**
is challenged once within ~15 min of the ack (the fast lane, served by
`pin_assignments_acked_at_idx`) to catch a donor lying immediately on receipt.
The fast lane has its **own bounded quota per tick** so a large upload burst
cannot starve the baseline cadence (audit-storm containment); fast-lane demand
beyond the quota defers to the next tick rather than displacing baseline audits.

### D-M6-6 — Audit traffic uses a separate, donor-authoritative egress governor.

Audit egress must not starve real read/repair service, and the control plane
must not be the only thing keeping audits inside budget (M5's rule:
*the coordinator reserves optimistically; the donor decides*, D-M5-6-TEL).

- Audit traffic is capped by `possession_audit.audit_budget_fraction` of the
  donor's daily bandwidth budget (default 1 %), enforced by a **separate
  donor-side audit governor** — it does **not** debit the M5 source/repair
  egress bucket.
- The coordinator paces to the same fraction; the donor governor is the
  authoritative safety valve.
- If the donor governor rejects an over-budget audit, the coordinator records
  `skip` with `audit_budget_exhausted` — **not** a possession `fail`. Repeated
  exhaustion is an observability/operator signal, never an automatic reputation
  penalty.

### D-M6-7 — A failed audit moves reputation **and corrects durability** — not just punishment.

The central robustness point: M5's `healthy_acked_count` counts
`pin_assignments.state='acked'` on active/suspect, sync-current nodes — it does
**not** drop a pin merely because an audit failed. So a `404`/mismatch that only
docks reputation would leave the failed replica still counted as a healthy acked
holder, defeating the audit's purpose. M6 therefore distinguishes **soft** from
**hard** failures, and a hard failure mutates the *specific* `pin_assignments`
row so M5's acked-only projection self-corrects on the next `RecomputeCID`.

All effects below happen in the **same transaction** as the `pin_audits` UPDATE,
and the `reputation_score` write uses an atomic `UPDATE … RETURNING` (row lock)
— per-node single-flight (D-M6-4) alone does not prevent a lost update against
other writers:

| Outcome | Reputation | Pin / durability effect |
|---|---|---|
| `pass` | `min(1.0, score + 0.01)` | — |
| `fail` — deadline exceeded (**soft**) | `score *= 0.95` | pin stays `acked` (tolerate transient latency; repeated misses drift the score toward the floor) |
| `fail` — `404` / not-present / stale-assignment (**hard**) | `score *= 0.5` | `FailAckedPinAssignmentForAudit(cid,node_id,assignment_id,generation)` → `state='failed'` (acked-state-guarded; **M5's `FailPinAssignment` only fails `pending` rows**, so M6 adds this acked-specific query); `EnqueueReconcile(cid, 'audit_not_present')` |
| `fail` — returned-bytes / CID mismatch / hash mismatch (**hard**) | `score = 0` | `FailAckedPinAssignmentForAudit(…)` → `state='failed'`; `EnqueueReconcile(cid, 'audit_mismatch')`; reset trust epoch + set the review marker (D-M6-2b); emit `federation.node_suspect` (D-M6-10) |
| `skip` — unreachable (pre-dispatch connection/TLS failure) / `audit_budget_exhausted` | — | — |

A failed audit moves reputation in a **row-locked** transaction (`SELECT … FOR
UPDATE` on the node row, then the clamped reputation write) so concurrent writers
cannot lose an update. A **pre-dispatch transport failure** (connection refused,
TLS handshake, no route — the donor may never have received the challenge) is a
`skip`, not a reputation `fail`; only a post-dispatch deadline miss is a `fail`.

**Below-floor reputation does not bulk-replace present replicas (narrowed,
2026-06-30).** When an audit drops a node below `reputation_floor`, M6 does **not**
bulk-fail or bulk-replace that node's existing acked replicas. Below-floor
reputation excludes the node from new placement through the existing M5 placement
floor and deprioritizes it in read/repair source ordering, but its existing acked
pins remain **countable** unless a *pin-specific* hard audit failure invalidates
them. A below-floor node usually still **holds** the bytes (it is slow/unreliable,
not empty); making its pins non-countable would risk corpus-wide replication
storms on reputation flapping near the floor. Bulk re-replication of
present-but-untrusted below-floor replicas is **deferred to P2-M7**, where
hysteresis, rate limits, and a separate "untrusted replica replacement" queue can
be designed explicitly. M6 adds **no new healing path**: hard-fail invalidation
reuses `blob_replication_reconcile_queue` (`EnqueueReconcile`) and the orchestrator
drains it as it already does.

#### D-M6-7a — Final-transaction revalidation guards against stale challenges.

`pin_assignments` is the authority; a challenge can race an `unpin`,
reassignment, eviction, or generation bump between target selection and audit
completion. Before applying *any* reputation movement or pin invalidation, the
final transaction re-checks that the audited row is still
`(cid=blob_cid, node_id=challenged, state='acked', assignment_id=challenged,
generation=challenged)`. If it is not, the result is recorded as **`skip`** with
`error='stale_challenge'` — no reputation move, no pin fail. M6 must never punish
a donor for a coordinator-side stale challenge.

### D-M6-8 — Trust graduation is automatic; demotion is automatic; suspension is operator-controlled.

M6 owns `probationary → trusted` and `trusted → probationary`. All thresholds
are operator-tunable under `possession_audit`; the defaults:

- **`probationary → trusted`** (automatic) iff **all**, counted since
  `trust_epoch_started_at` (D-M6-2b): age (epoch age, `now -
  trust_epoch_started_at`) ≥ 7 d ∧ passed_audits ≥ 10 ∧ acked_transfers ≥ 5 ∧
  hash_mismatch_count = 0 ∧ reputation ≥ 0.95 ∧ `trust_review_required_at IS
  NULL` (no pending operator review).
- **`trusted → probationary`** (automatic) iff reputation < `reputation_floor`
  (0.5) — a rotted node loses its relaxed trust caps without operator action.
- **`suspended`** is **operator-controlled only** (via `novactl` or the
  hash-mismatch operator-review path). No single audit auto-suspends; this keeps
  Kubo edge cases, local disk corruption, and malformed handling from becoming a
  quasi-revocation. The hash-mismatch case sets `score = 0` + the review marker +
  `federation.node_suspect`; an operator decides suspension/revocation.

**Clearing a review marker** is an explicit operator action —
`novactl node trust clear-review <node_id>` (with an admin-API equivalent) —
which sets `trust_review_required_at = NULL`, `trust_review_reason = NULL`, and
restarts the trust epoch (`trust_epoch_started_at = now()`). Without it the
hash-mismatch review gate (D-M6-2b) would be a sticky DB marker with no
operational exit; with it, "operator review required" is a complete loop. The
same surface offers `novactl node trust suspend|unsuspend <node_id>` for the
operator-only `suspended` transition.

Graduation/demotion run as part of the audit-result transaction (D-M6-7) so the
trust state is always consistent with the reputation that drove it. The M5
placement engine already consumes `trust_state`; M6 only makes the transitions
happen.

### D-M6-9 — Audit recency is a bounded preference; it touches source selection, not placement directly.

M6 uses audit recency only for: scheduler priority (D-M6-5), **source**
selection ordering, operator evidence, and at most a re-replication *nudge* for
very stale / high-risk holders. M6 does **not** make stale audits reduce
`healthy_acked_count` or introduce a "verified replication factor" — that would
change M5's acked-only, strict-Tier-1 countability, a far larger semantic change
left unspecified. M6 also adds **no direct audit-recency term to the M5 placement
engine** (`pkg/coordinator/admission`); steady-state placement remains
`~√free × trust × placement_weight` with anti-affinity, trust caps, and the
reputation floor. Placement is affected only *indirectly*, because audits move
`reputation_score` and `trust_state`.

**Two distinct source queries — M6 must not assume one owns both.** Read source
selection and repair source selection are different queries with different
suspended-handling, and M6 keeps that asymmetry deliberate:

| Path | Query | Suspended? | Ordering |
|---|---|---|---|
| Donor-backed **read** (M4.1, user-facing) | `ListSourceableHolders` | **excluded** (`trust_state <> 'suspended'`) + fresh `last_seen_at` | `reputation_score DESC` |
| Donor↔donor **repair** (M5, internal) | `ListRepairSourceHolders` | **not** excluded | `remaining × reputation DESC` |

Suspended-as-source policy, made explicit:

- **revoked** — never a source, never a destination (the cert cannot reconnect;
  rows are retained non-counting).
- **suspended** — never a destination (`ListPlacementCandidates` already
  excludes it); **excluded as a read source** (already, via
  `ListSourceableHolders`) because that is user-facing and `suspended` is an
  operator judgment; **retained as a repair source of last resort** (internal,
  and destination root-CID verification neutralizes provenance risk, so refusing
  the only source would only raise loss risk — consistent with D-M5). M6 changes
  neither M5 query.
- **below-floor but not suspended** — source-eligible (read + repair), sorted
  last by the now-moving reputation; excluded from new placement destinations by
  the reputation floor.

Within those queries M6's contribution is simply that `reputation_score` now
moves (it already flows into both orderings and into M5's `step_capacity`
multiplier with no scheduler change), plus the bounded recent-pass preference as
a tie-breaker where a query already reads reputation.

### D-M6-10 — Webhook: `federation.node_suspect` via the M5 dispatcher.

The canonical event is **`federation.node_suspect`** (the M5 event family is
`federation.*`; `node.suspect` from `POSSESSION_AUDIT.md` is recorded as the spec
alias). It is dispatched through `internal/notify` (best-effort,
HMAC-SHA256-signed-when-configured, bounded worker pool, paranoid-gated, durable
scoped suppression). It fires on persistent severe failure (`POSSESSION_AUDIT.md`:
reputation < 0.1 sustained 24 h) and on the hash-mismatch case. Payload:

```json
{ "node_id": "...", "reputation_score": 0.0, "trust_state": "probationary",
  "reason": "hash_mismatch | score_below_0_1_24h | audit_failure_burst",
  "affected_blob_cid": "...", "audit_id": "...", "suppression_scope": "node_id" }
```

Suppression is scoped by `node_id` (the M5 `ScopeKey` mechanism, so one node's
event never suppresses another's); the per-type window is added to the emitter's
window switch (24 h, matching `federation.shrinking`).

### D-M6-11 — Observability: slog evidence set, metric-shaped (Prometheus → M7).

A USE/RED-named slog signal set, the blueprint for the M7 Prometheus promotion
(matching M5's D-M5-13 approach): `audit.possession.{challenged,passed,failed,
skipped}` — fail `reason` ∈ {`deadline`, `not_present`, `mismatch`}, skip
`reason` ∈ {`budget_exhausted`, `unreachable`} — `audit.reputation.moved`,
`audit.trust.{graduated,demoted}`, `audit.governor.exhausted`.

### D-M6-12 — Config surface.

A new `possession_audit` block, operator-tunable (restart-effect unless trivially
live), validated by the existing config-validation seam: base interval, deadline
(default 30 s), challenge body/response caps, `audit_budget_fraction` (default
0.01), the cadence-modulation factors, and the trust-graduation thresholds
(age / passed-audits / acked-transfers / reputation). Reuses M5's
`reputation_floor`. One or two first-class `/settings` knobs (interval, deadline)
with the rest advanced — matching the M0.6 settings pattern.

### D-M6-13 — Spec amendments at the owning milestone.

- `POSSESSION_AUDIT.md` — status → implemented; **the response carries block
  bytes, not a digest, and verification is CID-recomputation-first** (D-M6-3),
  because M4.1 removed the guaranteed coordinator copy; the challenge carries
  `assignment_id`/`generation` and the audit is assignment-bound (D-M6-4-BIND);
  the domain-separated, length-prefixed transcript digest (D-M6-3a); `received_at`
  vs `decided_at` (D-M6-2a); synchronous-only (two-call form marked
  not-implemented); a failed audit **invalidates the pin** and corrects M5
  durability (D-M6-7); the donor-side audit governor (D-M6-6);
  `federation.node_suspect` canonical name.
- `DATA_MODEL.sql` — annotate `pin_audits.received_at` / `decided_at` and the new
  `nodes` trust-epoch / review columns.
- `HEALING_PROTOCOL.md` — reputation now moves and feeds source selection; audit
  recency is a bounded preference (D-M6-9), not a new countability.
- `THREAT_MODEL.md` — audit collusion / backdating mitigations as implemented
  (coordinator receive-time; synchronous; Ed25519 source/dest-bound repair tokens
  + Bitswap-disabled donor; domain-separated digest).
- `ARCHITECTURE_DECISIONS.md` — a trust-graduation-policy row (automatic
  graduate/demote; operator-only suspend).
- `FEDERATION_PROTOCOL.md` — confirm the audit endpoint section (coordinator→donor
  control traffic, no repair token).

### D-M6-14 — Security & testing.

Anti-cheat rests on existing mechanism (`POSSESSION_AUDIT.md` § "Anti-cheat"): a
donor under audit cannot lawfully fetch the block in-window because repair
transport is Ed25519 source/dest-bound-token-gated and the donor's Kubo is
Bitswap-disabled. A pass proves **timely retrievability under the node
identity**, not unique physical residency.

Tests (unit + loopback-mTLS, no scale gate): `pass`; `404`; hash mismatch
(→ score 0 + requeue + epoch reset + `federation.node_suspect`); deadline
exceeded; **donor-backdated `completed_at` ignored** (receive-time binding);
**replay** of a challenge id; **collusion** (a co-located/peer fetch attempt
within the window must not pass); **`BlockGetLocal` triggers no network fetch**;
two-stage weighted-sampling distribution; new-ack fast lane; egress-governor
exhaustion → `skip` not `fail`; **stale challenge** (unpin/reassign/generation
race) → `skip` with no reputation/pin change; reputation-floor exclusion;
graduation + demotion across the trust epoch; review-gate-blocks-graduation +
`clear-review`; webhook scoped suppression; the audit route rejecting `RoleNode`
and a missing/garbage challenge. Fixture + EXPLAIN-oriented
query tests for the sampling and recency queries; an optional non-blocking local
bench script. `donor-deps-boundary` (the audit endpoint reuses existing donor-
graph packages; `BlockGetLocal` adds no new allowlist entry) and
`migrations-frozen` stay green.

### D-M6-15 — Failure-mode acceptance criteria.

These are explicit, test-backed exit conditions, not aspirations:

1. **No coordinator-origin assumption.** An audit verifies with the
   coordinator's local copy of the block *absent* — the donor returns the bytes
   and the coordinator verifies by CID recomputation (D-M6-3).
2. **Pin invalidation.** A `404` / CID-mismatch moves the *specific*
   `pin_assignments` row out of `acked` and enqueues reconcile; a follow-up
   `RecomputeCID` drops it from `healthy_acked_count` (D-M6-7).
3. **Assignment-bound.** A donor holding orphaned / unpinned / stale-generation
   blockstore residue **fails** the audit (D-M6-4-BIND).
4. **Late-body attack.** A donor that sends headers quickly but the body slowly
   is judged against the deadline **after** the full bounded body read; the
   coordinator caps the body and read time (D-M6-3, D-M6-4).
5. **Audit-storm containment.** An upload burst cannot starve baseline audits —
   the new-ack fast lane has its own per-tick quota (D-M6-5b).
6. **Network-outage classification.** A connection failure *before* the challenge
   is dispatched is a `skip`, not a reputation failure; a `deadline` fail means
   the donor was actually challenged and missed the response window (D-M6-7).
7. **Lost-update protection.** Concurrent reputation/trust writes use an atomic
   `UPDATE … RETURNING` (row lock); per-node single-flight alone is insufficient
   (D-M6-7).
8. **Review gate is real.** A node with `trust_review_required_at` set cannot
   auto-graduate until an operator clears it (D-M6-2b, D-M6-8).
9. **Epoch restart.** A hash-mismatch restarts the trust clock
   (`trust_epoch_started_at = now()`), so graduation evidence accrues afresh
   (D-M6-2b).
10. **Canonical digest framing.** The transcript digest length-prefixes every
    variable-length field; a fuzz/vector test pins the encoding (D-M6-3a).
11. **Stale-challenge safety.** A challenge that races an unpin / reassignment /
    eviction / generation bump records `skip` (`stale_challenge`) and moves
    neither reputation nor pin state (D-M6-7a).
12. **Local-only block read.** `BlockGetLocal` against a donor that lacks the
    block locally but has a peer holding it returns `404` and performs no network
    fetch (D-M6-4a).

## Scope

In: migration `0015` (forward-only ALTER — `pin_audits.received_at` /
`decided_at` / `transcript_hash`, `nodes` trust-epoch/review columns +
`joined_at` backfill, EXPLAIN-gated indexes) (D-M6-2); the `audit-block-hash/v1` capability +
challenge/response wire types where the donor returns block **bytes** and the
coordinator verifies by CID recomputation, with a length-prefixed transcript
digest and insert-before-dispatch + startup reconcile (D-M6-3); the strict,
**assignment-bound** donor audit endpoint + local-only `BlockGetLocal` (D-M6-4);
the `internal/audit/possession` scheduler with two-stage weighted sampling,
cadence modulation, and the quota-bounded new-ack fast lane (D-M6-5); the
donor-authoritative audit egress governor (D-M6-6); transactional reputation
movement **with durability correction** (hard-fail pin invalidation +
reconcile-enqueue + below-floor re-replication) (D-M6-7); the automatic trust
graduation/demotion state machine with operator-only suspension, the trust epoch,
and the review gate (D-M6-8); audit-recency as a bounded source-selection
preference, placement only indirect, the read/repair source-query asymmetry made
explicit (D-M6-9); `federation.node_suspect` via the M5 dispatcher (D-M6-10); the
slog evidence set (D-M6-11); the `possession_audit` config block + `/settings`
knobs (D-M6-12); spec amendments (D-M6-13); the security/collusion/replay test
suite (D-M6-14); the failure-mode acceptance criteria (D-M6-15).

Out (deferred):

- `envelope_round_trip` whole-blob challenge kind — **P2-M7** (revisit P2-M8 with
  streaming-AEAD).
- Corpus-scale benchmark **gate** (manifest insert throughput, random-challenge
  selection, delete cascades, backup/restore at ~9.8 M `blob_blocks` rows) +
  Prometheus `/metrics` — **P2-M7** (the D-M6-11 slog set is its blueprint).
- Automatic node **suspension** — never; operator/security judgment (D-M6-8).
- **Bulk re-replication of present-but-untrusted below-floor replicas** — **P2-M7**
  (D-M6-7): must include hysteresis around the floor, rate limits, and a separate
  "untrusted replica replacement" queue so transient reputation flapping cannot
  trigger corpus-wide replication storms.
- Streaming-AEAD audit semantics (per-record / CAR / Range) — **P2-M8+**.
- Multi-coordinator audit-leader fencing — **Phase 6**.

## Cross-references

- Normative contract: `docs/specs/POSSESSION_AUDIT.md` (amended per D-M6-13).
- Master federation design (D9/D10, milestone table P2-M6):
  `docs/superpowers/specs/phase2/2026-06-11-phase2-federation-design.md`.
- M5 hand-off (consumes vs. produces; `pin_audits` columns land in M6):
  `docs/superpowers/specs/phase2/2026-06-28-phase2-m5-liveness-healing-design.md`.
- Scheduler pattern to mirror: `internal/audit/integrity` (Phase-1 M8).
- Reused machinery: `internal/node/source` (donor inbound mTLS), `internal/notify`
  (webhooks), `internal/orchestrator` + `pkg/coordinator/admission` (placement/
  healing), `ListRepairSourceHolders` / `ListPlacementCandidates`.
- Related healing math: `docs/specs/HEALING_PROTOCOL.md`; threat model:
  `docs/THREAT_MODEL.md`; schema reference: `docs/specs/DATA_MODEL.sql`.
