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
