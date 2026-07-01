# Possession Audit

Status: **Implemented (P2-M6, 2026-06-29).** `internal/audit/possession` is
the normative implementation. Design:
`docs/superpowers/specs/phase2/2026-06-29-phase2-m6-possession-audits-design.md`.
Plan: `docs/superpowers/plans/phase2/2026-06-29-phase2-m6-possession-audits.md`.

> **Amended by P2-M0 (2026-06-13)** — the challenge deadline is decided by
> **coordinator receive-time**, not the donor-supplied `completed_at` (D10); a
> **synchronous** single-round-trip challenge→response is the primary design
> (the two-call form is a documented fallback); sampling is **weighted by
> stored bytes / pin count / node age / risk**, not flat per node. See
> `docs/superpowers/specs/phase2/2026-06-13-phase2-m0-spec-reconciliation-design.md`.

> **Amended by P2-M6 (2026-06-29) — implemented.** The response carries
> **block bytes** (not a digest); verification is **CID reconstruction**
> (`stored.Prefix().Sum(returnedBytes).Equals(stored)`) — M4.1 removed the
> coordinator's guaranteed local copy so digest-only responses are unverifiable.
> The challenge carries `assignment_id`/`generation`/`block_size` and is
> **assignment-bound**: the donor verifies its local `FileProgressStore` entry +
> recursive pin before returning the block (`BlockGetLocal`, local-only /
> `offline=true`; no Bitswap fetch). A **domain-separated, length-prefixed
> transcript digest** (`"NOVA-POSSESSION-AUDIT-v1" || 0x00 || lp(challenge_id)
> || lp(blob_cid) || lp(assignment_id) || uint64be(generation) || lp(block_cid)
> || uint64be(block_index) || uint64be(block_size) || lp(nonce) || lp(block_bytes)`)
> is recorded in `pin_audits.transcript_hash` (D-M6-3a). `received_at` is the
> coordinator receive-time (NULL on timeout; deadline basis; D10); `decided_at`
> is always set (pass/fail/skip/timeout) and is the indexing/operator-query
> column; both added by migration `0015`. **Synchronous-only is the final
> design**: the two-call `/fed/v1/audit/response` form and `envelope_round_trip`
> challenge kind are **not implemented** (deferred to P2-M7). A hard failure
> (`404` / CID mismatch) **invalidates the specific `pin_assignments` row**
> (`state='failed'`) and enqueues reconcile, correcting M5 acked-only durability;
> a soft failure (deadline exceeded) does not. The donor has a **separate audit
> egress governor** (default 1 % of daily budget; `possession_audit.audit_budget_fraction`);
> governor exhaustion is a `skip`, never a `fail`. Canonical webhook event:
> **`federation.node_suspect`** (this spec's `node.suspect` is the alias).

## Purpose

In Phase 2, the coordinator stops trusting donor `ack` messages at
face value. A donor that acks a pin must be able to prove, on
demand and from local storage only, that it still holds the bytes
it claimed.

Possession audits are challenge-response spot-checks. The
coordinator picks a random recently-acked pin, designates a random
block within the blob, sends a nonce, and demands the donor return
the block's bytes hashed with the nonce within a tight deadline.

A donor that lies (acked but doesn't actually hold) cannot fetch
the block on-demand from another peer fast enough to meet the
deadline, especially because the donor's repair endpoint is
explicitly Bitswap-disabled per `FEDERATION_PROTOCOL.md` and the
coordinator-issued repair tokens are bound to source-and-destination
node IDs that prevent ad-hoc fetches.

## What this is not

- **Not formal Provable Data Possession.** No homomorphic tags, no
  zero-knowledge proofs, no Merkle commitments beyond what IPFS
  already provides. PDP/POR research is Phase 8+.
- **Not Filecoin-style Proof-of-Replication or Proof-of-Spacetime.**
  No tokenomics, no sealing, no continuous-state attestation. Nova's
  trust model assumes a coordinator-administered network, not a
  permissionless storage market.
- **Not a moderation tool.** The audit verifies bytes are present,
  not what they are. Plaintext content moderation is the operator's
  responsibility, executed at upload through the product module's
  scanners.

The audit is a low-cost, high-value pragmatic check: enough rigor
to make donor acks meaningful, no more.

## Challenge protocol

**Synchronous single-round-trip is the implemented design (D-M6-3).** The coordinator
issues the challenge and the donor returns the **raw block bytes** in the same HTTP
response; the coordinator measures latency and decides the deadline from its
**own receive-time**, never from a donor-supplied timestamp (a lying donor would
backdate it). The two-call form (a separate `/audit/response` POST) is a
**design-only fallback, not implemented in M6** (deferred to P2-M7); any
donor `completed_at` is advisory and is never the deadline basis (D10).

### Challenge

The coordinator's audit scheduler picks a random `pin_assignments`
row with `state = 'acked'` and a random `blob_blocks` row for that
CID. It sends a challenge to the donor's inbound endpoint:

```
POST /fed/v1/audit/challenge
Authorization: <coordinator's federation cert via mTLS>
Content-Type: application/json

{
  "challenge_id":  "01H8XYAB7KMQQ2GAQX1234567",
  "challenge_kind": "block_hash",
  "blob_cid":      "bafy...",
  "assignment_id": "7c9e...",
  "generation":    1,
  "block_index":   17,
  "block_cid":     "bafkrei...",
  "block_size":    262144,
  "nonce":         "Bm7WkE2vQX5fK9aPzL3hYr6tNcU8sJoG"
}
```

`challenge_kind`:

- `block_hash` (default; implemented): donor returns the **raw block bytes**;
  coordinator verifies by CID reconstruction
  (`stored.Prefix().Sum(returnedBytes).Equals(stored)`).
- `envelope_round_trip` — **design-only, not implemented in M6** (deferred to P2-M7).

### Response

The donor responds **synchronously** to the `POST /fed/v1/audit/challenge`
request with the raw block bytes:

```
200  <raw block bytes>               # block present and assignment-bound; length == block_size
404                                  # block absent, or assignment/generation stale (clean fail)
```

Implementation requirements on the donor:

- The audit handler MUST query its local Kubo blockstore directly
  (`BlockGetLocal`, `offline=true`). It MUST NOT trigger a Bitswap fetch.
- If the block is absent locally, or if any of the assignment-binding checks
  (conditions 1–6 in "Assignment Binding" above) fail, the donor responds
  with `404`. This is the clean failure indication.
- The donor's HTTP server returns within its handler timeout; the coordinator
  stamps `received_at` after the full body read. **The coordinator uses its own
  `received_at` for the deadline decision**; any donor-supplied `completed_at`
  is advisory and is never the deadline basis (D10).

> **Design-only fallback (not implemented in M6).** The original two-call form —
> a separate `POST /fed/v1/audit/response` carrying `result_hash` — and the
> `envelope_round_trip` challenge kind are **not implemented in M6** (deferred
> to P2-M7). The synchronous single-round-trip on `POST /fed/v1/audit/challenge`
> is the sole implemented protocol.

### Verification

The coordinator records the result in `pin_audits`:

```sql
INSERT INTO pin_audits (
    blob_cid, node_id, challenge_kind, nonce, deadline,
    result, latency_ms, bytes_verified, error,
    challenged_at, received_at, decided_at, transcript_hash
) VALUES (...);
```

`result`:

- `pass` — CID reconstruction succeeds
  (`stored.Prefix().Sum(returnedBytes).Equals(stored)`) AND the coordinator
  **received** the full response before the deadline (`received_at ≤ deadline`;
  coordinator receive-time, not the donor-supplied `completed_at`; D10). On a
  pass, the domain-separated, length-prefixed transcript digest
  (`"NOVA-POSSESSION-AUDIT-v1" || …`) is stored in `transcript_hash`; it is
  **not** the wire response — it is computed coordinator-side for the audit record.
- `fail` — CID mismatch, deadline exceeded, or donor returned `404`
  (block absent or assignment stale). `received_at` is NULL on deadline
  timeout; `decided_at` is always set.
- `skip` — the audit could not run (e.g., donor was `unreachable` before
  challenge dispatch, or the donor's audit governor was exhausted).

## Schedule and sampling

| Parameter | Default | Notes |
|---|---|---|
| Base per-node challenge interval | 1 hour | Baseline cadence, **modulated by the size/risk weight below — not flat** (D10) |
| **Sampling weight (D10)** | `~ stored_bytes × pin_count × age_factor × risk` | A donor holding more bytes / more pins is challenged proportionally more; flat per-node sampling under-audits large custodians |
| Newly-acked pin priority | within 15 min | New acks get challenged once shortly after to verify the donor isn't lying immediately on receipt |
| Trust/reputation-weighted skip | `trusted` & reputation ≥ 0.95 challenged 25 % less often | Trusted nodes get fewer challenges |
| Trust/reputation-weighted increase | `probationary` or reputation < 0.5 challenged 4× more often | Probation oversampling (D9 `trust_state`) |
| Challenge deadline | 30 seconds | **Coordinator receive-time** window; tight enough that on-demand network fetch fails |
| Block selection | uniform random over `blob_blocks` rows for the CID | Single-block blobs use the only block |
| Per-coordinator audit budget | 1 % of node bandwidth_budget_bytes_per_day | Audits do not consume meaningful donor budget |

Operator-tunable in `operator.yaml` under `possession_audit`.

## Reputation impact

Each result updates the donor's `reputation_score` (column on the
`nodes` table, range 0.0..1.0):

- `pass` — `score = min(1.0, score + 0.01)` (slow positive drift)
- `fail` (deadline exceeded) — `score *= 0.95` (mild penalty for
  network latency)
- `fail` (404 — donor doesn't have the block) — `score *= 0.5`
  (severe penalty; this is the lying-donor case)
- `fail` (hash mismatch) — `score = 0` and pin is requeued; further
  audits gate on operator review

A node whose reputation drops below `reputation_floor` (default 0.5
— operator configurable in `operator.yaml`) is excluded from new
pin assignments. Persistent failures (e.g., score < 0.1 for 24
hours) trigger a `federation.node_suspect` webhook (alias: `node.suspect`)
and may justify operator-initiated revocation.

## Anti-cheat: source-bound repair tokens

The audit is meaningful only if a lying donor cannot satisfy the
challenge by fetching the block from another peer in the
challenge window.

The federation protocol's repair-transport design (see
`FEDERATION_PROTOCOL.md` § "Repair transport") binds every repair
fetch to a coordinator-issued, time-limited, source-and-destination-pinned
**Ed25519** token (donors hold only the public key, so they cannot mint tokens;
D1). Therefore a donor under audit cannot lawfully fetch the block from another
peer during the audit window.

A donor that bypasses the protocol (uses Bitswap, opens
unauthenticated HTTP to other peers, etc.) is malicious and the
audit's deadline-based detection is the appropriate response.

**What a pass proves (and does not).** A passed audit demonstrates **timely
retrievability of the challenged block under the node's federation identity**,
within the coordinator-measured deadline — *not* unique physical residency. The
Ed25519 source-and-destination-pinned repair tokens (D1) and the
Bitswap-disabled repair path are what make "timely retrievability under the node
identity" a meaningful possession signal.

## Audit-aware orchestration

The orchestrator's source selection (see `HEALING_PROTOCOL.md`)
uses `reputation_score` as a multiplier on `step_capacity`. This
means failed audits naturally reduce a node's contribution to
healing without the orchestrator having to track audits directly.

The orchestrator also considers audit recency: a CID with three
acked pins of which only one has been recently audited is treated
as having "less verified durability" than three recently-audited
pins. The healing pass prefers re-replicating to recently-audited
nodes.

## Performance and cost

The audit budget is intentionally tiny relative to donor budgets:

- Challenge size: ~200 bytes.
- Block return size: 256 KiB max (one chunk).
- Per-audit cost: ~256 KiB egress + sha256 of 256 KiB +
  HTTP overhead.
- Per-donor daily cost at hourly cadence: 24 audits × ~256 KiB
  ≈ 6 MiB/day.

For a 50 GB/day residential donor, that is 0.012 % of their daily
budget. For a 2 TB/day VPS, even less. Operators pay this cost from
the `bandwidth_budget` accounting; the orchestrator deducts it
honestly.

## Schema

The `pin_audits` table is defined in `DATA_MODEL.sql`:

```sql
CREATE TABLE pin_audits (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    blob_cid          text NOT NULL REFERENCES blobs (cid) ON DELETE CASCADE,
    node_id           uuid NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    challenge_kind    text NOT NULL,
    nonce             text NOT NULL,
    deadline          timestamptz NOT NULL,
    result            audit_result,
    latency_ms        integer,
    bytes_verified    bigint,
    error             text,
    challenged_at     timestamptz NOT NULL DEFAULT now(),
    received_at       timestamptz,         -- P2-M6 migration 0015: coordinator receive-time; NULL on timeout; AUTHORITATIVE for deadline (D10)
    decided_at        timestamptz,         -- P2-M6 migration 0015: always set (pass/fail/skip/timeout); indexing / operator-query column (D-M6-2a)
    transcript_hash   bytea,               -- P2-M6 migration 0015: domain-separated, length-prefixed audit digest (D-M6-3a)
    completed_at      timestamptz          -- donor-supplied; ADVISORY only, never the deadline basis (D10)
);
```

> **P2-M0 note (D10).** `received_at` (coordinator receive-time) is the
> authoritative deadline basis; donor `completed_at` is advisory. Sampling
> weight (stored bytes / pin count / node age / risk) is computed from existing
> `nodes` / `pin_assignments` counts, not new columns.
>
> **P2-M6 note (migration 0015).** `received_at`, `decided_at`, and
> `transcript_hash` were added by migration `0015` (P2-M6). `decided_at` is
> always set (unlike `received_at`, which is NULL on timeout) and is the
> column to query for audit history. `transcript_hash` stores the
> `"NOVA-POSSESSION-AUDIT-v1"` domain-separated digest (D-M6-3a). The
> `nodes` table also received `trust_epoch_started_at`,
> `trust_review_required_at`, and `trust_review_reason` in the same migration.

## What this does not protect against

- A donor running a custom Kubo with relaxed semantics that lies
  about local block presence — fails open to the donor at the
  cost of a hash-mismatch detection (severe penalty, escalates).
- A donor that runs the full Nova federation alongside another
  storage backend and lies about provenance — same as above; the
  hash check on the actual content catches this.
- A donor that holds a CID's bytes but periodically becomes
  unreachable due to legitimate ISP/host issues — the audit fails
  gracefully (`skip` or `fail` with deadline exceeded) and slow
  reputation drift recovers when the donor stabilizes.
- A coordinator compromise that issues forged challenges — the
  audit is between coordinator and donor; a compromised coordinator
  is already total-compromise per the threat model and the audit
  is moot.

## Cross-references

- Federation protocol: `docs/specs/FEDERATION_PROTOCOL.md` (mTLS,
  repair tokens, donor inbound endpoint).
- Healing math: `docs/specs/HEALING_PROTOCOL.md` (reputation-
  weighted source selection).
- Local fixity: `docs/specs/INTEGRITY_AUDIT.md` (Phase 1 internal
  checks).
- Schema: `docs/specs/DATA_MODEL.sql` (`pin_audits`, `blob_blocks`,
  `blob_manifests`, `nodes.reputation_score`).
