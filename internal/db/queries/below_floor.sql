-- P2-M7.1 (D-M7.1-3): below-floor replacement sweep — the WRITE side of the
-- below_floor_since marker (0018). Marker maintenance is hysteretic (enter at
-- reputation < floor, exit only at >= floor + margin) and ALWAYS runs so the
-- nova_below_floor_nodes gauge stays truthful even with the remedy disabled.
-- The remedy (requeue + replace-then-demote) rides the EXISTING healing
-- machinery: enqueue the sustained node's CIDs for bounded recompute (the
-- Task 10 count exclusion drops them below target, so they surface as
-- tier1/tier2 and heal normally), then demote a sustained node's acked
-- assignment ONLY once enough trusted replacements exist — never a
-- durability dip. Mirrors the drain (D-M7-6) pattern: below-floor is the
-- drain treatment for DISTRUSTED nodes instead of politely-leaving ones.

-- name: MarkBelowFloorNodes :execrows
-- Entry edge: stamp the marker when a live node's reputation sinks below the
-- floor. Only live (active/suspect) nodes are marked — unreachable/evicted/
-- revoked nodes are already excluded from counts by status, and marking them
-- would start a grace clock nobody is watching. An existing marker is NEVER
-- re-stamped (below_floor_since IS NULL guard): wobble inside the band must
-- not reset the sustainment clock.
UPDATE nodes
SET below_floor_since = now()
WHERE reputation_score < sqlc.arg(floor)::float8
  AND below_floor_since IS NULL
  AND status IN ('active','suspect');

-- name: ClearBelowFloorNodes :execrows
-- Hysteresis exit edge: the marker clears only once the score recovers past
-- floor + hysteresis_margin (the caller passes the SUM as exit_threshold).
-- Wobble inside [floor, floor+margin) never clears; recovery is deliberate.
-- No status filter: a recovered score clears the marker regardless of
-- liveness (status governs countability independently).
UPDATE nodes
SET below_floor_since = NULL
WHERE below_floor_since IS NOT NULL
  AND reputation_score >= sqlc.arg(exit_threshold)::float8;

-- name: EnqueueBelowFloorReconcile :execrows
-- Bounded requeue (the D-M7.1-3 remedy, gated by below_floor_replacement.
-- enabled): CIDs held (acked) by SUSTAINED-below-floor nodes (marker older
-- than grace) enter the reconcile queue with reason 'below_floor', capped at
-- requeue_batch per tick ACROSS ALL sustained nodes. The dirtied CTE marks
-- the SAME CID set's projection rows dirty in the same atomic statement (the
-- two halves of the D-M5-2d bulk-transition contract, as in
-- EnqueueReconcileForNode). Re-enqueueing an already-queued CID bumps
-- enqueued_at, which sorts it BEHIND older entries in ListReconcileBatch —
-- below-floor churn can never starve liveness-driven reconciles. CIDs demoted
-- by DemoteBelowFloorReplicas leave the predicate (state <> 'acked'), so the
-- batch works through the backlog tick by tick.
WITH victims AS (
    SELECT DISTINCT pa.cid
    FROM pin_assignments pa
    JOIN nodes n ON n.id = pa.node_id
    WHERE pa.state = 'acked'
      AND n.below_floor_since IS NOT NULL
      AND n.below_floor_since <= now() - make_interval(secs => sqlc.arg(grace_secs)::float)
    ORDER BY pa.cid
    LIMIT sqlc.arg(requeue_batch)
), dirtied AS (
    UPDATE blob_replication_state brs
    SET dirty = true, updated_at = now()
    FROM victims v
    WHERE brs.cid = v.cid
)
INSERT INTO blob_replication_reconcile_queue (cid, reason)
SELECT v.cid, 'below_floor' FROM victims v
ON CONFLICT (cid) DO UPDATE SET reason = EXCLUDED.reason, enqueued_at = now();

-- name: DemoteBelowFloorReplicas :execrows
-- Replace-then-demote (gated by below_floor_replacement.enabled): fail a
-- SUSTAINED-below-floor node's acked assignment ONLY where the blob already
-- has target_count-many trusted holders WITHOUT it. The live-holder count is
-- computed from AUTHORITY (pin_assignments ⨝ nodes, the same countability
-- filter as RecomputeReplicationCounts) rather than the projection's
-- healthy_acked_count: the projection can read stale-HIGH in the window
-- between an audit hard-fail / liveness transition and the next reconcile
-- drain, and a demote decided on a stale-high count would BE the durability
-- dip this pass must never cause. Same safe-to-revoke reasoning — and the
-- same hash-aggregate shape — as CountDrainPendingCIDs (the correlated
-- per-pin probe measured ~23 s on a 500k-pin holder in the P2-M7 bench).
-- A SOLE holder never demotes: zero qualifying live holders yields no
-- aggregate row, the inner join drops the CID, and the below-floor copy
-- stays the repair source of last resort. Bounded by demote_batch per tick
-- (one bounded transaction per D-M5-2d); the remainder demotes on subsequent
-- ticks. pin_assignments has NO fail-reason column (liveness and audit
-- hard-fail both set bare state='failed'); the reason surface for demotions
-- is the sweep's structured log + result counter.
UPDATE pin_assignments
SET state = 'failed'
WHERE (cid, node_id) IN (
    SELECT pa.cid, pa.node_id
    FROM pin_assignments pa
    JOIN nodes bf ON bf.id = pa.node_id
    JOIN (
        SELECT pa2.cid, count(*) AS live_holders
        FROM pin_assignments pa2
        JOIN nodes n2 ON n2.id = pa2.node_id
        WHERE pa2.state = 'acked'
          AND n2.status IN ('active','suspect')
          AND n2.assignment_sync_state = 'current'
          AND n2.draining_at IS NULL
          AND (n2.below_floor_since IS NULL
               OR n2.below_floor_since > now() - make_interval(secs => sqlc.arg(grace_secs)::float))
          AND pa2.cid IN (SELECT pa3.cid
                          FROM pin_assignments pa3
                          JOIN nodes bf3 ON bf3.id = pa3.node_id
                          WHERE pa3.state = 'acked'
                            AND bf3.below_floor_since IS NOT NULL
                            AND bf3.below_floor_since <= now() - make_interval(secs => sqlc.arg(grace_secs)::float))
        GROUP BY pa2.cid
    ) h ON h.cid = pa.cid
    JOIN blob_replication_state brs ON brs.cid = pa.cid
    WHERE pa.state = 'acked'
      AND bf.below_floor_since IS NOT NULL
      AND bf.below_floor_since <= now() - make_interval(secs => sqlc.arg(grace_secs)::float)
      AND h.live_holders >= brs.target_count
    LIMIT sqlc.arg(demote_batch)
);

-- name: CountBelowFloorNodes :many
-- Scrape-time input for the nova_below_floor_nodes gauge: nodes carrying a
-- below-floor marker, split by whether the marker is older than the grace
-- window (sustained — excluded from safety counts and placement) or still
-- inside it (in_grace — still counts; hysteresis). Counts marked nodes
-- regardless of liveness status: the marker is reputation state, not
-- liveness. The 0018 partial index covers the IS NOT NULL scan.
SELECT (below_floor_since <= now() - make_interval(secs => sqlc.arg(grace_secs)::float)) AS sustained,
       count(*) AS n
FROM nodes
WHERE below_floor_since IS NOT NULL
GROUP BY 1;
