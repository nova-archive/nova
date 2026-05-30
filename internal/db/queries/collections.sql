-- name: ResolveEffectiveVisibility :many
-- For an original, resolves its own collection memberships; for a derivative
-- (parent_cid NOT NULL) resolves the PARENT's, since derivatives inherit
-- parent visibility and hold no membership of their own. One query, no N+1.
SELECT c.visibility::text AS visibility
FROM blobs b
JOIN collection_items ci ON ci.blob_cid = COALESCE(b.parent_cid, b.cid)
JOIN collections c        ON c.id = ci.collection_id
WHERE b.cid = $1;
