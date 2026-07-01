# Federation Protocol

Status: **Phase 0 v3 — normative.** `internal/federation` (coordinator
side) and `cmd/node` (donor side) must conform exactly.

> **Amended by P2-M0 (2026-06-13)** — Ed25519 repair tokens (was HMAC,
> D1), re-import + root-CID transfer verification (was `hash(bytes) ==
> cid`, D4), durable `pin_changes` log + `snapshot_required` recovery and
> fail-closed capability-negotiated change-log kinds (D7), identity from
> the verified mTLS cert + capability negotiation (D-cap), donor-
> authoritative bandwidth budgets (D11), and `assignment_id`/`generation`
> on the change log + ack/fail (D6). See
> `docs/superpowers/specs/phase2/2026-06-13-phase2-m0-spec-reconciliation-design.md`.

## Purpose

The federation protocol is how a donor's pinning node — running
elsewhere on the operator's mesh VPN — exchanges state with the
coordinator. It covers registration, heartbeats, pin assignment
delivery, ack/fail reporting, controlled donor-to-donor repair
transfers, and the poison-pill node-revocation flow.

The protocol is **HTTPS over Nebula with mutual TLS authentication**.
Donor nodes are HTTP clients to the coordinator's federation
endpoint, and HTTP servers to other donors **only over the Nebula
interface** for repair fetches. Donors expose no public ports.

## Authentication (v2)

The original Phase 0 spec described a single Nebula-cert-derived
identity, conflating overlay-network auth with HTTP-application auth.
v2 splits the two layers explicitly:

| Layer | Mechanism | Identity |
|---|---|---|
| Mesh transport | Nebula | `nebula_cert_fingerprint` |
| HTTP application | mTLS over Nebula | `federation_cert_fingerprint` |

Both certificates are issued by the operator's CA at registration
time. The Nebula cert authorizes overlay membership; the federation
cert authorizes HTTP API calls to `/fed/v1`. Donors hold both.

The coordinator's federation endpoint:
- Binds only to the Nebula interface address (not 0.0.0.0).
- Requires HTTPS with client certificate verification against the
  operator's CA.
- Reads `federation_cert_fingerprint` from the verified client cert
  and looks up the corresponding `nodes` row.

Each donor's small inbound HTTPS server (Phase 2 — see "Repair
transport" below) follows the same pattern: Nebula-interface-bound,
mTLS with the operator CA, verified peer fingerprint.

## Endpoints

All paths are under `/fed/v1/` on the coordinator's Nebula interface.
JSON bodies use snake_case field names. Timestamps are RFC 3339.

### `POST /fed/v1/register`

Sent once, on the donor node's first boot. Idempotent on
`nebula_cert_fingerprint` + `federation_cert_fingerprint`.

Request:

```json
{
  "client_version": "0.1.0",
  "supported_protocols": ["fed/v1"],
  "capabilities": ["pin-change-log/v1", "snapshot/v1", "repair-stream/v1", "audit-block-hash/v1"],
  "nebula_cert_fingerprint": "sha256:f8...",
  "federation_cert_fingerprint": "sha256:a3...",
  "display_name": "tor-relay-archive-1",
  "geo_declared": "DE",
  "capacity_bytes": 5497558138880,
  "bandwidth_budget_bytes_per_day": 2199023255552,
  "policy_filters": {
    "max_blob_bytes": 104857600,
    "opt_out_categories": ["video"]
  }
}
```

Response `201 Created` (or `200 OK` if already registered):

```json
{
  "node_id": "550e8400-e29b-41d4-a716-446655440000",
  "selected_protocol": "fed/v1",
  "required_capabilities": ["pin-change-log/v1", "snapshot/v1"],
  "config": {
    "heartbeat_interval_seconds": 300,
    "pins_poll_interval_seconds": 600,
    "max_pin_concurrency": 16,
    "suspect_after_missed_heartbeats": 3,
    "unreachable_after_seconds": 3600,
    "evicted_after_seconds": 2592000
  }
}
```

