-- name: GetNodeByID :one
SELECT * FROM nodes WHERE id = $1;

-- name: RegisterNode :one
-- Registration (insert or re-register) always lands the node in
-- assignment_sync_state='snapshot_required' (D-M5-4a): a fresh node has no synced
-- desired set, and a re-registering one (e.g. a returning evicted node) must
-- (re)sync from the current epoch before its assignments count again. Re-register
-- also reactivates (status='active') and refreshes last_seen_at so the liveness
-- sweeper does not immediately re-evict a node that just contacted us. The
-- handler rejects revoked nodes before calling this, so the reactivation is safe.
INSERT INTO nodes (
    id, nebula_cert_fingerprint, federation_cert_fingerprint, display_name,
    geo_declared, capacity_bytes, bandwidth_budget_bytes_per_day, policy_filters,
    status, trust_state, selected_protocol, advertised_capabilities,
    required_capabilities, client_version, source_nebula_addr, assignment_sync_state
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8,
    'active', 'probationary', $9, $10, $11, $12, $13, 'snapshot_required'
)
ON CONFLICT (id) DO UPDATE SET
    nebula_cert_fingerprint        = EXCLUDED.nebula_cert_fingerprint,
    display_name                   = EXCLUDED.display_name,
    geo_declared                   = EXCLUDED.geo_declared,
    capacity_bytes                 = EXCLUDED.capacity_bytes,
    bandwidth_budget_bytes_per_day = EXCLUDED.bandwidth_budget_bytes_per_day,
    policy_filters                 = EXCLUDED.policy_filters,
    selected_protocol              = EXCLUDED.selected_protocol,
    advertised_capabilities        = EXCLUDED.advertised_capabilities,
    required_capabilities          = EXCLUDED.required_capabilities,
    client_version                 = EXCLUDED.client_version,
    source_nebula_addr             = EXCLUDED.source_nebula_addr,
    status                         = 'active',
    assignment_sync_state          = 'snapshot_required',
    last_seen_at                   = now(),
    last_status_change_at          = now()
RETURNING *;

-- name: UpdateNodeHeartbeat :one
-- Heartbeat is the canonical liveness path (D-M5-4a-LIVENESS-SIGNAL): besides
-- refreshing telemetry it reactivates a suspect/unreachable node. A node returning
-- from unreachable had pending divergence, so it re-enters 'reconciling' and does
-- NOT count toward durability until it resyncs to the current epoch (D-M5-2a).
-- All SET right-hand sides reference the pre-update row, so the CASE expressions
-- read the OLD status. The handler rejects evicted/revoked before calling this, so
-- only active/suspect/unreachable reach here.
UPDATE nodes
SET last_seen_at      = now(),
    last_free_bytes   = $2,
    last_stored_bytes = $3,
    source_nebula_addr = COALESCE(NULLIF(sqlc.arg(source_nebula_addr)::text, ''), nodes.source_nebula_addr),
    -- M5 egress telemetry (D-M5-6-TEL): only a telemetry-capable donor reports a
    -- positive capacity; gate on it so a non-reporting donor leaves the columns
    -- untouched (NULL until first real report), while a reporting donor may carry a
    -- meaningful remaining of 0.
    last_egress_remaining_bytes = CASE WHEN sqlc.arg(egress_capacity)::bigint > 0 THEN sqlc.arg(egress_remaining)::bigint ELSE last_egress_remaining_bytes END,
    last_egress_capacity_bytes  = CASE WHEN sqlc.arg(egress_capacity)::bigint > 0 THEN sqlc.arg(egress_capacity)::bigint ELSE last_egress_capacity_bytes END,
    last_egress_refill_bps      = CASE WHEN sqlc.arg(egress_capacity)::bigint > 0 THEN sqlc.arg(egress_refill)::bigint ELSE last_egress_refill_bps END,
    status = CASE WHEN status IN ('suspect','unreachable') THEN 'active'::node_status ELSE status END,
    assignment_sync_state = CASE WHEN status = 'unreachable' THEN 'reconciling' ELSE assignment_sync_state END,
    last_status_change_at = CASE WHEN status IN ('suspect','unreachable') THEN now() ELSE last_status_change_at END
WHERE id = $1
RETURNING *;

-- name: RevokeNode :execrows
UPDATE nodes
SET status = 'revoked', cert_revoked_at = now(), last_status_change_at = now()
WHERE id = $1 AND status <> 'revoked';

-- name: RotateNodeCert :execrows
UPDATE nodes
SET federation_cert_fingerprint = $2,
    cert_rotation_started_at = now(),
    cert_rotated_at = now()
