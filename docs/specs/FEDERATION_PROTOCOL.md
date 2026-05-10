# Federation Protocol

Status: **Phase 0 v2 — normative.** `internal/federation` (coordinator
side) and `cmd/node` (donor side) must conform exactly.

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
    { "seq": 4172837, "kind": "assign", "cid": "bafy...", "byte_size": 1048576, "priority": "tier1" },
    { "seq": 4172838, "kind": "unpin",  "cid": "bafy..." }
  ],
  "next_seq": 4172839,
  "current_epoch": 421
}
```

`kind` is one of `assign`, `unpin`. The donor applies changes in
sequence order.

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
      "byte_size": 1048576,
      "assigned_at": "2026-05-09T12:34:56Z",
      "priority": "tier1"
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
has verified the envelope CID matches the bytes it received.

Request:

```json
{
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
  "reason": "out_of_space",
  "details": "free_bytes=12345 < required=1048576"
}
```

`reason` ∈ {`out_of_space`, `blob_unavailable`, `policy_filter`,
`network_error`, `kubo_error`, `source_unauthorized`, `cid_mismatch`,
`other`}.

`source_unauthorized` and `cid_mismatch` are donor-detected misuse
of the repair transport (see below) and trigger reputation hits on
the offending source.

Response `204 No Content`.

## Repair transport (Phase 2)

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
- Request must carry `X-Nova-Repair-Token: <token>`, where `token`
  is a coordinator-issued, time-limited, source-and-destination-
  pinned grant (HMAC-signed by the coordinator's federation
  signing key).

Token claims:
- `cid` — the requested blob
- `source_node_id` — must match the donor receiving the request
- `dest_node_id` — must match the requester's federation cert
- `not_before` / `not_after` — short window, default 5 min

The source donor:
1. Verifies the token signature (using a coordinator pubkey
   delivered via `config_updates` in heartbeats).
2. Confirms `source_node_id` is its own.
3. Confirms it locally holds the CID.
4. Streams the envelope bytes; debits its own daily bandwidth budget.
5. Logs the transfer for the coordinator's reconciliation.

The destination donor:
1. Receives the bytes.
2. Verifies the envelope CID (`hash(bytes) == cid`).
3. Pins locally.
4. Sends `ack` with `fetched_from_node_id = source_node_id`.

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
  "cid": "bafy...",
  "byte_size": 1048576,
  "priority": "tier1",
  "source": {
    "node_id": "f47ac10b-58cc-4372-a567-0e02b2c3d479",
    "nebula_addr": "10.42.0.5:9443",
    "token": "eyJhbGciOiJIUzI1NiI..."
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
  |   verify CID == hash(bytes)                      |
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
Backwards-compatible additions (new optional fields, new endpoints,
new change-log `kind` values) are made within v1.

The donor node's `client_version` field in `register` lets the
coordinator emit deprecation warnings for outdated clients via
`config_updates.deprecation_message` on heartbeats.

## Out of scope

- Push notifications from coordinator to donor (would require donor
  to listen on the public internet).
- Multi-coordinator federation (cross-operator). Each operator runs
  an independent federation; donor nodes belong to exactly one.
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