**Identity and capability negotiation (D-cap).** The coordinator derives node
identity from the **verified mTLS federation certificate**, not from any
self-asserted field in the request body; the request's
`nebula_cert_fingerprint` / `federation_cert_fingerprint` are matched against
the verified peer cert and rejected on mismatch. The coordinator selects the
highest common `fed/vN` from `supported_protocols` and the intersecting
`capabilities`, returning `selected_protocol` + `required_capabilities`; if no
compatible set exists it **fails registration** with a clear, machine-readable
error (`incompatible_protocol` / `missing_capability`) rather than degrading
silently. `geo_declared` is **informational only** — placement anti-affinity
uses the operator-verified `failure_domain_id` / `donor_principal_id` set
coordinator-side (see `HEALING_PROTOCOL.md`), never the donor's self-declared
geo.

### `POST /fed/v1/heartbeat`

Sent every `heartbeat_interval_seconds`. Updates `nodes.last_seen_at`
and supplies operational telemetry. Fast path; the coordinator may
process tens of thousands per minute.

Request:

```json
{
  "free_bytes": 4294967296,
  "bytes_uploaded_today": 12884901888,
  "pins_held_count": 12534,
  "kubo_health": "ok",
  "uptime_seconds": 86400
}
```

Response `200 OK`:

```json
{
  "config_updates": null,
  "current_epoch": 421
}
```

### `GET /fed/v1/pins/changes`

**v2 incremental change log.** The donor passes the last
`change_seq` it observed; the coordinator returns assignment
mutations since that point. This is the steady-state path.

```
GET /fed/v1/pins/changes?since_seq=4172836&limit=1000
```

Response `200 OK`:

```json
{
  "changes": [
    { "seq": 4172837, "kind": "assign", "assignment_id": "7c9e...", "generation": 1, "cid": "bafy...", "byte_size": 1048576, "source": { "node_id": "f47a...", "nebula_addr": "10.42.0.5:9443", "token": "<ed25519-repair-token>" } },
    { "seq": 4172838, "kind": "unpin",  "assignment_id": "7c9e...", "generation": 2, "cid": "bafy..." }
  ],
  "next_seq": 4172839,
  "current_epoch": 421
}
```

`kind` is one of `assign`, `unpin`. The donor applies changes in
sequence order, keyed by `(assignment_id, generation)` so a delayed or
duplicated change is idempotent (D6). The **`priority`/`tier1` flag is never
sent to donors** — it is coordinator-only scheduling metadata that would leak
"this is the federation's last safe copy" (master design § component ownership).

**Durable backing + recovery (D7).** The change log is backed by the durable
`pin_changes(sequence bigserial, node_id, assignment_id, generation, kind, cid,
created_at)` table (see `DATA_MODEL.sql`) with a retention window. When a donor's
`since_seq` predates retention, the coordinator returns a machine-readable
**`snapshot_required`** error rather than an incomplete diff; the donor then
recovers via `GET /fed/v1/pins/snapshot`. An **unknown `kind`** for a
state-mutating change **fails closed** — the donor stops and re-syncs rather
than silently skipping it — and new `kind` values are introduced via
**capability negotiation** (D-cap), never as silent backward-compatible
additions.

### `GET /fed/v1/pins/snapshot`

**Recovery path.** Returns the complete current assignment set for
this node, paginated with snapshot-epoch consistency. Donors call
this on first boot, after long offline periods, or whenever
`current_epoch` from `changes` jumps non-contiguously (indicating a
log gap).

Query parameters:

| Name             | Type    | Notes                                                                  |
|------------------|---------|------------------------------------------------------------------------|
| `cursor`         | string  | Pagination cursor; omit for first page.                                |
| `snapshot_epoch` | integer | Required after the first page; pins the donor to a consistent snapshot. |
| `limit`          | integer | 1..1000; default 1000.                                                 |

Response `200 OK`:

```json
{
  "data": [
    {
      "cid": "bafy...",
      "assignment_id": "7c9e...",
      "generation": 1,
      "byte_size": 1048576,
      "assigned_at": "2026-05-09T12:34:56Z"
    }
  ],
  "cursor": "eyJsYXN0X2NpZCI6ICJiYWZ5..."  ,
  "snapshot_epoch": 421
}
```

