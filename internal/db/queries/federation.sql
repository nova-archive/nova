-- name: GetNodeByID :one
SELECT * FROM nodes WHERE id = $1;

-- name: RegisterNode :one
INSERT INTO nodes (
    id, nebula_cert_fingerprint, federation_cert_fingerprint, display_name,
    geo_declared, capacity_bytes, bandwidth_budget_bytes_per_day, policy_filters,
    status, trust_state, selected_protocol, advertised_capabilities,
    required_capabilities, client_version
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8,
    'active', 'probationary', $9, $10, $11, $12
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
    client_version                 = EXCLUDED.client_version
RETURNING *;

-- name: UpdateNodeHeartbeat :one
UPDATE nodes
SET last_seen_at = now(), last_free_bytes = $2, last_stored_bytes = $3
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
SELECT sequence, assignment_id, generation, kind, cid, byte_size
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
SELECT pa.cid, pa.assignment_id, pa.generation, m.envelope_size AS byte_size, pa.assigned_at
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
