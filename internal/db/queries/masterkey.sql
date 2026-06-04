-- name: GetMasterVersionByLabel :one
SELECT id, version_label, state, created_at, retired_at
FROM master_key_versions
WHERE version_label = $1;

-- name: GetRotatingVersion :one
SELECT id, version_label, state, created_at, retired_at
FROM master_key_versions
WHERE state = 'rotating'
ORDER BY created_at
LIMIT 1;

-- name: BeginVersionRotation :execrows
-- Atomically mark the source version 'rotating' iff it is currently 'active'
-- and no other version is already rotating. 0 rows => caller maps to 409/400.
UPDATE master_key_versions AS mkv
SET state = 'rotating'
WHERE mkv.version_label = $1
  AND mkv.state = 'active'
  AND NOT EXISTS (SELECT 1 FROM master_key_versions r WHERE r.state = 'rotating');

-- name: RetireVersion :execrows
UPDATE master_key_versions
SET state = 'retired', retired_at = now()
WHERE version_label = $1 AND state = 'rotating';

-- name: ListMasterVersions :many
SELECT
  v.version_label,
  v.state,
  v.retired_at,
  (SELECT count(*) FROM data_encryption_keys d
     WHERE d.master_key_version_id = v.id AND d.state IN ('active','rotating')) AS dek_count,
  (SELECT count(*) FROM signing_keys s
     WHERE s.master_key_version_id = v.id AND s.state IN ('active','retired')) AS signing_count
FROM master_key_versions v
ORDER BY v.created_at;

-- name: ClaimDEKsForRewrap :many
-- Claim a batch of re-wrappable DEK ids for a version. FOR UPDATE SKIP LOCKED
-- gives clean N-worker parallelism; run inside the per-batch tx so the locks
-- are held until commit. Served by dek_master_version_idx.
SELECT id, wrapped_key
FROM data_encryption_keys
WHERE master_key_version_id = $1 AND state IN ('active','rotating')
ORDER BY id
LIMIT $2
FOR UPDATE SKIP LOCKED;

-- name: RewrapDEK :execrows
-- Atomic, version-guarded re-wrap: wrapped_key + master_key_version_id flip
-- together; the old-version guard makes it idempotent and race-safe.
UPDATE data_encryption_keys
SET wrapped_key = sqlc.arg(wrapped_key), master_key_version_id = sqlc.arg(new_version_id)
WHERE id = sqlc.arg(id) AND master_key_version_id = sqlc.arg(old_version_id);

-- name: CountDEKsForVersion :one
SELECT count(*) FROM data_encryption_keys
WHERE master_key_version_id = $1 AND state IN ('active','rotating');

-- name: ListSigningKeysForRewrap :many
-- Deliberately re-wraps ALL non-shredded retired keys (not only within-grace):
-- a retired-but-not-yet-shredded key still holds real wrapped bytes that would
-- be orphaned if left under the retiring version.
SELECT kid, wrapped_key
FROM signing_keys
WHERE master_key_version_id = $1 AND state IN ('active','retired');

-- name: RewrapSigningKey :execrows
UPDATE signing_keys
SET wrapped_key = sqlc.arg(wrapped_key), master_key_version_id = sqlc.arg(new_version_id)
WHERE kid = sqlc.arg(kid) AND master_key_version_id = sqlc.arg(old_version_id);

-- name: CountSigningKeysForVersion :one
SELECT count(*) FROM signing_keys
WHERE master_key_version_id = $1 AND state IN ('active','retired');
