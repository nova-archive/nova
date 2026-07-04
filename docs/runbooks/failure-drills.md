# Runbook: failure drills (provider loss / disk full / corrupt donor state)

> **P2-M7 (D-M7-4).** Triage procedures for the failure modes the release
> drills prove automatically (`internal/orchestrator/drills_test.go`,
> `internal/node/transfer/outofspace_test.go`,
> `internal/node/state/corrupt_test.go`). Metrics are on the coordinator's
> `/metrics` plane (default `127.0.0.1:2112`, D-M7-1).

## Provider loss (a whole failure domain goes dark)

**What you'll see.** Every node in the domain stops heartbeating; after the
liveness thresholds they sweep `suspect → unreachable`:

- `nova_nodes{status="unreachable"}` jumps by the domain's node count;
- `nova_reconcile_queue_depth{reason="node_unreachable"}` spikes;
- `nova_replication_cids{tier="tier1"}` / `{tier="donor_lost"}` grow for CIDs
  whose surviving copies were concentrated in that domain.

**What the system does without you.** The sweeper fails the dead nodes'
pending reservations and enqueues their CIDs; each scheduler tick recomputes
and re-places toward the surviving domains — from surviving donor holders
when any exist, else from the coordinator's local copy (emergency source).
The provider-loss drill proves the replacement lands in the surviving domain.

**Your job is capacity, not urgency:**

1. Confirm the blast radius is one domain (`novactl node list`, your
   `failure_domain` records) — a multi-domain outage is a mass-casualty event
   (`federation.mass_casualty` webhook) and may be YOUR network, not theirs.
2. Check the queue is draining: `nova_reconcile_queue_oldest_seconds` should
   rise, plateau, and fall. Climbing forever = no eligible destinations —
   add capacity or raise budgets; healing is destination-starved.
3. Expect donor egress budgets to throttle the heal rate
   (`nova_donor_egress_refusals_total` rising is budget enforcement working,
   not a fault).

**When NOT to panic.** Unreachable nodes are not evicted for a long time
(`evicted_after_seconds`); when the provider comes back, the nodes heartbeat,
re-enter `reconciling`, resync, and their replicas count again. Do not revoke
a domain because it is down — revocation is for hostility, not outages.

## Disk full (donor)

**What you'll see (operator).** The donor's transfers fail with the wire
reason `out_of_space` — a CLEAN classified refusal, whether the donor's
configured cap (`storage_max_bytes`) or the actual filesystem (ENOSPC during
the Kubo import) is what filled. The assignment fails; the scheduler places
the replica elsewhere. Nothing corrupts: the donor persists transfer progress
only after a verified import, so a disk-full import leaves no partial state.

**What the volunteer does.**

1. Free space (grow the volume, or raise/inspect `storage_max_bytes`).
2. Restart the donor. It re-registers idempotently, resyncs its assignment
   set, and resumes; nothing needs re-issuing.

**What you (operator) check afterward.** The node returns `active/current` in
`novactl node list`; failed assignments were already re-placed. If you prefer
the node to shed load permanently, that is a `drain`
([donor-lifecycle](donor-lifecycle.md)), not a revoke.

## Corrupt donor state (`storage_dir` damage)

Safe-to-delete table for the donor's `state/` directory — what each file does
when its bytes are garbage (all proven by the corrupt-state drill):

| File | On corruption | Safe to delete? |
|---|---|---|
| `state/cursor.json` | Load error → the agent resyncs from zero; the coordinator answers with the change log from 0 or `snapshot_required` (the M3 recovery contract). | **Yes.** Recovery is automatic on restart. |
| `state/progress.json` | Loads EMPTY; the donor simply re-verifies its assignments. It is a cache of verified-pending acks, never authority. | **Yes.** Costs one re-verification pass. |
| `state/registration.json` | **Fail-fast startup error** (`corrupt registration.json`). The donor refuses to run rather than silently re-register under a new identity. | **No — never casually.** Deleting it makes the donor mint a NEW node_id on next start, orphaning the old node row and its acked replicas. Restore from backup, or coordinate with the operator: re-issue is an operator act. |
| `federation.key` / `federation.crt` | TLS failures against the coordinator. | **No.** Never regenerate yourself — the operator must re-issue (`novactl node rotate-cert` keeps the same node_id). |

**Operator side of a registration/identity loss.** If the volunteer truly lost
`registration.json` + certs: treat the old node as departed (`drain` if it is
still serving under the old identity, else `revoke`), then issue a fresh
bundle. Do not try to graft a new registration onto the old node row.
