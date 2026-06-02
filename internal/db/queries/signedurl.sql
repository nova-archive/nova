-- name: GetActiveSigningKey :one
SELECT kid, wrapped_key, master_key_version_id, state, retire_after
FROM signing_keys
WHERE state = 'active'
ORDER BY active_from DESC
LIMIT 1;

-- name: GetSigningKeyByKID :one
SELECT kid, wrapped_key, master_key_version_id, state, retire_after
FROM signing_keys
WHERE kid = $1;

-- name: InsertSigningKey :exec
INSERT INTO signing_keys (kid, algorithm, wrapped_key, master_key_version_id, state, active_from)
VALUES ($1, 'HMAC-SHA256', $2, $3, 'active', now());

-- name: RetirePriorActiveSigningKey :exec
UPDATE signing_keys
SET state = 'retired', retire_after = $1
WHERE state = 'active' AND kid <> $2;

-- name: ShredExpiredRetiredSigningKeys :exec
UPDATE signing_keys
SET state = 'shredded', wrapped_key = $1
WHERE state = 'retired' AND retire_after <= now();

-- name: CountActiveSigningKeys :one
SELECT count(*) FROM signing_keys WHERE state = 'active';

-- name: ListRevocations :many
SELECT kind, value FROM signed_url_revocations;

-- name: InsertRevocation :exec
INSERT INTO signed_url_revocations (kind, value)
VALUES ($1, $2)
ON CONFLICT (kind, value) DO NOTHING;
