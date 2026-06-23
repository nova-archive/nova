-- name: GetBlobCore :one
SELECT
    cid,
    state::text                       AS state,
    mime_type,
    envelope_version,
    (encryption_key_id IS NOT NULL)::boolean   AS encrypted,
    COALESCE(owner_id::text, '')::text         AS owner_id,
    uploaded_at,
    product::text                     AS product
FROM blobs
WHERE cid = $1;

-- name: GetDEKByBlob :one
SELECT
    k.wrapped_key,
    k.state::text                     AS state,
    k.master_key_version_id::text     AS master_key_version_id
FROM blobs b
JOIN data_encryption_keys k ON k.id = b.encryption_key_id
WHERE b.cid = $1;

-- name: GetManifestSize :one
SELECT plaintext_size
FROM blob_manifests
WHERE cid = $1;

-- name: GetBlobMeta :one
-- State-agnostic metadata read for the owner/admin detail view (M11). Unlike
-- GetBlobCore (the public read path, which Resolve rejects on non-active states),
-- this returns the row in ANY state so an operator/owner can inspect a
-- soft_deleted / quarantined / tombstoned blob. owner_id coalesced to '' (the
-- GetBlobForModeration precedent) so a NULL owner never crashes the scan.
SELECT
    cid,
    coalesce(owner_id::text, '')::text AS owner_id,
    parent_cid,
    derivative_preset,
    derivative_format,
    mime_type,
    byte_size,
    uploaded_at,
    soft_deleted_at,
    state::text                       AS state,
    product::text                     AS product
FROM blobs
WHERE cid = $1;

-- name: MarkSoftDeleted :execrows
-- Owner soft-delete (M11): active → soft_deleted, stamping soft_deleted_at for
-- the lifecycle sweep. 0 rows ⇒ the blob was absent or not active (the caller
-- distinguishes 404 vs 409 via GetBlobMeta).
UPDATE blobs
SET state = 'soft_deleted', soft_deleted_at = now()
WHERE cid = $1 AND state = 'active';

-- name: ListOverdueSoftDeletes :many
-- The lifecycle sweep's claim (M11): soft-deletes older than the grace cutoff,
-- excluding legal-held trees. Mirrors ListOverdueTombstones' legal-hold filter
-- (holds are set tree-wide, so the blob's own DEK reflects the hold); the
-- no_shred_under_legal_hold CHECK is the hard backstop.
SELECT b.cid
FROM blobs b
LEFT JOIN data_encryption_keys k ON k.id = b.encryption_key_id
WHERE b.state = 'soft_deleted'
  AND b.soft_deleted_at IS NOT NULL
  AND b.soft_deleted_at < sqlc.arg('cutoff')
  AND (k.legal_hold IS NULL OR k.legal_hold = false)
ORDER BY b.soft_deleted_at
LIMIT sqlc.arg('lim');

-- name: ListBlobs :many
-- Operator-wide listing for GET /api/v1/admin/blobs (M11). Optional state /
-- product / owner filters via sqlc.narg (NULL ⇒ no filter), newest-first. Served
-- by blobs_product_state_idx / blobs_owner_state_idx / blobs_uploaded_at_idx.
SELECT
    cid,
    coalesce(owner_id::text, '')::text AS owner_id,
    parent_cid,
    mime_type,
    byte_size,
    uploaded_at,
    state::text                       AS state,
    product::text                     AS product
FROM blobs
WHERE (sqlc.narg('state')::text   IS NULL OR state::text   = sqlc.narg('state'))
  AND (sqlc.narg('product')::text IS NULL OR product::text = sqlc.narg('product'))
  AND (sqlc.narg('owner')::uuid    IS NULL OR owner_id      = sqlc.narg('owner'))
ORDER BY uploaded_at DESC, cid
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountBlobs :one
SELECT count(*)
FROM blobs
WHERE (sqlc.narg('state')::text   IS NULL OR state::text   = sqlc.narg('state'))
  AND (sqlc.narg('product')::text IS NULL OR product::text = sqlc.narg('product'))
  AND (sqlc.narg('owner')::uuid    IS NULL OR owner_id      = sqlc.narg('owner'));

-- name: GetBlobByteSize :one
-- M4 coordinator-as-source preflight: the on-disk envelope size for max_bytes
-- enforcement before streaming (D-M4-3). Only `active` blobs are sourceable for
-- federation replication — quarantined / tombstoned / soft_deleted blobs MUST
-- NOT be served to donors (a no-row result becomes 404 blob_unavailable at the
-- endpoint, which is the correct refusal). `blobs.state` is the `blob_state`
-- enum (`active`, `quarantined`, `tombstoned`, `soft_deleted`, …).
SELECT byte_size FROM blobs WHERE cid = $1 AND state = 'active';
