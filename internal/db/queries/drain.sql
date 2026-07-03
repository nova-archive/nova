-- P2-M7 (D-M7-6): voluntary graceful drain — marker + debt queries. Drain debt
-- deliberately queries pin_assignments ⨝ nodes directly; it does NOT overload
-- blob_replication_state.sourceable_acked_count (that is a safety count).

-- name: SetNodeDraining :execrows
-- One-shot; a second drain is a no-op here (idempotency: timestamp preserved).
UPDATE nodes SET draining_at = now()
WHERE id = $1 AND draining_at IS NULL;

-- name: ClearNodeDraining :execrows
UPDATE nodes SET draining_at = NULL
WHERE id = $1 AND draining_at IS NOT NULL;

-- name: GetNodeDrainState :one
SELECT status, assignment_sync_state, draining_at FROM nodes WHERE id = $1;

-- name: ListDrainingNodes :many
SELECT id, draining_at FROM nodes WHERE draining_at IS NOT NULL ORDER BY id;

-- name: CountDrainPendingCIDs :one
-- Drain debt (D-M7-6f): CIDs acked on the draining node whose count of acked,
-- live, sync-current, NON-draining holders is below target_count. Pending
-- reservations are NOT safe and do not reduce debt.
SELECT count(*) FROM (
    SELECT pa.cid
    FROM pin_assignments pa
    JOIN blob_replication_state brs ON brs.cid = pa.cid
    WHERE pa.node_id = $1 AND pa.state = 'acked'
      AND (SELECT count(*)
           FROM pin_assignments pa2
           JOIN nodes n2 ON n2.id = pa2.node_id
           WHERE pa2.cid = pa.cid AND pa2.state = 'acked'
             AND n2.status IN ('active','suspect')
             AND n2.assignment_sync_state = 'current'
             AND n2.draining_at IS NULL) < brs.target_count
) debt;

-- name: CountDrainInflightCIDs :one
-- Replacement in progress but not acked: lets an operator distinguish "stuck"
-- from "working" (D-M7-6f). Counts pending replacements ONLY on eligible
-- destinations (non-draining, live/current, non-suspended, not the draining
-- node itself) — a stale/dead pending row must not read as drain progress
-- (the same reason liveness fails dead pendings). Pending still never reduces
-- safe-to-revoke debt; this is "work in flight" only.
SELECT count(DISTINCT pa.cid)
FROM pin_assignments pa
WHERE pa.node_id = $1 AND pa.state = 'acked'
  AND EXISTS (
      SELECT 1
      FROM pin_assignments p3
      JOIN nodes n3 ON n3.id = p3.node_id
      WHERE p3.cid = pa.cid
        AND p3.state = 'pending'
        AND p3.node_id <> $1
        AND n3.status IN ('active','suspect')
        AND n3.assignment_sync_state = 'current'
        AND n3.trust_state <> 'suspended'
        AND n3.draining_at IS NULL
  );

-- name: CountBelowFloorReplicas :many
-- Below-floor replica debt (D-M7-1a): acked replicas on live nodes below the
-- reputation floor whose pins have not hard-failed. Mirrors healthy_acked
-- COUNTABILITY (active/suspect + sync-current) — deliberately NO trust_state
-- filter, because healthy_acked_count does not exclude suspended either; this
-- metric is "still countable despite below-floor reputation", not read-source
-- eligibility. Observability only — the automated remedy is P2-M6.1, NOT M7.
SELECT n.id AS node_id, count(*) AS acked_replicas
FROM pin_assignments pa
JOIN nodes n ON n.id = pa.node_id
WHERE pa.state = 'acked'
  AND n.status IN ('active','suspect')
  AND n.assignment_sync_state = 'current'
  AND n.reputation_score < sqlc.arg(floor)::float8
GROUP BY n.id
ORDER BY n.id;
