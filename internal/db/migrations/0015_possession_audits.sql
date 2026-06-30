-- +goose Up
-- +goose StatementBegin
-- 0015_possession_audits.sql
-- P2-M6: possession audits & reputation. Forward-only ALTER/reconciliation —
-- pin_audits + audit_result already exist in frozen 0001_init; this adds only the
-- D10 receive-time column, an always-set decision time, the transcript digest, the
-- trust-epoch/review columns, and scheduler-support indexes. No Phase-1 migration
-- is modified (migrations-frozen stays green).

ALTER TABLE pin_audits
    ADD COLUMN received_at     timestamptz,   -- coordinator response receive-time; NULL on timeout (D10)
    ADD COLUMN decided_at      timestamptz,   -- when the coordinator decided the outcome (always set)
    ADD COLUMN transcript_hash bytea;         -- domain-separated audit transcript digest (D-M6-3a)

ALTER TABLE nodes
    ADD COLUMN trust_epoch_started_at   timestamptz NOT NULL DEFAULT now(), -- graduation-evidence anchor
    ADD COLUMN trust_review_required_at timestamptz,                        -- set on hash-mismatch; gates graduation
    ADD COLUMN trust_review_reason      text;

-- Existing donors keep their tenure: anchor the epoch at registration, not deploy.
UPDATE nodes SET trust_epoch_started_at = joined_at;

-- New-ack fast lane: "freshly acked within 15 min".
CREATE INDEX pin_assignments_acked_at_idx
    ON pin_assignments (acked_at) WHERE state = 'acked';

-- Audit recency: most-recent pass per (node, blob).
CREATE INDEX pin_audits_recent_pass_node_blob_idx
    ON pin_audits (node_id, blob_cid, received_at DESC) WHERE result = 'pass';

-- Failure history: decided_at is set even when received_at is NULL (timeouts).
CREATE INDEX pin_audits_recent_fail_node_idx
    ON pin_audits (node_id, decided_at DESC) WHERE result = 'fail';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS pin_audits_recent_fail_node_idx;
DROP INDEX IF EXISTS pin_audits_recent_pass_node_blob_idx;
DROP INDEX IF EXISTS pin_assignments_acked_at_idx;
ALTER TABLE nodes
    DROP COLUMN trust_review_reason,
    DROP COLUMN trust_review_required_at,
    DROP COLUMN trust_epoch_started_at;
ALTER TABLE pin_audits
    DROP COLUMN transcript_hash,
    DROP COLUMN decided_at,
    DROP COLUMN received_at;
-- +goose StatementEnd
