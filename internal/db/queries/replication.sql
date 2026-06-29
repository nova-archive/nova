-- P2-M5 blob_replication_state projection (donor-replica health). The projection
-- is rebuildable cache; authority remains pin_assignments ⨝ node liveness. These
-- queries are consumed by internal/orchestrator/projection.go under a per-CID
-- advisory lock (RecomputeCID), the bounded reconcile drain (DrainReconcile), and
-- the config-change reconcile (RecomputeTargets).

-- name: LockReplicationCID :exec
-- Per-CID transaction-scoped advisory lock (D-M5-2d) so admission, healing, the
-- dirty drain, and ack/fail cannot recompute from or write stale counts for the
-- same CID concurrently. Orthogonal to AcquireChangeLogLock (a single global
-- sequence-ordering lock): this one is keyed per CID in a distinct namespace.
SELECT pg_advisory_xact_lock(hashtext('blob_replication_state'), hashtext($1));

-- name: RecomputeReplicationCounts :one
-- The authoritative recompute over countable holders. healthy counts acks on
-- active/suspect nodes that are sync-current (D5/countability guard). sourceable
-- additionally requires non-suspended + read-source/v1 + a nebula addr (the
-- read-availability count; status-based, NO time predicate — D-M5-2a). in_flight
-- counts ONLY pending reservations on destinations still eligible to complete, so
-- a dead pending row never throttles healing forever (Rev. 5 #5).
WITH holders AS (
    SELECT pa.state,
           n.status,
           n.assignment_sync_state,
           n.trust_state,
           (n.source_nebula_addr IS NOT NULL AND n.source_nebula_addr <> ''
            AND n.advertised_capabilities @> ARRAY['read-source/v1']) AS read_srcable
    FROM pin_assignments pa
    JOIN nodes n ON n.id = pa.node_id
    WHERE pa.cid = $1
)
SELECT
    count(*) FILTER (
        WHERE state = 'acked' AND status IN ('active', 'suspect')
          AND assignment_sync_state = 'current'
    )::int AS healthy_acked,
    count(*) FILTER (
        WHERE state = 'acked' AND status IN ('active', 'suspect')
          AND assignment_sync_state = 'current'
          AND trust_state <> 'suspended' AND read_srcable
    )::int AS sourceable_acked,
    count(*) FILTER (
        WHERE state = 'pending' AND status IN ('active', 'suspect')
          AND trust_state <> 'suspended'
    )::int AS in_flight
FROM holders;

-- name: GetLocalRecoverable :one
-- The DB-derivable part of local_recoverable: the coordinator has a present local
-- copy in a usable role for a repair-eligible blob. backend.Has(cid) confirms at
-- emergency-source mint time (D-M5-8b); this is the projection's cheap predicate.
SELECT (s.local_present AND s.local_role IN ('origin', 'staging', 'cache')
        AND b.state IN ('active', 'quarantined'))::bool AS local_recoverable
FROM blob_storage_state s
JOIN blobs b ON b.cid = s.cid
WHERE s.cid = $1;

-- name: GetReplicationDurabilityClass :one
-- durability_class is authoritative on blob_storage_state (M4.1).
SELECT durability_class FROM blob_storage_state WHERE cid = $1;

-- name: UpsertReplicationState :exec
INSERT INTO blob_replication_state
    (cid, healthy_acked_count, sourceable_acked_count, in_flight_count,
     target_count, safety_tier, local_recoverable, durability_class, dirty, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, false, now())
ON CONFLICT (cid) DO UPDATE SET
    healthy_acked_count    = EXCLUDED.healthy_acked_count,
    sourceable_acked_count = EXCLUDED.sourceable_acked_count,
    in_flight_count        = EXCLUDED.in_flight_count,
    target_count           = EXCLUDED.target_count,
    safety_tier            = EXCLUDED.safety_tier,
    local_recoverable      = EXCLUDED.local_recoverable,
    durability_class       = EXCLUDED.durability_class,
    dirty                  = false,
    updated_at             = now();

-- name: MarkReplicationDirty :exec
-- Bulk liveness transitions mark affected CIDs dirty in their mutation tx; the
-- scheduler recomputes a dirty CID from authority before reserving against it.
UPDATE blob_replication_state SET dirty = true, updated_at = now()
WHERE cid = ANY(sqlc.arg(cids)::text[]);

-- name: EnqueueReconcile :exec
INSERT INTO blob_replication_reconcile_queue (cid, reason)
VALUES ($1, $2)
ON CONFLICT (cid) DO UPDATE SET reason = EXCLUDED.reason, enqueued_at = now();

-- name: ListReconcileBatch :many
SELECT cid FROM blob_replication_reconcile_queue
ORDER BY enqueued_at
LIMIT $1;

-- name: DeleteReconciled :exec
DELETE FROM blob_replication_reconcile_queue WHERE cid = $1;

-- name: ListUnderReplicatedByTier :many
-- Healing input: CIDs at a given safety tier, smallest target first is irrelevant;
-- ordered by updated_at so the oldest under-replication is addressed first.
SELECT cid, healthy_acked_count, target_count, durability_class, local_recoverable, dirty
FROM blob_replication_state
WHERE safety_tier = $1
ORDER BY updated_at
LIMIT $2;

-- name: RecomputeTargetsForClass :exec
-- On a replication.factor change (D-M5-2b): reset target_count for a class, mark
-- rows dirty so the drain recomputes safety_tier, and the scheduler re-evaluates.
UPDATE blob_replication_state
SET target_count = $2, dirty = true, updated_at = now()
WHERE durability_class = $1;

-- name: EnqueueReconcileByClass :exec
-- Companion to RecomputeTargetsForClass: enqueue the class's CIDs for bounded
-- recompute of their new safety_tier.
INSERT INTO blob_replication_reconcile_queue (cid, reason)
SELECT cid, 'target_change' FROM blob_replication_state WHERE durability_class = $1
ON CONFLICT (cid) DO UPDATE SET reason = EXCLUDED.reason, enqueued_at = now();
