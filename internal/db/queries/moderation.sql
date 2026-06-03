-- Moderation queries (M9). See docs/specs/DATA_MODEL.sql and
-- docs/superpowers/specs/2026-06-02-phase1-m9-moderation-design.md.
-- Schema: moderation_decisions, dmca_cases, takedown_repeat_infringers,
-- and blocklist (0008_moderation.sql) + enums from 0001_init.sql.

-- name: GetBlobForModeration :one
-- owner_id is nullable (anonymous / public_archival uploads); coalesce so the
-- scan never fails on a NULL owner. '' means "no owner" → the caller skips the
-- repeat-infringer strike.
-- The outer ::text pins the sqlc-inferred Go type to a non-null string (a bare
-- coalesce confuses sqlc into interface{}); coalesce makes it non-null at runtime.
SELECT coalesce(owner_id::text, '')::text AS owner_id, state::text AS state, (encryption_key_id IS NOT NULL) AS encrypted
FROM blobs WHERE cid = $1;

-- name: SetBlobState :exec
UPDATE blobs SET state = $2 WHERE cid = $1;

-- name: ListDerivativeCIDs :many
SELECT cid FROM blobs WHERE parent_cid = $1;

-- name: ShredDEKsForBlobTree :exec
UPDATE data_encryption_keys k
SET state = 'shredded', wrapped_key = sqlc.arg('zeros'), shredded_at = now()
FROM blobs b
WHERE b.encryption_key_id = k.id AND (b.cid = sqlc.arg('cid') OR b.parent_cid = sqlc.arg('cid'));

-- name: SetDEKLegalHoldForBlobTree :exec
UPDATE data_encryption_keys k
SET legal_hold = sqlc.arg('hold')
FROM blobs b
WHERE b.encryption_key_id = k.id AND (b.cid = sqlc.arg('cid') OR b.parent_cid = sqlc.arg('cid'));

-- name: InsertModerationDecision :one
INSERT INTO moderation_decisions (cid, rule, rule_ref, action, decided_by, scheduled_tombstone_at, legal_hold, notes)
VALUES (sqlc.arg('cid'), sqlc.arg('rule'), sqlc.narg('rule_ref'), sqlc.arg('action'),
        sqlc.narg('decided_by'), sqlc.narg('scheduled_tombstone_at'), sqlc.arg('legal_hold'), sqlc.narg('notes'))
RETURNING id;

-- name: ClearScheduledTombstone :exec
UPDATE moderation_decisions SET scheduled_tombstone_at = NULL
WHERE cid = $1 AND scheduled_tombstone_at IS NOT NULL;

-- name: ClearModerationLegalHold :exec
UPDATE moderation_decisions SET legal_hold = false, scheduled_tombstone_at = now()
WHERE cid = $1 AND legal_hold = true;

-- name: ListOverdueTombstones :many
SELECT md.cid, md.rule::text AS rule, md.rule_ref
FROM moderation_decisions md
JOIN blobs b ON b.cid = md.cid
LEFT JOIN data_encryption_keys k ON k.id = b.encryption_key_id
WHERE md.scheduled_tombstone_at IS NOT NULL
  AND md.scheduled_tombstone_at < now()
  AND md.action = 'quarantine'
  AND b.state = 'quarantined'
  AND (k.legal_hold IS NULL OR k.legal_hold = false);

-- name: ListModerationDecisions :many
-- decided_by is nullable — the scheduled-tombstone sweep records system actions
-- with decided_by=NULL; coalesce so the listing never crashes after an
-- auto-tombstone. '' renders as a null actor in the handler.
SELECT id, cid, rule::text AS rule, rule_ref, action::text AS action, coalesce(decided_by::text, '')::text AS decided_by,
       decided_at, scheduled_tombstone_at, legal_hold, notes
FROM moderation_decisions
ORDER BY decided_at DESC, id DESC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountModerationDecisions :one
SELECT count(*) FROM moderation_decisions;

-- name: UpsertRepeatInfringer :exec
INSERT INTO takedown_repeat_infringers (user_id, strikes, last_strike_at)
VALUES ($1, 1, now())
ON CONFLICT (user_id) DO UPDATE
SET strikes = takedown_repeat_infringers.strikes + 1, last_strike_at = now();

-- name: InsertDMCACase :one
INSERT INTO dmca_cases (claimant_name, claimant_email, sworn_statement, target_cid)
VALUES ($1, $2, $3, $4) RETURNING id;

-- name: GetDMCACase :one
SELECT id, claimant_name, claimant_email, sworn_statement, target_cid, received_at, actioned_at, status::text AS status
FROM dmca_cases WHERE id = $1;

-- name: ListDMCACases :many
SELECT id, claimant_name, claimant_email, target_cid, received_at, actioned_at, status::text AS status
FROM dmca_cases ORDER BY received_at DESC, id DESC
LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountDMCACases :one
SELECT count(*) FROM dmca_cases;

-- name: SetDMCACaseActioned :exec
UPDATE dmca_cases SET status = 'actioned', actioned_at = now() WHERE id = $1;

-- name: InsertBlocklist :exec
INSERT INTO blocklist (cid, reason, rule, added_by) VALUES ($1, $2, $3, $4)
ON CONFLICT (cid) DO NOTHING;

-- name: DeleteBlocklist :exec
DELETE FROM blocklist WHERE cid = $1;

-- name: ListBlocklist :many
-- added_by is nullable; coalesce so a system-added entry (NULL) never crashes
-- the listing. '' renders as a null actor in the handler.
SELECT cid, reason, rule::text AS rule, coalesce(added_by::text, '')::text AS added_by, created_at
FROM blocklist ORDER BY created_at DESC LIMIT sqlc.arg('lim') OFFSET sqlc.arg('off');

-- name: CountBlocklist :one
SELECT count(*) FROM blocklist;

-- name: IsBlocklisted :one
SELECT EXISTS(SELECT 1 FROM blocklist WHERE cid = $1);
