-- name: CountSourceableHolders :one
-- Sourceable-holder count: acked pin + reachable + trusted + fresh + has read-source/v1 cap + has nebula addr.
-- Called by commit/prune/read tiers to determine if donor-backed reads are viable.
SELECT count(*) FROM pin_assignments pa JOIN nodes n ON n.id = pa.node_id
WHERE pa.cid = $1 AND pa.state = 'acked'
  AND n.status IN ('active','suspect') AND n.trust_state <> 'suspended'
  AND n.last_seen_at > now() - make_interval(secs => sqlc.arg(stale_secs)::float)
  AND n.advertised_capabilities @> ARRAY['read-source/v1']
  AND n.source_nebula_addr IS NOT NULL AND n.source_nebula_addr <> '';

-- name: ListSourceableHolders :many
-- Best-link sourceable holders: reputation desc, then id for stable rotation.
SELECT n.id AS node_id, pa.assignment_id, pa.generation, n.source_nebula_addr, n.reputation_score
FROM pin_assignments pa JOIN nodes n ON n.id = pa.node_id
WHERE pa.cid = $1 AND pa.state = 'acked'
  AND n.status IN ('active','suspect') AND n.trust_state <> 'suspended'
  AND n.last_seen_at > now() - make_interval(secs => sqlc.arg(stale_secs)::float)
  AND n.advertised_capabilities @> ARRAY['read-source/v1']
  AND n.source_nebula_addr IS NOT NULL AND n.source_nebula_addr <> ''
ORDER BY n.reputation_score DESC, n.id;

-- name: UpsertStorageStateStaging :exec
-- Gate-on Put: insert staging row; ON CONFLICT re-opens a previously failed row.
INSERT INTO blob_storage_state (cid, commit_state, durability_class, local_role, local_present, local_bytes, updated_at)
VALUES ($1, 'staging', sqlc.arg(durability_class), 'staging', false, 0, now())
ON CONFLICT (cid) DO UPDATE SET
    commit_state    = 'staging',
    durability_class = EXCLUDED.durability_class,
    local_role      = 'staging',
    local_present   = false,
    local_bytes     = 0,
    updated_at      = now();

-- name: MarkCommitted :execrows
-- Reconciler: staging → committed, set committed_at and local_bytes.
UPDATE blob_storage_state
SET commit_state  = 'committed',
    local_role    = 'origin',
    local_present = true,
    local_bytes   = sqlc.arg(local_bytes),
    committed_at  = now(),
    updated_at    = now()
WHERE cid = $1 AND commit_state = 'staging';

-- name: MarkFailed :execrows
-- Reconciler: staging → failed (upload or remote-commit error).
UPDATE blob_storage_state
SET commit_state = 'failed',
    updated_at   = now()
WHERE cid = $1 AND commit_state = 'staging';

-- name: TouchLastAccessed :exec
-- Throttled by caller; only update when last_accessed_at is stale or NULL.
UPDATE blob_storage_state
SET last_accessed_at = now(),
    updated_at       = now()
WHERE cid = $1
  AND (last_accessed_at IS NULL OR last_accessed_at < sqlc.arg(threshold_at));

-- name: AdmitToCache :exec
-- Donor-fetched: set local_role='cache', cache_segment='probationary', record bytes.
INSERT INTO blob_storage_state (cid, commit_state, durability_class, local_role, cache_segment, local_present, local_bytes, last_accessed_at, updated_at)
VALUES ($1, 'committed', sqlc.arg(durability_class), 'cache', 'probationary', true, sqlc.arg(local_bytes), now(), now())
ON CONFLICT (cid) DO UPDATE SET
    local_role       = 'cache',
    cache_segment    = 'probationary',
    local_present    = true,
    local_bytes      = sqlc.arg(local_bytes),
    last_accessed_at = now(),
    updated_at       = now();

-- name: PromoteToProtected :exec
-- Cache hit on a probationary row: promote to protected segment (throttled like TouchLastAccessed).
UPDATE blob_storage_state
SET cache_segment    = 'protected',
    last_accessed_at = now(),
    updated_at       = now()
WHERE cid = $1
  AND local_role = 'cache'
  AND cache_segment = 'probationary'
  AND (last_accessed_at IS NULL OR last_accessed_at < sqlc.arg(threshold_at));

-- name: SumCacheBytes :one
-- Per-segment byte totals for admission/eviction budgeting.
SELECT
    COALESCE(SUM(local_bytes) FILTER (WHERE cache_segment = 'probationary'), 0)::bigint AS probationary_bytes,
    COALESCE(SUM(local_bytes) FILTER (WHERE cache_segment = 'protected'),    0)::bigint AS protected_bytes
FROM blob_storage_state
WHERE local_role = 'cache' AND local_present;

-- name: ListEvictionCandidates :many
-- SLRU/2Q drain order: probationary oldest-first (false < true in Postgres boolean sort),
-- then protected oldest-first. Limit provided by caller.
SELECT cid, local_bytes, cache_segment
FROM blob_storage_state
WHERE local_role = 'cache' AND local_present
ORDER BY (cache_segment = 'protected'), last_accessed_at
LIMIT sqlc.arg(lim);

-- name: SetLocalPresence :exec
-- Pruner/cache: update local_present, local_role, cache_segment, local_bytes, prune_eligible_at.
UPDATE blob_storage_state
SET local_present     = sqlc.arg(local_present),
    local_role        = sqlc.arg(local_role),
    cache_segment     = sqlc.arg(cache_segment),
    local_bytes       = sqlc.arg(local_bytes),
    prune_eligible_at = sqlc.arg(prune_eligible_at),
    updated_at        = now()
WHERE cid = $1;

-- name: GetStorageState :one
-- Full row fetch for internal state inspection.
SELECT cid, commit_state, durability_class, local_role, local_present, local_bytes,
       cache_segment, committed_at, last_accessed_at, prune_eligible_at, last_refetch_at, updated_at
FROM blob_storage_state
WHERE cid = $1;

-- name: ListPruneCandidates :many
-- Committed + present + mode-eligible durability class; ordered by prune_eligible_at (oldest first).
SELECT cid, local_bytes, durability_class, prune_eligible_at
FROM blob_storage_state
WHERE commit_state = 'committed'
  AND local_present
  AND durability_class = sqlc.arg(durability_class)
  AND (prune_eligible_at IS NULL OR prune_eligible_at <= now())
ORDER BY prune_eligible_at
LIMIT sqlc.arg(lim);

-- name: GetCommitState :one
-- Resolve visibility: is the blob committed (write-through to origin)?
SELECT commit_state FROM blob_storage_state WHERE cid = $1;
