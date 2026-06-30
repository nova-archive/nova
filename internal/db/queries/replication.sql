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

-- name: GetReplicationState :one
-- The scheduler reads the fresh projection row (after RecomputeCID) to decide
-- whether a CID still needs healing and how (D-M5-6).
SELECT healthy_acked_count, target_count, safety_tier, durability_class, local_recoverable
FROM blob_replication_state WHERE cid = $1;

-- name: ListRepairSourceHolders :one
-- Asymmetric repair-source selection (D-M5-6): the single best repair-sourceable
-- acked holder of this CID, weighted by step_capacity × reputation. step_capacity
-- is the reported egress remaining (a telemetry-less donor scores 0 on the weight
-- but is still eligible, sorted last). A donor with KNOWN remaining below the blob
-- size is excluded (byte-infeasible); unknown (NULL) remaining is not excluded.
SELECT n.id AS node_id, n.reputation_score,
       COALESCE(n.last_egress_remaining_bytes, 0) AS remaining
FROM nodes n
JOIN pin_assignments pa ON pa.node_id = n.id
WHERE pa.cid = $1 AND pa.state = 'acked'
  AND n.status IN ('active','suspect')
  AND n.assignment_sync_state = 'current'
  AND n.advertised_capabilities @> ARRAY['repair-stream/v1']
  AND n.source_nebula_addr IS NOT NULL AND n.source_nebula_addr <> ''
  AND (n.last_egress_remaining_bytes IS NULL OR n.last_egress_remaining_bytes >= sqlc.arg(size))
ORDER BY (COALESCE(n.last_egress_remaining_bytes, 0)::float8 * n.reputation_score) DESC,
         n.reputation_score DESC, n.id
LIMIT 1;

-- name: ListPlacementCandidates :many
-- Eligible repair DESTINATIONS for a CID: live, current-synced, non-suspended
-- NON-holders (a node already assigned the CID, pending or acked, is excluded).
-- The placement engine (Task 4) applies anti-affinity, trust caps, capacity and
-- reputation floor over these.
SELECT n.id AS node_id, n.failure_domain_id, n.donor_principal_id, n.provider, n.asn, n.region,
       (n.operator_verified_at IS NOT NULL)::boolean AS operator_verified,
       COALESCE(n.last_free_bytes, 0) AS free_bytes,
       n.trust_state, n.reputation_score, n.placement_weight
FROM nodes n
WHERE n.status = 'active'
  AND n.assignment_sync_state = 'current'
  AND n.trust_state <> 'suspended'
  AND NOT EXISTS (SELECT 1 FROM pin_assignments pa WHERE pa.cid = $1 AND pa.node_id = n.id)
ORDER BY n.id;

-- name: ListCIDHolders :many
-- Existing acked holders' verified placement dimensions, for the engine's
-- anti-affinity comparison.
SELECT n.failure_domain_id, n.donor_principal_id, n.provider, n.asn, n.region,
       (n.operator_verified_at IS NOT NULL)::boolean AS operator_verified
FROM nodes n
JOIN pin_assignments pa ON pa.node_id = n.id
WHERE pa.cid = $1 AND pa.state = 'acked';

-- name: CIDHasPendingInBackoff :one
-- The scheduler skips a CID that already has a pending reservation in source-retry
-- backoff, so a flapping source is not re-picked before its backoff elapses
-- (Rev. 5 #3).
SELECT EXISTS (
  SELECT 1 FROM pin_assignments
  WHERE cid = $1 AND state = 'pending'
    AND source_next_attempt_at IS NOT NULL AND source_next_attempt_at > now()
) AS in_backoff;
