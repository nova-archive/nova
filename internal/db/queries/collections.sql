-- name: ResolveEffectiveVisibility :many
-- For an original, resolves its own collection memberships; for a derivative
-- (parent_cid NOT NULL) resolves the PARENT's, since derivatives inherit
-- parent visibility and hold no membership of their own. One query, no N+1.
SELECT c.visibility::text AS visibility
FROM blobs b
JOIN collection_items ci ON ci.blob_cid = COALESCE(b.parent_cid, b.cid)
JOIN collections c        ON c.id = ci.collection_id
WHERE b.cid = $1;

-- name: CreateCollection :one
-- Creates a collection (owner_id must reference an existing user; the
-- public_archival CHECK requires visibility='public'). Backs
-- `novactl collection create` so operators don't seed collections via raw SQL.
INSERT INTO collections (owner_id, name, slug, visibility, public_archival)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListUserIDsByRole :many
-- Owner resolution for `novactl collection create`: the sole operator user is
-- the default collection owner when --owner is omitted.
SELECT id FROM users WHERE role = $1;
