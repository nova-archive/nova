-- name: CreateUploadToken :one
INSERT INTO upload_tokens (token_hash, label, role, collection_id, product, max_file_size, expires_at, created_by)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING *;

-- name: GetUploadTokenByID :one
SELECT * FROM upload_tokens WHERE id = $1;

-- name: ListUploadTokens :many
SELECT id, label, role, collection_id, product, max_file_size, expires_at, created_by, created_at, last_used_at, revoked_at
FROM upload_tokens ORDER BY created_at DESC;

-- name: RevokeUploadToken :execrows
UPDATE upload_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL;

-- name: TouchUploadTokenUsed :exec
UPDATE upload_tokens SET last_used_at = now() WHERE id = $1;

-- name: CountActiveSessionsByToken :one
SELECT count(*) FROM upload_sessions
WHERE upload_token_id = $1 AND state = 'in_progress' AND expires_at > now();