WHERE id = $1;

-- name: ListNodes :many
SELECT id, display_name, status, trust_state, selected_protocol, last_seen_at
FROM nodes
ORDER BY joined_at DESC;

-- name: GetChangeLogHead :one
-- Monotonic change-log head: the greatest sequence ever issued that a donor
-- cursor should reach = max(retained max sequence, highest pruned sequence). Using
-- the prune watermark keeps the head from regressing to 0 when the log is fully
-- pruned (which would otherwise loop a recovered donor back into snapshot_required).
SELECT GREATEST(
    (SELECT COALESCE(MAX(sequence), 0) FROM pin_changes),
    (SELECT pruned_through_seq FROM federation_change_log_state WHERE id = true)
)::bigint AS head;

-- name: GetPruneWatermark :one
SELECT pruned_through_seq FROM federation_change_log_state WHERE id = true;

-- name: AcquireChangeLogLock :exec
-- Transaction-scoped advisory lock that serializes change-log appends so
-- pin_changes.sequence values commit in assignment order (commit-order-safe):
-- a donor never advances its cursor past a lower-sequence row that can still
-- commit. Gaps from rolled-back txns are harmless (cursor uses sequence > N).
SELECT pg_advisory_xact_lock(8030600000000000001);

-- name: GetBlobSize :one
-- Feeds AssignPin → pin_changes.byte_size. Selects blob_manifests.envelope_size
-- (the actual on-disk ciphertext size) so the change-log byte_size always
-- reflects what the donor will receive. No state filter, as before: unknown CID
-- → no row → AssignPin rolls back (correct; every committed blob has a manifest).
SELECT m.envelope_size FROM blob_manifests m WHERE m.cid = $1;

-- name: GetEnvelopeSize :one
-- Source preflight + read tier: the on-disk ciphertext envelope size for
-- max_bytes enforcement before streaming (D-M4-3). Preserves the active-state
-- filter from the old GetBlobByteSize — quarantined / tombstoned / soft_deleted
-- blobs MUST NOT be served to donors (no row → 404 blob_unavailable).
SELECT m.envelope_size
FROM blob_manifests m JOIN blobs b ON b.cid = m.cid
WHERE m.cid = $1 AND b.state = 'active';

-- name: UpsertPinAssignmentAssign :one
INSERT INTO pin_assignments (cid, node_id, state, generation)
VALUES ($1, $2, 'pending', 1)
ON CONFLICT (cid, node_id) DO UPDATE SET
    state       = 'pending',
    generation  = pin_assignments.generation + 1,
    acked_at    = NULL,
    assigned_at = now()
RETURNING assignment_id, generation;

-- name: GetPinAssignmentForUpdate :one
SELECT assignment_id, generation FROM pin_assignments
WHERE cid = $1 AND node_id = $2
FOR UPDATE;

-- name: DeletePinAssignment :execrows
DELETE FROM pin_assignments WHERE cid = $1 AND node_id = $2;

-- name: InsertPinChange :one
INSERT INTO pin_changes (node_id, assignment_id, generation, kind, cid, byte_size)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING sequence;

