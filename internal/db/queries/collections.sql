-- name: ResolveBlobVisibility :many
SELECT c.visibility::text AS visibility
FROM collection_items ci
JOIN collections c ON c.id = ci.collection_id
WHERE ci.blob_cid = $1;