If `snapshot_epoch` advances mid-pagination, the coordinator returns
`409 Conflict` with the new epoch; the donor restarts from page 1
against the new snapshot.

### `POST /fed/v1/pins/{cid}/ack`

Sent when the donor has successfully pinned a CID locally. The donor
has verified the CID by **deterministic re-import + root-CID comparison**
(see "Repair transport" → D4), not a flat byte hash. The `ack` carries the
`assignment_id` + `generation` it is acking; the coordinator applies the
transition conditionally (`… WHERE assignment_id=$ AND generation=$ AND
state='pending'`) so a delayed ack for a superseded assignment is rejected (D6).

Request:

```json
{
  "assignment_id": "7c9e...",
  "generation": 1,
  "byte_size": 1048576,
  "ipfs_pin_status": "pinned",
  "fetched_from_node_id": "f47ac10b-58cc-4372-a567-0e02b2c3d479"
}
```

`fetched_from_node_id` is the source the donor actually fetched from.
For coordinator-mediated transfers (Phase 1) this is the
coordinator's own node id. For donor-to-donor repair (Phase 2+) this
must match the source the orchestrator designated; mismatches are
audit-logged.

Response `204 No Content`.

### `POST /fed/v1/pins/{cid}/fail`

Sent when a pin attempt could not complete.

Request:

```json
{
  "assignment_id": "7c9e...",
  "generation": 1,
  "reason": "out_of_space",
  "details": "free_bytes=12345 < required=1048576"
}
```

`reason` ∈ {`out_of_space`, `blob_unavailable`, `policy_filter`,
`network_error`, `kubo_error`, `source_unauthorized`, `cid_mismatch`,
`budget_exceeded`, `other`}.

`budget_exceeded` is the donor's **authoritative** local token-bucket refusing
work that would exceed its configured daily budget (D11); the coordinator's
schedule is only a best-effort reservation. `cid_mismatch` is the re-import +
root-CID verification (D4) failing.

`source_unauthorized` and `cid_mismatch` are donor-detected misuse
of the repair transport (see below) and trigger reputation hits on
the offending source.

Response `204 No Content`.

## Repair transport (Phase 2)

> **P2-M4 status (2026-06-23).** P2-M4 implements **coordinator-as-source only**:
> the coordinator serves `GET /fed/v1/blob/{cid}` from its origin and the donor is
> a fetch-only client. The **donor-as-source inbound server** and **donor↔donor
> repair** described below are **P2-M5** (the healing scheduler is their trigger).
> The Ed25519 mint, source-side token verify (`source_node_id` = the reserved
> coordinator source identity), `source_boot_time` + single-use `jti` replay
> defense, and `max_bytes` enforcement all ship in M4 as reusable seams. In M4 the
> destination donor verifies by deterministic re-import + **canonical CID-string
> equality** (it is `go-cid`-free to keep the donor dependency boundary minimal);
> both sides share Nova's deterministic import parameters, so a real match is
> byte-identical. The D11 authoritative egress budget is first debited in M5 when
> a donor serves as a source.

The original Phase 0 spec said donors fetch via `ipfs pin add`,
relying on Bitswap to choose a peer. That defeats the orchestrator's
bandwidth-budget guarantee: Bitswap may pull from a residential
donor while the orchestrator scheduled a high-capacity-VPS source.

v2 introduces a **controlled donor-to-donor repair endpoint** the
destination calls directly against the orchestrator-designated source.

### Donor inbound endpoint

Each donor's small inbound HTTPS server (Nebula-bound, mTLS) exposes:

```
GET /fed/v1/blob/{cid}
```

