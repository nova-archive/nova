-- name: CreateUploadSession :one
INSERT INTO upload_sessions (owner_id, declared_length, mime_type, product, collection_id, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id;

-- name: GetUploadSession :one
SELECT id, owner_id, declared_length, offset_bytes, mime_type,
       product::text AS product, collection_id, state::text AS state, blob_cid
FROM upload_sessions
WHERE id = $1;

-- name: AdvanceUploadOffset :execrows
UPDATE upload_sessions
SET offset_bytes = $2
WHERE id = $1 AND offset_bytes = $3 AND state = 'in_progress';

-- name: FinalizeUploadSession :exec
UPDATE upload_sessions
SET state = 'finalized', blob_cid = $2
WHERE id = $1;

-- name: AbortUploadSession :exec
UPDATE upload_sessions
SET state = 'aborted'
WHERE id = $1;

-- name: ListExpiredUploadSessions :many
SELECT id
FROM upload_sessions
WHERE state = 'in_progress' AND expires_at < now();

-- name: DeleteUploadSession :exec
DELETE FROM upload_sessions
WHERE id = $1 AND state = 'in_progress';

-- name: SetUploadSessionToken :exec
UPDATE upload_sessions SET upload_token_id = $2 WHERE id = $1;