-- name: GetPinChangesSince :many
-- source_node_id (M5, D-M5-8a) is the durable repair source copied in at assign
-- time: NULL ⇒ coordinator-as-source, a node id ⇒ donor-as-source (late-bound to
-- the source's current address at serve time).
SELECT sequence, assignment_id, generation, kind, cid, byte_size, source_node_id
FROM pin_changes
WHERE node_id = $1 AND sequence > $2
ORDER BY sequence
LIMIT $3;

-- name: NodeHasChangesAfter :one
SELECT EXISTS (
    SELECT 1 FROM pin_changes WHERE node_id = $1 AND sequence > $2
) AS changed;

-- name: GetPinSnapshotPage :many
-- Donor desired-assignment snapshot page: selects envelope_size (the actual
-- on-disk ciphertext size) aliased as byte_size for wire compatibility.
-- source_node_id (M5, D-M5-8a) lets snapshot recovery reconstruct the repair
-- source after the change log that carried it has been pruned.
SELECT pa.cid, pa.assignment_id, pa.generation, m.envelope_size AS byte_size, pa.assigned_at, pa.source_node_id
FROM pin_assignments pa
JOIN blob_manifests m ON m.cid = pa.cid
WHERE pa.node_id = $1 AND pa.cid > $2
ORDER BY pa.cid
LIMIT $3;

-- name: AckPinAssignment :execrows
UPDATE pin_assignments
SET state = 'acked', acked_at = now()
WHERE cid = $1 AND node_id = $2 AND assignment_id = $3 AND generation = $4 AND state = 'pending';

-- name: FailPinAssignment :execrows
UPDATE pin_assignments
SET state = 'failed'
WHERE cid = $1 AND node_id = $2 AND assignment_id = $3 AND generation = $4 AND state = 'pending';

-- name: GetPinAssignment :one
SELECT cid, node_id, state, assignment_id, generation, assigned_at, acked_at
FROM pin_assignments
WHERE cid = $1 AND node_id = $2;

-- name: PruneChangeLog :one
-- Atomic delete + watermark advance in one statement (no tx needed).
WITH del AS (
    DELETE FROM pin_changes WHERE created_at < $1 RETURNING sequence
)
UPDATE federation_change_log_state
SET pruned_through_seq = GREATEST(
    pruned_through_seq,
    COALESCE((SELECT MAX(sequence) FROM del), pruned_through_seq)
)
WHERE id = true
RETURNING pruned_through_seq;

-- name: ListDesiredAssignmentsByCID :many
SELECT node_id, generation, state FROM pin_assignments WHERE cid = $1 ORDER BY node_id;

-- name: ListVerifiedHoldersByCID :many
SELECT node_id, generation FROM pin_assignments WHERE cid = $1 AND state = 'acked' ORDER BY node_id;

-- name: ListDesiredAssignmentsByNode :many
SELECT cid, generation, state FROM pin_assignments WHERE node_id = $1 ORDER BY cid;

-- name: ListAdmissionCandidates :many
-- Admission targets: live, non-suspended, read-source-capable, addressed nodes,
-- preferring those with known free space >= the blob's envelope size (unknown
-- free space is treated as OK; the donor's own storage_max_bytes is the real
-- safety gate). Ordered for best-link selection: free-OK first, then reputation.
SELECT n.id AS node_id, n.reputation_score, n.last_free_bytes
FROM nodes n
WHERE n.status IN ('active','suspect')
  AND n.trust_state <> 'suspended'
  AND n.advertised_capabilities @> ARRAY['read-source/v1']
  AND n.source_nebula_addr IS NOT NULL AND n.source_nebula_addr <> ''
ORDER BY (n.last_free_bytes IS NULL OR n.last_free_bytes >= sqlc.arg(min_free_bytes)) DESC,
         n.reputation_score DESC, n.id
LIMIT sqlc.arg(lim);

