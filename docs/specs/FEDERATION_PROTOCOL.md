# Federation Protocol

Status: **Phase 0 — normative.** `internal/federation` (coordinator
side) and `cmd/node` (donor side) must conform exactly.

## Purpose

The federation protocol is how a donor's pinning node — running
elsewhere on the operator's mesh VPN — exchanges state with the
coordinator. It covers registration, heartbeats, pin assignment
delivery, ack/fail reporting, and the poison-pill node-revocation
flow.

The protocol is **HTTP/JSON over Nebula**. Donor nodes are HTTP
clients only — they never expose a listening port. The coordinator's
federation endpoint sits inside the Nebula overlay and is not
reachable from the public internet.

## Authentication

Every federation request is mutually authenticated by the Nebula
mesh:

- The donor node's Nebula certificate is signed by the operator's
  Nebula CA at registration.
- The coordinator presents a Nebula cert that the node validated
  against the same CA.
- The Nebula transport is encrypted; the inner HTTP traffic is
  cleartext-acceptable, but the coordinator implementation MUST
  bind its federation endpoint only to the Nebula interface.

The `nebula_cert_fingerprint` extracted from the donor's mTLS handshake
is the durable identity. `nodes.id` (UUID) is the operator-issued
short reference but the fingerprint is what `internal/federation`
authenticates against on every request.

## Endpoints

All paths are under `/fed/v1/` on the coordinator's Nebula interface.
JSON bodies use snake_case field names. Timestamps are RFC 3339.

### `POST /fed/v1/register`

Sent once, on the donor node's first boot. Idempotent on
`nebula_cert_fingerprint`.

Request:

```json
{
  "client_version": "0.1.0",
  "nebula_cert_fingerprint": "sha256:f8...",
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
    "max_offline_window_seconds": 2592000
  }
}
```

The coordinator validates the Nebula cert fingerprint, inserts (or
updates) the `nodes` row, and returns operating parameters. The
donor node persists `node_id` and uses the returned intervals for
its main loop.

### `POST /fed/v1/heartbeat`

Sent every `heartbeat_interval_seconds`. Updates `nodes.last_seen_at`
and supplies operational telemetry.

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

`kubo_health` is one of `ok`, `degraded`, `unreachable`. The donor
node performs its own embedded Kubo health probe before sending.

Response `200 OK`:

```json
{
  "config_updates": null
}
```

`config_updates` is `null` when nothing has changed. When the operator
updates a node's policy via `novactl`, the next heartbeat returns the
new values and the donor node applies them locally on receipt.

### `GET /fed/v1/pins/assigned`

The donor node polls this every `pins_poll_interval_seconds`. The
response is the **complete current set of assignments** for this
node. The donor node diffs against its local pin set and acts:

- Pin (fetch and `ipfs pin add`) any CIDs in the response that the
  donor does not already hold.
- Unpin (`ipfs pin rm`; IPFS GC reclaims storage on its own
  schedule) any locally-pinned CIDs that are absent from the
  response.

This is intentionally pull-based. The coordinator never pushes; the
donor never opens a listening socket.

Query parameters:

| Name     | Type    | Notes                                      |
|----------|---------|--------------------------------------------|
| `cursor` | string  | Pagination cursor; omit for first page.    |
| `limit`  | integer | 1..1000; default 1000.                     |

Response `200 OK`:

```json
{
  "data": [
    {
      "cid": "bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi",
      "byte_size": 1048576,
      "assigned_at": "2026-05-09T12:34:56Z",
      "priority": "tier1"
    }
  ],
  "cursor": "eyJsYXN0X2NpZCI6ICJiYWZ5Li4uIn0",
  "epoch": 421
}
```

`priority` is one of `tier1`, `tier2`, `normal`. Tier 1 pins are
critical (CIDs that, including this assignment, are at fewer than 2
acked replicas) and the donor SHOULD prioritize them in its own
internal fetch queue. The orchestrator orders the page so Tier 1
appears first.

`epoch` increments every time the orchestrator changes its
assignment view. The donor stores it; if the next poll's `epoch`
matches its cached one, the donor MAY skip the diff entirely.

### `POST /fed/v1/pins/{cid}/ack`

Sent when the donor has successfully pinned a CID locally.

Request:

