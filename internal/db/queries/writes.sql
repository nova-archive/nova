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