-- name: SelectLivenessTransitions :many
-- One sweep's pending status changes (P2-M5 D-M5-4): each silent node mapped to
-- the most severe liveness state its silence warrants, computed from its last
-- evidence of life (last_seen_at, or joined_at if it never heartbeated). Only
-- non-terminal states are considered; the WHERE bounds the result to nodes past
-- at least the suspect threshold. The Go sweeper applies only strict advancements
-- (reactivation is the heartbeat handler's job).
SELECT id, status,
  (CASE
    WHEN COALESCE(last_seen_at, joined_at) < now() - sqlc.arg(evicted_secs)::int     * interval '1 second' THEN 'evicted'
    WHEN COALESCE(last_seen_at, joined_at) < now() - sqlc.arg(unreachable_secs)::int * interval '1 second' THEN 'unreachable'
    WHEN COALESCE(last_seen_at, joined_at) < now() - sqlc.arg(suspect_secs)::int     * interval '1 second' THEN 'suspect'
    ELSE status
  END)::node_status AS target_status
FROM nodes
WHERE status IN ('active','suspect','unreachable')
  AND COALESCE(last_seen_at, joined_at) < now() - sqlc.arg(suspect_secs)::int * interval '1 second';

-- name: SetNodeStatus :exec
UPDATE nodes SET status = $2, last_status_change_at = now() WHERE id = $1;

-- name: SetNodeSyncState :exec
UPDATE nodes SET assignment_sync_state = $2 WHERE id = $1;

-- name: FailNodePendingAssignments :execrows
-- A liveness transition to unreachable/evicted fails the node's still-pending
-- reservations so a dead in-flight reservation never blocks re-scheduling.
UPDATE pin_assignments SET state = 'failed'
WHERE node_id = $1 AND state = 'pending';

-- name: DeleteNodeAssignments :execrows
-- Eviction retires the node's desired set (D-M5-4a-EVICT): pin_state has no
-- 'retired', so the rows are deleted (after the affected CIDs were enqueued).
DELETE FROM pin_assignments WHERE node_id = $1;

-- name: MarkReplicationDirtyForNode :exec
-- Bulk projection enqueue half (D-M5-2d): mark every projection row for a CID the
-- node holds dirty, so the scheduler recomputes it from authority before reserving.
UPDATE blob_replication_state SET dirty = true, updated_at = now()
WHERE cid IN (SELECT cid FROM pin_assignments WHERE node_id = $1);

-- name: EnqueueReconcileForNode :exec
-- Durable enqueue half (D-M5-2d): queue every CID the node holds an assignment for,
-- for bounded async recompute after a transition drops the node's countability.
INSERT INTO blob_replication_reconcile_queue (cid, reason)
SELECT DISTINCT cid, sqlc.arg(reason) FROM pin_assignments WHERE node_id = sqlc.arg(node_id)
ON CONFLICT (cid) DO UPDATE SET reason = EXCLUDED.reason, enqueued_at = now();

-- name: SelectUnsignaledRevoked :many
-- node_revoked observation path (D-M5-4-REVOKE-OBS): novactl revoke is DB-direct,
-- so the sweeper detects revoked-but-unsignaled nodes to emit the event once.
SELECT id FROM nodes WHERE status = 'revoked' AND revoked_signaled_at IS NULL;

-- name: MarkRevokedSignaled :exec
UPDATE nodes SET revoked_signaled_at = now() WHERE id = $1;

-- name: UpsertPinAssignmentAssignWithSource :one
-- M5 reservation primitive (D-M5-8a): like UpsertPinAssignmentAssign but binds a
-- durable repair source. source_node_id is NULL for coordinator-as-source (never
-- the synthetic CoordinatorSourceID — that lives only on the wire). A fresh assign
-- resets the late-bind backoff counters.
INSERT INTO pin_assignments (cid, node_id, state, generation, source_node_id)
VALUES ($1, $2, 'pending', 1, $3)
ON CONFLICT (cid, node_id) DO UPDATE SET
    state                  = 'pending',
    generation             = pin_assignments.generation + 1,
    acked_at               = NULL,
    assigned_at            = now(),
    source_node_id         = EXCLUDED.source_node_id,
    source_attempts        = 0,
    source_next_attempt_at = NULL
RETURNING assignment_id, generation;

-- name: InsertPinChangeWithSource :one
-- Copies the repair source into the change log for incremental delivery + audit.
INSERT INTO pin_changes (node_id, assignment_id, generation, kind, cid, byte_size, source_node_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING sequence;

-- name: IsRepairSourceableForCID :one
-- Reservation-time guard: the chosen source must currently be repair-sourceable
-- (live, current-synced, advertises repair-stream/v1, addressed) AND hold an acked
-- copy of this CID (D-M5-8a/8c). A non-advertiser is read-sourceable but never
-- repair-sourceable (mixed-version safety).
SELECT EXISTS (
  SELECT 1 FROM nodes n
  JOIN pin_assignments pa ON pa.node_id = n.id
  WHERE n.id = $1 AND pa.cid = $2 AND pa.state = 'acked'
    AND n.status IN ('active','suspect')
    AND n.assignment_sync_state = 'current'
    AND n.advertised_capabilities @> ARRAY['repair-stream/v1']
    AND n.source_nebula_addr IS NOT NULL AND n.source_nebula_addr <> ''
) AS ok;

-- name: GetRepairSource :one
-- Late-mint resolution: the source's CURRENT address + its acked assignment for
-- this CID, only while it is still repair-sourceable. No row ⇒ the stored source
-- is no longer usable ⇒ the caller requeues (D-M5-8a).
SELECT n.source_nebula_addr, pa.assignment_id, pa.generation
FROM nodes n
JOIN pin_assignments pa ON pa.node_id = n.id
WHERE n.id = $1 AND pa.cid = $2 AND pa.state = 'acked'
  AND n.status IN ('active','suspect')
  AND n.assignment_sync_state = 'current'
  AND n.advertised_capabilities @> ARRAY['repair-stream/v1']
  AND n.source_nebula_addr IS NOT NULL AND n.source_nebula_addr <> '';

-- name: RequeuePinAssignmentSource :exec
-- The stored source is no longer repair-sourceable: clear it (back to NULL /
-- coordinator-unbound), bump the attempt counter, and back off exponentially
-- (30s × 2^attempts, capped at 1h) so a flapping source does not tight-loop; the
-- Task 6 scheduler re-picks once the backoff elapses (D-M5-8a).
UPDATE pin_assignments
SET source_node_id         = NULL,
    source_attempts        = source_attempts + 1,
    source_next_attempt_at = now() + make_interval(secs => LEAST(3600, 30 * power(2, LEAST(source_attempts, 7))::int))
WHERE cid = $1 AND node_id = $2;
