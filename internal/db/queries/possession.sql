-- name: InsertAuditChallenge :exec
-- D-M6-3b: insert BEFORE dispatch with result NULL so a crash mid-flight is recoverable.
INSERT INTO pin_audits (id, blob_cid, node_id, challenge_kind, nonce, deadline, challenged_at)
VALUES ($1, $2, $3, $4, $5, $6, now());

-- name: RevalidateAuditPin :one
-- D-M6-7a: confirm the audited assignment is still the live acked one before scoring.
SELECT EXISTS (
  SELECT 1 FROM pin_assignments
  WHERE cid = $1 AND node_id = $2 AND state = 'acked'
    AND assignment_id = $3 AND generation = $4
) AS still_current;

-- name: FailAckedPinAssignmentForAudit :execrows
-- D-M6-7 hard-fail: invalidate the ACKED row for this exact assignment/generation.
-- (M5's FailPinAssignment only fails 'pending' rows — wrong state for audits.)
UPDATE pin_assignments
SET state = 'failed'
WHERE cid = $1 AND node_id = $2 AND assignment_id = $3 AND generation = $4 AND state = 'acked';

-- name: GetNodeTrustForUpdate :one
-- Row-locked read for the reputation/trust transaction (Blocker 5: lost-update safe).
SELECT trust_state, reputation_score, trust_epoch_started_at, trust_review_required_at, joined_at
FROM nodes WHERE id = $1 FOR UPDATE;

-- name: SelectLastAuditPerNode :many
-- Startup cadence seed (D-M6-5): last resolved audit per node, so a restart does not
-- immediately re-audit every due node.
SELECT node_id, max(decided_at)::timestamptz AS last_decided_at
FROM pin_audits WHERE result IS NOT NULL
GROUP BY node_id;

-- name: RecordAuditOutcome :execrows
-- Resolve only the unresolved row, so a replayed challenge_id cannot overwrite a
-- decided audit (the caller asserts 1 row; 0 = replay/already-decided).
UPDATE pin_audits
SET result = $2, received_at = $3, decided_at = $4, latency_ms = $5,
    bytes_verified = $6, transcript_hash = $7, error = $8
WHERE id = $1 AND result IS NULL;

-- name: MoveReputation :one
-- Atomic, lost-update-safe (D-M6-7): clamp to [0,1].
UPDATE nodes
SET reputation_score = LEAST(1.0, GREATEST(0.0, $2::real))
WHERE id = $1
RETURNING reputation_score;

-- name: GetNodeTrust :one
SELECT trust_state, reputation_score, trust_epoch_started_at, trust_review_required_at, joined_at
FROM nodes WHERE id = $1;

-- name: CountPassedAuditsSince :one
SELECT count(*) FROM pin_audits
WHERE node_id = $1 AND result = 'pass' AND decided_at >= $2;

-- name: CountAckedTransfersSince :one
SELECT count(*) FROM pin_assignments
WHERE node_id = $1 AND state = 'acked' AND acked_at >= $2;

-- name: SetTrustState :exec
UPDATE nodes SET trust_state = $2 WHERE id = $1;

-- name: SetTrustReview :exec
-- Hash-mismatch path: reset epoch + mark for operator review (D-M6-2b).
UPDATE nodes
SET trust_epoch_started_at = now(), trust_review_required_at = now(), trust_review_reason = $2
WHERE id = $1;

-- name: ClearTrustReview :exec
-- Operator clear-review: restart the epoch, drop the marker (D-M6-8).
UPDATE nodes
SET trust_review_required_at = NULL, trust_review_reason = NULL, trust_epoch_started_at = now()
WHERE id = $1;

-- name: ReconcileStaleAudits :exec
-- Startup: crashed-mid-flight attempts -> skip (D-M6-3b step 4).
UPDATE pin_audits
SET result = 'skip', decided_at = now(), error = 'coordinator_crash_or_timeout'
WHERE result IS NULL AND deadline < now() - make_interval(secs => $1::float);

-- name: SelectDueAuditNodes :many
-- Stage 1 (D-M6-5a): live, current-synced nodes with at least one acked pin, ordered
-- by node-level pressure (stored bytes proxy + acked pin count) and audit staleness.
SELECT n.id AS node_id, n.trust_state, n.reputation_score,
       COALESCE(n.last_stored_bytes, 0) AS stored_bytes,
       count(pa.cid) AS acked_pins
FROM nodes n
JOIN pin_assignments pa ON pa.node_id = n.id AND pa.state = 'acked'
WHERE n.status IN ('active','suspect') AND n.assignment_sync_state = 'current'
  AND n.advertised_capabilities @> ARRAY['audit-block-hash/v1']  -- only challengeable donors
  AND n.source_nebula_addr IS NOT NULL AND n.source_nebula_addr <> ''
GROUP BY n.id
ORDER BY (COALESCE(n.last_stored_bytes,0)::float8 * count(pa.cid)) DESC, n.id
LIMIT $1;

-- name: SelectAckedPinForAudit :one
-- Stage 2: one acked pin for a node, WEIGHTED-random by envelope_size
-- (Efraimidis–Spirakis: key = random()^(1/weight), largest key wins) so big
-- custodians are sampled proportionally without ORDER BY random() over the corpus.
SELECT pa.cid, pa.assignment_id, pa.generation
FROM pin_assignments pa
JOIN blob_manifests m ON m.cid = pa.cid
WHERE pa.node_id = $1 AND pa.state = 'acked'
ORDER BY power(random(), 1.0 / GREATEST(m.envelope_size, 1)) DESC
LIMIT 1;

-- name: SelectNewlyAckedPins :many
-- Fast lane (D-M6-5b): pins acked within the window, not yet audited, bounded quota.
SELECT pa.node_id, pa.cid, pa.assignment_id, pa.generation
FROM pin_assignments pa
WHERE pa.state = 'acked' AND pa.acked_at >= $1
  AND NOT EXISTS (SELECT 1 FROM pin_audits a WHERE a.blob_cid = pa.cid AND a.node_id = pa.node_id)
ORDER BY pa.acked_at
LIMIT $2;

-- name: SelectRandomBlockForCID :one
-- Stage 3: a block to challenge (size <= max_block_bytes; never issue an over-cap block).
SELECT block_cid, block_index, block_size
FROM blob_blocks
WHERE blob_cid = $1 AND block_size <= $2
ORDER BY random()
LIMIT 1;

-- name: GetNodeSourceAddr :one
-- The donor's inbound source address to POST the challenge to.
SELECT source_nebula_addr FROM nodes WHERE id = $1;