Authentication:
- Client mTLS cert must be a valid federation cert.
- Request must carry `X-Nova-Repair-Token: <token>`, where `token` is a
  coordinator-issued, time-limited, source-and-destination-pinned grant
  **signed with the coordinator's Ed25519 federation repair-signing key** (D1).
  The signature is **asymmetric**: the coordinator holds the private key; donors
  hold only the **public** key, delivered via `config_updates` on heartbeats.
  (The earlier "HMAC-signed" design was unimplementable — an HMAC verify-key is
  symmetric, so distributing it would let any donor mint tokens.)

Token claims (D1):
- `jti` — unique token id, for single/bounded-use replay defense
- `cid` — the requested blob
- `assignment_id` + `generation` — the assignment this transfer serves
- `source_node_id` — must match the donor receiving the request
- `dest_node_id` — must match the requester's federation cert
- `not_before` / `not_after` — short window, ≤ `repair_token_ttl_seconds`
- `max_bytes` — upper bound the source will serve under this token
- `protocol_version` — the negotiated `fed/vN`

The source donor:
1. Verifies the token's **Ed25519 signature** with the coordinator public key
   (delivered via `config_updates`).
2. Confirms `source_node_id` is its own and `dest_node_id` matches the verified
   requester cert.
3. Enforces **single/bounded-use** via a local `jti` replay cache, so a
   malicious destination cannot replay a valid token to drain the source's
   budget.
4. Confirms it locally holds the CID.
5. Streams the envelope bytes (up to `max_bytes`), debiting its **authoritative
   local token-bucket** — it refuses (`budget_exceeded`) if serving would exceed
   its configured daily budget (D11), regardless of any coordinator reservation.
6. Logs the transfer for the coordinator's reconciliation (heartbeat/transfer
   reports reconcile actual bytes against the best-effort reservation).

The destination donor:
1. Receives the bytes (CAR stream or raw envelope).
2. Verifies the CID by **deterministic re-import through `IPFS_IMPORT_RULES.md`
   and comparing the computed root CID** to the assigned CID — or, for a CAR,
   verifies every block and the root (D4). A flat `hash(bytes) == cid` is wrong
   for any multi-block UnixFS/DAG-PB object: the CID is a DAG root, not
   `sha256(bytes)`. `github.com/ipld/go-car/v2` is already a dependency.
3. Pins locally.
4. Sends `ack` with `fetched_from_node_id = source_node_id`, plus
   `assignment_id` and `generation`.

**This is the only sanctioned repair path in Phase 2.** Bitswap-
backed `ipfs pin add` for repair is explicitly disabled by the donor
implementation; the donor's only repair input is the coordinator's
designated source.

The orchestrator schedules a transfer by including the source
designation in the `assign` change-log entry:

```json
{
  "seq": 4172837,
  "kind": "assign",
  "assignment_id": "7c9e...",
  "generation": 1,
  "cid": "bafy...",
  "byte_size": 1048576,
  "source": {
    "node_id": "f47ac10b-58cc-4372-a567-0e02b2c3d479",
    "nebula_addr": "10.42.0.5:9443",
    "token": "<ed25519-repair-token>"
  }
}
```

For **initial uploads** (not repair), the source is the coordinator
itself; the donor fetches from `/fed/v1/blob/{cid}` on the
coordinator's address.

## Sequence diagrams

### First boot → steady state

```
Donor                                           Coordinator
  |                                                  |
  | (read kubo-config.json hardening; refuse if bad) |
  | (start hardened Kubo daemon; private swarm key)  |
  | (start inbound HTTPS server bound to nebula iface)
  | mTLS handshake (federation cert)                 |
  | POST /fed/v1/register ──────────────────────────►|
  |                              ◄── 201 + node_id, config
  | (loop: every heartbeat_interval)                 |
  | POST /fed/v1/heartbeat ─────────────────────────►|
  |                              ◄── 200 + current_epoch
  | (loop: every pins_poll_interval, OR on demand)   |
  | GET /fed/v1/pins/changes?since_seq=N ───────────►|
  |                              ◄── 200 + changes, next_seq
  | (for each "assign" with source.node_id != self:) |
  |   GET /fed/v1/blob/{cid}     (donor-to-donor)    |
  |     X-Nova-Repair-Token: ... ────────────────────►other donor
  |                              ◄── envelope bytes  |
  |   verify via re-import + root-CID compare (D4)   |
  |   ipfs block put                                 |
  | POST /fed/v1/pins/{cid}/ack ────────────────────►|
  |                              ◄── 204            |
```

