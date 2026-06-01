-- name: GetUserByEmail :one
SELECT id, email, role, password_hash, disabled, created_at, updated_at
FROM users WHERE email = $1;

-- name: GetUserByID :one
SELECT id, email, role, password_hash, disabled, created_at, updated_at
FROM users WHERE id = $1;

-- name: CreateUser :one
INSERT INTO users (email, role, password_hash)
VALUES ($1, $2, $3)
RETURNING id, email, role, created_at, updated_at;

-- name: SetUserPasswordHash :exec
UPDATE users SET password_hash = $2 WHERE id = $1;

-- name: InsertRefreshToken :one
INSERT INTO refresh_tokens (user_id, token_hash, expires_at, user_agent)
VALUES ($1, $2, $3, $4)
RETURNING id;

-- name: GetRefreshTokenByHash :one
SELECT id, user_id, expires_at, rotated_to, revoked_at
FROM refresh_tokens WHERE token_hash = $1;

-- name: MarkRefreshTokenRotated :execrows
UPDATE refresh_tokens SET rotated_to = $2
WHERE id = $1 AND rotated_to IS NULL AND revoked_at IS NULL;

-- name: RevokeRefreshToken :exec
UPDATE refresh_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL;

-- name: RevokeRefreshTokenFamily :exec
UPDATE refresh_tokens SET revoked_at = now()
WHERE user_id = $1 AND revoked_at IS NULL;

-- name: DeleteExpiredRefreshTokens :execrows
-- Splits the GC across the two partial indexes defined in migration 0006/0007
-- (refresh_tokens_gc_idx, refresh_tokens_revoked_gc_idx). The AND revoked_at
-- IS NULL filter matches the partial-index predicate, so the planner can use
-- the index scan instead of a sequential scan. Revoked-but-old rows are
-- cleaned up by DeleteRevokedRefreshTokensOlderThan below.
DELETE FROM refresh_tokens
WHERE expires_at < now() AND revoked_at IS NULL;

-- name: DeleteRevokedRefreshTokensOlderThan :execrows
-- Drops refresh_tokens rows that were explicitly revoked more than $1 ago
-- (typically 30 days, giving operators a window to forensically inspect
-- revoke events). Uses the refresh_tokens_revoked_gc_idx partial index
-- added in migration 0007.
DELETE FROM refresh_tokens
WHERE revoked_at IS NOT NULL AND revoked_at < $1;
