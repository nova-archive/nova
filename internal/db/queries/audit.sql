-- Integrity-audit queries (M8). See docs/specs/INTEGRITY_AUDIT.md and
-- docs/superpowers/specs/2026-06-02-phase1-m8-integrity-audit-scheduler-design.md.
-- No schema change: integrity_audits + the audit_kind/audit_result enums ship
-- in 0001_init.sql (partitioned in 0003_partitions.sql).

-- name: SampleEncryptedBlobs :many
SELECT b.cid, b.byte_size
FROM blobs b
WHERE b.state = 'active' AND b.encryption_key_id IS NOT NULL
ORDER BY random()
LIMIT $1;

-- name: SampleActiveBlobs :many
SELECT cid
FROM blobs
WHERE state = 'active'
ORDER BY random()
LIMIT $1;

-- name: SampleDerivatives :many
SELECT d.cid, d.state::text AS state, p.state::text AS parent_state
FROM blobs d
JOIN blobs p ON p.cid = d.parent_cid
WHERE d.parent_cid IS NOT NULL
ORDER BY random()
LIMIT $1;

-- name: SampleMultiBlockBlocks :many
SELECT bb.block_cid, bb.blob_cid
FROM blob_blocks bb
JOIN blob_manifests m ON m.cid = bb.blob_cid
WHERE m.block_count > 1
ORDER BY random()
LIMIT $1;

-- name: SampleManifestConsistency :many
SELECT m.cid,
       m.block_count,
       m.envelope_size,
       count(bb.block_index)                   AS actual_count,
       COALESCE(sum(bb.block_size), 0)::bigint  AS actual_size
FROM blob_manifests m
LEFT JOIN blob_blocks bb ON bb.blob_cid = m.cid
WHERE m.cid IN (
    SELECT cid FROM blobs WHERE state = 'active' ORDER BY random() LIMIT $1
)
GROUP BY m.cid, m.block_count, m.envelope_size;

-- name: InsertIntegrityAudit :batchexec
INSERT INTO integrity_audits (cid, audit_kind, result, error)
VALUES ($1, $2, $3, $4);

-- name: ListIntegrityAudits :many
SELECT id, cid, audit_kind::text AS audit_kind, result::text AS result, error, audited_at
FROM integrity_audits
WHERE (sqlc.narg('result')::audit_result IS NULL OR result = sqlc.narg('result')::audit_result)
  AND (sqlc.narg('audit_kind')::audit_kind IS NULL OR audit_kind = sqlc.narg('audit_kind')::audit_kind)
ORDER BY audited_at DESC, id DESC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountIntegrityAudits :one
SELECT count(*)
FROM integrity_audits
WHERE (sqlc.narg('result')::audit_result IS NULL OR result = sqlc.narg('result')::audit_result)
  AND (sqlc.narg('audit_kind')::audit_kind IS NULL OR audit_kind = sqlc.narg('audit_kind')::audit_kind);

-- name: SeedAuditSchedule :many
SELECT audit_kind::text AS audit_kind, max(audited_at)::timestamptz AS last_run
FROM integrity_audits
GROUP BY audit_kind;

-- Audit-log write + query queries (M9). See docs/specs/DATA_MODEL.sql.
-- The audit_log table is monthly-partitioned (0003_partitions.sql).

-- name: InsertAuditLog :exec
INSERT INTO audit_log (actor_id, action, target_type, target_id, payload)
VALUES (sqlc.narg('actor_id'), sqlc.arg('action'), sqlc.arg('target_type'), sqlc.arg('target_id'), sqlc.arg('payload'));

-- name: ListAuditLog :many
SELECT id, actor_id::text AS actor_id, action, target_type, target_id, payload, at
FROM audit_log
WHERE (sqlc.narg('action')::text IS NULL OR action = sqlc.narg('action')::text)
  AND (sqlc.narg('target_type')::text IS NULL OR target_type = sqlc.narg('target_type')::text)
ORDER BY at DESC, id DESC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountAuditLog :one
SELECT count(*) FROM audit_log
WHERE (sqlc.narg('action')::text IS NULL OR action = sqlc.narg('action')::text)
  AND (sqlc.narg('target_type')::text IS NULL OR target_type = sqlc.narg('target_type')::text);