### Tombstone (deletion) propagation

```
User           Coordinator                          Donor (next changes poll)
  |                |                                       |
DELETE /api/v1/blobs/{cid} ──►                             |
  |          (tombstone row, shred key (if !legal_hold),   |
  |           audit log, increment change_seq)             |
  | ◄── 204        |                                       |
                   |  (donor polls /fed/v1/pins/changes)   |
                   | ◄────────────────────────────────────│
                   | 200 (change kind=unpin)               |
                   |                                       |
                   |   donor: ipfs pin rm                  |
                   |   IPFS GC reclaims on its own         |
```

### Poison-pill revocation

```
Operator                Coordinator                    Lighthouse / Donor
  |                          |                                |
novactl node revoke <id>     |                                |
  ────────────────────────►  |                                |
                             | nodes.status = 'revoked'       |
                             | DELETE pin_assignments WHERE node_id = ?
                             | (revoke Nebula cert via lighthouse) ─►│
                             | (revoke federation cert)        │ next mTLS handshake fails
                             | (orchestrator: re-replicate     │
                             |   any CIDs whose acked count    │
                             |   dropped below R)              │
                             | EMIT webhook federation.node_revoked
```

## Liveness state machine (v2)

The original four-state model (`active`, `degraded`, `offline`,
`revoked`) collapsed "missed heartbeat" with "long-term gone" into
the same `max_offline_window` timer (default 30 days). That delayed
mass-casualty healing for a month, defeating the whole healing
system.

v2 splits the timers:

| State | Trigger | Counted in `acked_count`? | Notes |
|---|---|---|---|
| `active` | Heartbeating within tolerance | yes | Standard operation |
| `suspect` | Missed 2-3 heartbeats (~15 min) | yes | Avoid flapping; pause new assignments |
| `unreachable` | Missed > 1 hour | **no** | Healing engages immediately |
| `evicted` | Missed > `evicted_after_seconds` (default 30d) | no | `pin_assignments` removed; node must re-register |
| `revoked` | Operator marked compromised | no | Cert revoked, immediate re-replication |

Configurable: `suspect_after_missed_heartbeats`,
`unreachable_after_seconds`, `evicted_after_seconds`.

Healing engagement at `unreachable` (~1 hour after failure) is the
load-bearing change: a mass-casualty event triggers Tier-1 work
within minutes of the failure becoming visible, not 30 days later.

## Mass-casualty event detection

The coordinator detects a mass-casualty event when more than
`mass_casualty_threshold_ratio * total_active_nodes` transition from
`active` to `unreachable` (or further) within
`mass_casualty_window_seconds`. On detection:

1. Emits the `federation.degraded` webhook.
2. Increments `nova_federation_mass_casualty_events_total`.
3. Continues healing under the standard Tier-1-priority algorithm
   (see `HEALING_PROTOCOL.md`). **No emergency override of bandwidth
   budgets occurs.**

## Configuration parameters

Operator-tunable in `operator.yaml` under `federation`:

| Key                                   | Default       | Notes |
|---------------------------------------|---------------|-------|
| `heartbeat_interval_seconds`          | 300           | 60..3600 |
| `pins_poll_interval_seconds`          | 600           | 60..3600 |
| `max_pin_concurrency`                 | 16            | 1..256 |
| `suspect_after_missed_heartbeats`     | 3             | 2..10 |
| `unreachable_after_seconds`           | 3600          | 300..86400 |
| `evicted_after_seconds`               | 2_592_000     | 1d..90d |
| `mass_casualty_threshold_ratio`       | 0.20          | 0.05..0.50 |
| `mass_casualty_window_seconds`        | 3600          | 60..86400 |
| `pin_failure_streak_threshold`        | 3             | distinct-CID failures before reputation hit |
| `repair_token_ttl_seconds`            | 300           | 60..1800 |

