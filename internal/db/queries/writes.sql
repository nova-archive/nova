-- name: GetCollectionForWrite :one
SELECT public_archival, visibility::text AS visibility
FROM collections
WHERE id = $1;

-- name: InsertDEK :one
INSERT INTO data_encryption_keys (algorithm, wrapped_key, master_key_version_id, state)
VALUES ($1, $2, $3, 'active')
RETURNING id;

-- name: InsertBlob :exec
INSERT INTO blobs (cid, encryption_key_id, owner_id, mime_type, byte_size, source_ip, product, state, envelope_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'active', 1)
ON CONFLICT (cid) DO NOTHING;

-- name: InsertManifest :exec
INSERT INTO blob_manifests (cid, hash_alg, codec, chunker, plaintext_size, envelope_size, block_count, merkle_root)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (cid) DO NOTHING;

-- name: InsertBlock :exec
INSERT INTO blob_blocks (blob_cid, block_cid, block_index, block_size)
VALUES ($1, $2, $3, $4)
ON CONFLICT (blob_cid, block_index) DO NOTHING;

-- name: InsertCollectionItem :exec
INSERT INTO collection_items (collection_id, blob_cid)
VALUES ($1, $2)
ON CONFLICT (collection_id, blob_cid) DO NOTHING;

-- name: InsertDerivativeBlob :execrows
-- Inserts a derivative blob. ON CONFLICT on the (parent,preset,format) partial
-- unique index DO NOTHING ⇒ 0 rows when a concurrent/cross-process writer won
-- (the caller then unpins its orphan import and reads the winner).
INSERT INTO blobs (cid, encryption_key_id, parent_cid, derivative_preset, derivative_format,
                   mime_type, byte_size, product, state, envelope_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'image', 'active', 1)
ON CONFLICT (parent_cid, derivative_preset, derivative_format) WHERE parent_cid IS NOT NULL
DO NOTHING;

-- name: GetDerivativeCID :one
SELECT cid FROM blobs
WHERE parent_cid = $1 AND derivative_preset = $2 AND derivative_format = $3;