```json
{
  "byte_size": 1048576,
  "ipfs_pin_status": "pinned"
}
```

Response `204 No Content`.

The coordinator transitions the corresponding `pin_assignments` row
to `state = 'acked'` and sets `acked_at`.

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
`network_error`, `kubo_error`, `other`}.

Response `204 No Content`.

The coordinator transitions the row to `state = 'failed'`. The
orchestrator's next tick re-selects a different destination for the
same CID. After three consecutive failures from the same node for
distinct CIDs, the orchestrator marks the node as `degraded`.

## Sequence diagrams

### First boot → steady state

```
Donor                                           Coordinator
  |                                                  |
  | (read kubo-config.json hardening; refuse if bad) |
  | (start hardened Kubo daemon; private swarm key)  |
  | POST /fed/v1/register ──────────────────────────►|
  |                              ◄── 201 + node_id, config
  | (loop: every heartbeat_interval)                 |
  | POST /fed/v1/heartbeat ─────────────────────────►|
  |                              ◄── 200 + config_updates?
  | (loop: every pins_poll_interval)                 |
  | GET /fed/v1/pins/assigned ──────────────────────►|
  |                              ◄── 200 + data, cursor, epoch
  | (diff against local; for each new CID:)          |
  |   ipfs pin add (Kubo fetches from swarm)         |
  | POST /fed/v1/pins/{cid}/ack ────────────────────►|
  |                              ◄── 204            |
```

### Tombstone (deletion) propagation

```
User           Coordinator                          Donor (next poll)
  |                |                                       |
DELETE /api/v1/blobs/{cid} ──►                             |
  |          (tombstone row, shred key, audit log)         |
  |                | (orchestrator removes pin_assignments)|
  | ◄── 204        |                                       |
                   |  (some donor polls /fed/v1/pins/assigned)
                   | ◄────────────────────────────────────│
                   | 200 (CID absent from response)        |
                   |                                       |
                   |   donor diffs: CID locally pinned but |
                   |   not in assigned set → ipfs pin rm   |
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
                             |                                │ donor's next handshake fails
                             | (orchestrator: re-replicate    │
                             |   any CIDs whose acked count   │
                             |   dropped below R)             │
                             | EMIT webhook federation.node_revoked
```

The revoked node, when it next attempts a Nebula handshake, finds
its certificate refused. It cannot rejoin without a fresh
operator-issued cert.

## Mass-casualty event detection

The coordinator detects a mass-casualty event when more than `0.20 * total_active_nodes`
transition from `active` to `offline` within a 1-hour sliding window.
On detection it:

1. Emits the `federation.degraded` webhook.
2. Increments a metric `nova_federation_mass_casualty_events_total`.
3. Continues healing under the standard Tier-1-priority algorithm
   (see `HEALING_PROTOCOL.md`). **No emergency override of bandwidth
   budgets occurs.**

## Configuration parameters

Operator-tunable in `operator.yaml` under `federation`:

| Key                              | Default       | Notes                          |
|----------------------------------|---------------|--------------------------------|
| `heartbeat_interval_seconds`     | 300           | 60..3600                       |
| `pins_poll_interval_seconds`     | 600           | 60..3600                       |
| `max_pin_concurrency`            | 16            | 1..256                         |
| `max_offline_window_seconds`     | 2_592_000     | 1d..90d                        |
| `mass_casualty_threshold_ratio`  | 0.20          | 0.05..0.50                     |
| `mass_casualty_window_seconds`   | 3600          | 60..86400                      |
| `pin_failure_streak_threshold`   | 3             | distinct-CID failures before degraded |

## Versioning

`/fed/v1/` is the v1 namespace. Breaking changes require a `/fed/v2/`
prefix and a transition period during which both are supported.
Backwards-compatible additions (new optional fields, new endpoints)
are made within v1.

The donor node's `client_version` field in `register` lets the
coordinator emit deprecation warnings for outdated clients via
`config_updates.deprecation_message` on heartbeats.

## Out of scope

- Push notifications from coordinator to donor (would require donor
  to listen).
- Multi-coordinator federation (cross-operator). Each operator runs
  an independent federation; donor nodes belong to exactly one.
- Storage proofs (donor proves it actually holds the bytes). The
  ack is taken at face value; future versions may add periodic
  random-spot-check challenges.