## Versioning

`/fed/v1/` is the v1 namespace. Breaking changes require a `/fed/v2/`
prefix and a transition period during which both are supported.
Backwards-compatible additions (new optional fields, new endpoints) are made
within v1. **New change-log `kind` values are NOT silently backward-compatible**
(D7): because a `kind` can drive a state-mutating operation an old donor would
misapply, new kinds are gated by **capability negotiation** (D-cap) and an
unknown kind **fails closed** at the donor.

The donor node's `client_version` field in `register` lets the
coordinator emit deprecation warnings for outdated clients via
`config_updates.deprecation_message` on heartbeats; `supported_protocols` +
`capabilities` (not the free-form version string) are the **interop contract**.

## Out of scope

- Push notifications from coordinator to donor (would require donor
  to listen on the public internet).
- Multi-coordinator HA and cross-operator peering are **out of scope for the
  1.0 line** (every conforming 1.0 deployment is single-coordinator,
  single-federation; donor nodes belong to exactly one federation). The Tier-1
  invariants `T1.27`/`T1.28` were **reframed** (not relaxed) to make room for two
  deliberate **post-1.0** phases — multi-coordinator *single-authority* HA
  (Phase 6) and opaque inter-federation *ciphertext-only* peering (Phase 7);
  independent concurrent authoritative histories (multi-master) remain
  prohibited. See
  `docs/superpowers/specs/phase6/2026-06-12-resilience-and-post-1.0-architecture-design.md`.
- Storage proofs are specified separately in
  `docs/specs/POSSESSION_AUDIT.md`. Donor acks are taken at face
  value in Phase 2's first cut; possession spot-checks raise
  reputation, repeated failures degrade or revoke nodes.

## Cross-references

- Schema: `docs/specs/DATA_MODEL.sql` (`nodes`, `pin_assignments`)
- Healing math: `docs/specs/HEALING_PROTOCOL.md`
- IPFS hardening: `docs/specs/KUBO_HARDENING.md`
- Possession audits: `docs/specs/POSSESSION_AUDIT.md`
- Donor onboarding UX: `docs/VOLUNTEER_DEPLOYMENT_GUIDANCE.md`

---

## P2-M5 amendment — liveness & healing (as-built, D-M5-14)

**Liveness state machine v2 (code-enforced).** The coordinator's `internal/orchestrator`
sweeper transitions `active→suspect→unreachable→evicted` from `last_seen_at` (strict
advancement only). **Heartbeat is the canonical recovery path:** an authenticated
heartbeat flips `suspect/unreachable→active`; a node returning from `unreachable`
re-enters `assignment_sync_state='reconciling'` and **does not count toward `R`**
until it resyncs to the current change-log head (or completes a snapshot). `evicted`
heartbeats are rejected (`registration_required`); re-registration reactivates with
`assignment_sync_state='snapshot_required'`. Countability requires
`status IN (active,suspect) ∧ assignment_sync_state='current'`.

**Endpoint × status matrix.** `pins/changes` pauses for `unreachable`
(`heartbeat_required`); `ack`/`fail` accept active/suspect/unreachable; all reject
`evicted`/`revoked`. A `suspect` node may remain the last surviving repair **source**
but is never a new **destination**.

**Heartbeat egress telemetry (D-M5-6-TEL).** `HeartbeatRequest` carries optional
`egress_budget_remaining_bytes` / `egress_budget_capacity_bytes` /
`egress_refill_bytes_per_second`. These are a **best-effort scheduling hint** only
(persisted on `nodes.last_egress_*`); the donor's token bucket stays authoritative —
an over-optimistic hint still yields a `budget_exceeded` refusal.

