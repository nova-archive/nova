-- +goose Up
-- +goose StatementBegin
-- P2-M4.1: coordinator becomes a bounded cache over the donor durable substrate.
CREATE TYPE blob_commit_state     AS ENUM ('staging','committed','failed');
CREATE TYPE coordinator_local_role AS ENUM ('origin','staging','cache','absent');
CREATE TYPE cache_segment         AS ENUM ('probationary','protected');  -- SLRU/2Q (D-M4.1-9)

CREATE TABLE blob_storage_state (
  cid               text PRIMARY KEY REFERENCES blobs(cid) ON DELETE CASCADE,
  commit_state      blob_commit_state      NOT NULL DEFAULT 'committed',
  durability_class  text                   NOT NULL DEFAULT 'normal'
                      CHECK (durability_class IN ('important','normal','cache')),
  local_role        coordinator_local_role NOT NULL DEFAULT 'origin',
  local_present     boolean                NOT NULL DEFAULT true,
  local_bytes       bigint                 NOT NULL DEFAULT 0 CHECK (local_bytes >= 0),
  cache_segment     cache_segment,         -- NULL unless local_role='cache'
  committed_at      timestamptz,
  last_accessed_at  timestamptz,
  prune_eligible_at timestamptz,
  last_refetch_at   timestamptz,
  updated_at        timestamptz            NOT NULL DEFAULT now()
);
-- prune candidate scan: committed + present + class
CREATE INDEX blob_storage_prune_idx ON blob_storage_state (commit_state, local_present, durability_class);
-- SLRU/2Q eviction scan: drain probationary (oldest first), then protected
CREATE INDEX blob_storage_evict_idx ON blob_storage_state (cache_segment, last_accessed_at) WHERE local_present;

ALTER TABLE nodes ADD COLUMN source_nebula_addr text;

-- backfill existing corpus as committed origin copies with derived class + bytes
INSERT INTO blob_storage_state (cid, commit_state, durability_class, local_role, local_present, local_bytes, committed_at)
SELECT b.cid, 'committed',
       CASE WHEN b.parent_cid IS NULL THEN 'important' ELSE 'normal' END,
       'origin', true, COALESCE(m.envelope_size, 0), b.uploaded_at
FROM blobs b LEFT JOIN blob_manifests m ON m.cid = b.cid
WHERE b.state <> 'tombstoned';

-- D-M4.1-16: refresh stale plaintext-sized change-log rows to envelope size
UPDATE pin_changes c SET byte_size = m.envelope_size
FROM blob_manifests m WHERE m.cid = c.cid AND c.byte_size <> m.envelope_size;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- NOTE: the pin_changes.byte_size envelope-size backfill above is intentionally
-- NOT reversed (we cannot know which rows were plaintext-sized, and envelope size
-- is the correct value regardless). This is deliberate, not an oversight.
ALTER TABLE nodes DROP COLUMN source_nebula_addr;
DROP TABLE blob_storage_state;
DROP TYPE cache_segment;
DROP TYPE coordinator_local_role;
DROP TYPE blob_commit_state;
-- +goose StatementEnd
