# Recipe: Cold-standby coordinator for read availability

Status: **Operator-side pattern.** Nova does not ship the failover
orchestration. The architecture is compatible with manual cold
standby; this document describes how an operator can set one up.

## What this is for

`THREAT_MODEL.md` § "Acknowledged residual risks" item 3:
single-coordinator deployments tolerate coordinator downtime by
recovering from backups. For most operators this is acceptable:
restore Postgres, set `NOVA_MASTER_KEY`, start the binary,
federation re-syncs within minutes via the change-log endpoint.

For operators who cannot tolerate even the minutes-to-hours of a
fresh restore — typically because read traffic has zero tolerance
for unavailability — a cold-standby host shortens the recovery
window without changing the trust model. The standby holds a
streaming replica of Postgres and the same `NOVA_MASTER_KEY`
versions, ready to be promoted on manual operator command.

This is **not** multi-master HA. There is one active coordinator
at a time. Failover is manual and explicit. Split-brain is
prevented by the operator's discipline, not by consensus.

## What changes versus the default

The default deployment has one host running the coordinator,
Postgres, and Kubo. Backups are off-box and rehearsed; recovery
takes whatever the restore script takes.

The cold-standby deployment has:

- A primary host with the normal coordinator, Postgres, and Kubo.
- A standby host with Postgres in streaming-replica mode, the
  same `NOVA_MASTER_KEY` versions installed, and Kubo running
  with the same swarm key but no production federation traffic.
- A documented failover procedure the operator runs by hand.

Nothing in the Nova protocol changes. The federation sees one
coordinator at any given moment.

## Architecture

```
                      Public Internet
                            │
                            ▼
                +-------------------+
                │ DNS / Anycast IP  │  ← operator-controlled
                +---------+---------+
                          │
              ┌───────────┴───────────┐
              │                       │
              ▼                       ▼
    +---------+---------+   +---------+---------+
    │  PRIMARY HOST     │   │  STANDBY HOST     │
    │  - nginx          │   │  - nginx (idle)   │
    │  - coordinator    │   │  - coordinator    │
    │    (running)      │   │    (NOT running)  │
    │  - postgres       │   │  - postgres       │
    │    (primary)      │◄──┤    (hot replica)  │
    │  - kubo           │   │  - kubo           │
    │    (active)       │   │    (idle peer)    │
    │  - NOVA_MASTER_   │   │  - NOVA_MASTER_   │
    │    KEY versions   │   │    KEY versions   │
    +-------------------+   +-------------------+
```

Both hosts hold the same `NOVA_MASTER_KEY` versions (managed via
the operator's secret system; see `docs/recipes/KEY_ESCROW.md`).
Postgres streaming replication keeps the standby's database
fresh. The standby's Kubo can peer on the Nebula mesh but does
not receive pin assignments — the active coordinator is the only
participant making placement decisions.

## Failover procedure

When the operator decides to fail over (primary host
unreachable, hardware failure, planned maintenance):

1. **Verify the primary is actually down.** A network partition
   that leaves the primary running while the operator promotes the
   standby produces split-brain. The operator confirms via an
   out-of-band channel (cloud provider console, hosting NOC, the
   primary's host monitoring) before promoting.
2. **Promote Postgres on the standby.** Standard streaming-replica
   promotion (`pg_promote()`).
3. **Repoint Nebula / DNS to the standby.** The standby's
   coordinator endpoint becomes the federation's `/fed/v1`
   address. Nebula lighthouse routing or DNS update — operator's
   choice.
4. **Start the coordinator on the standby.** It boots, reads the
   promoted Postgres, loads the master key, and begins serving.
5. **Donors re-handshake.** Existing donors notice the
   coordinator's federation endpoint moved, complete mTLS against
   the standby, and resume polling `/fed/v1/pins/changes`.
6. **Decommission the old primary.** Once the standby is serving
   cleanly, the old primary's host is wiped or otherwise removed
   from the mesh so it cannot reawaken and split-brain.

Recovery from failover (rebuilding a new standby) is the inverse:
deploy a fresh host, start Postgres in replica mode against the
new primary, install master keys, leave the coordinator stopped,
and you are back to the steady-state topology.

## What Nova provides

- The coordinator binary is stateless beyond Postgres and the
  master key. A second host running the same binary with the same
  database and the same key is functionally interchangeable.
- The federation protocol's `/fed/v1/register` is idempotent on
  cert fingerprint. Donors re-handshaking against the standby
  produce no schema churn.
- The change-log endpoint (`/fed/v1/pins/changes`) lets donors
  catch up after any disruption; they observe a transient gap
  during failover and resume from their last known `change_seq`.

## What the operator must build

- The streaming-replica Postgres topology and the promotion
  runbook.
- The Nebula or DNS routing flip. Whether the standby's coordinator
  binds to the same Nebula IP (requiring lighthouse-level
  reassignment) or a different address (requiring the federation
  to learn the new address) depends on the operator's mesh
  topology.
- The secret-management discipline for keeping `NOVA_MASTER_KEY`
  versions in sync across both hosts. **A standby whose master key
  is stale from the primary's rotation is useless;** the secret
  must be propagated to the standby every time the primary
  rotates.
- The failover runbook and its rehearsal cadence. Failover is the
  kind of procedure that fails when it has never been tested
  outside of a real outage.

## What to watch for

- **Split-brain.** The most catastrophic failure mode is "both
  coordinators believe they are primary." Donors talking to one
  while moderation actions land on the other produces inconsistent
  state that crypto-shredding does not repair. The operator's
  out-of-band verification is the only defense.
- **Stale master key on the standby.** Every master-key rotation
  must update the standby's environment. A runbook step "deploy the
  rotation to both hosts" is mandatory; a `novactl` post-rotation
  hook that warns about divergent versions is a useful addition.
- **Pinned-blockstore divergence.** Kubo's local blockstore on the
  standby is not automatically synchronized; it accumulates blocks
  via possession audits and re-replication after promotion. Plan
  for the standby to be slower to serve reads in the minutes after
  promotion while it warms up.
- **This is not multi-master.** Uploads and moderation are not
  highly available during failover; the federation observes a
  write-unavailable window. If write availability is what you
  need, the right answer is shorter restore times, not a different
  Nova.

## Cross-references

- `docs/THREAT_MODEL.md` § "Acknowledged residual risks" item 3
- `docs/THREAT_MODEL.md` § "Out of scope" — multi-master HA and cold-standby rows
- `docs/specs/ENCRYPTION_ENVELOPE.md` § "Master key versioning" — how to keep keys in sync
- `docs/recipes/KEY_ESCROW.md` — escrow for the secrets the standby needs
- `docs/specs/ARCHITECTURE_DECISIONS.md` § "Tier 3" — physical deployment topology is operator's freedom