**Donor↔donor repair (live).** `repair-stream/v1` is advertised by donors that run a
source server; the coordinator selects a donor **source** only from advertisers (a
non-advertiser stays read-sourceable only — mixed-version safety). The repair token's
`assignment_id`/`generation` name the **source donor's acked** assignment (the source
verifies against its own progress record); the additive `dest_assignment_id`/
`dest_generation` bind the **destination's pending** assignment. The destination donor
verifies that binding (`dest_node_id`==self, `dest_*`==its `PinChange`) **before**
fetching the ciphertext envelope from the source's address; the source debits its
authoritative egress bucket and streams **exactly `byte_size`**; the destination
re-imports + root-CID-verifies before ack. A repair source is late-bound at
`/pins/changes` / `/pins/snapshot` serve time to its **current** address; an
unsourceable stored source is requeued with backoff (never silently substituted).

**Snapshot recovery learns the source.** `SnapshotItem` carries an optional `source`,
so a long-offline donor recovering via snapshot after change-log retention still
learns its repair source (`pin_assignments.source_node_id` is the durable binding;
`pin_changes.source_node_id` is the incremental copy).

---

## P2-M6 amendment — possession audits (as-built, D-M6-4)

### Audit endpoint (donor inbound — coordinator control traffic)

The coordinator's possession-audit scheduler (`internal/audit/possession`) calls a
new endpoint on the donor's inbound mTLS server (`internal/node/source/server.go`):

```
POST /fed/v1/audit/challenge          (coordinator → donor, mTLS)

{
  "challenge_id":   "<uuid>",         -- also the pin_audits.id (PK)
  "challenge_kind": "block_hash",
  "blob_cid":       "bafy...",
  "assignment_id":  "<uuid>",
  "generation":     7,
  "block_index":    17,
  "block_cid":      "bafkrei...",
  "block_size":     262144,           -- bytes; donor length-checks the response
  "nonce":          "<base64>"
}

200  <raw block bytes>                -- block present; length-capped at block_size
404                                   -- block absent / assignment stale (clean fail)
```

**Authorization model — coordinator control traffic only.**
Unlike `GET /fed/v1/blob/{cid}` (which accepts `RoleCoordinator` *or* `RoleNode`
for donor↔donor repair), the audit route is **strictly coordinator-gated**:
- Peer certificate role MUST be `RoleCoordinator`; `RoleNode` is refused.
- **No repair token required or accepted** — mTLS coordinator identity plus
  assignment binding is the full authorization. This is control traffic, not a
  budgeted byte transfer.
- A handler-level timeout shorter than the coordinator's deadline ensures the
  coordinator's `received_at` stamp is the binding decision.
- A per-node concurrency cap prevents audit amplification.
- The donor-side **audit egress governor** (`possession_audit.audit_budget_fraction`,
  default 1 % of daily budget) gates the block read before any bytes are served;
  an over-budget audit returns an appropriate error and the coordinator records
  `skip` with `audit_budget_exhausted`.

**Verification.** The coordinator verifies the returned block bytes by
reconstructing the CID from the stored prefix — codec/version/mhtype-agnostic,
no coordinator local copy required (M4.1 removed that guarantee):

```
stored      := cid.Decode(block_cid)
recomputed  := stored.Prefix().Sum(returnedBytes)
pass        := recomputed == stored
```

**Capability.** Donors that implement this endpoint advertise
**`audit-block-hash/v1`** in the `capabilities` field of `POST /fed/v1/register`
(and in the register example above). The coordinator only issues challenges to
donors advertising this capability. The whole-blob `envelope_round_trip` kind and
its capability are **not implemented in M6** (deferred to P2-M7).

**Assignment binding (D-M6-4-BIND).** The challenge carries `assignment_id` and
`generation`; the donor verifies all of:
1. Local `FileProgressStore` entry for `blob_cid` is `AckDelivered`.
2. Progress `assignment_id` matches the challenge.
3. Progress `generation` matches the challenge.
4. `ipfsclient.Has(blob_cid)` — recursive pin, not stray blockstore residue.
5. `BlockGetLocal(block_cid)` succeeds — local-only (`offline=true`; no Bitswap).
6. Returned block length equals `block_size` from the challenge.

Any failure of 1–4 or a clean block absence → `404`. Block present but conditions
1–4 failing → `fail` for that pin (not a pass on orphaned residue).
