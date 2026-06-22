-- +goose Up
-- +goose StatementBegin
-- P2-M3: assignment versioning + durable change log (D6/D7). Sync-only and
-- read-selection-ready: blob_replication_state, the nodes D8/D9 placement
-- columns, and pin_audits receive-time columns land with their owning milestones
-- (M5/M6), not here. (cid, node_id) stays the natural current-assignment key;
-- assignment_id is the immutable handle carried in the change log + (M4) repair
-- tokens + acks.
ALTER TABLE pin_assignments
    ADD COLUMN assignment_id uuid   NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN generation    bigint NOT NULL DEFAULT 1,
    ADD CONSTRAINT pin_assignments_assignment_id_key UNIQUE (assignment_id);

CREATE TABLE pin_changes (
    sequence      bigserial PRIMARY KEY,
    node_id       uuid   NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    assignment_id uuid   NOT NULL,
    generation    bigint NOT NULL,
    kind          text   NOT NULL CHECK (kind IN ('assign', 'unpin')),
    cid           text   NOT NULL,
    byte_size     bigint NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX pin_changes_node_seq_idx ON pin_changes (node_id, sequence);

-- Singleton retention watermark: the highest sequence pruned out of pin_changes.
-- A donor whose since_seq < pruned_through_seq must recover via snapshot (D7).
CREATE TABLE federation_change_log_state (
    id                 boolean PRIMARY KEY DEFAULT true CHECK (id),
    pruned_through_seq bigint  NOT NULL DEFAULT 0
);
INSERT INTO federation_change_log_state (id) VALUES (true);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE federation_change_log_state;
DROP TABLE pin_changes;
ALTER TABLE pin_assignments
    DROP CONSTRAINT pin_assignments_assignment_id_key,
    DROP COLUMN generation,
    DROP COLUMN assignment_id;
-- +goose StatementEnd
