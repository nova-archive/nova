-- +goose Up
-- +goose StatementBegin
-- Migration 0005: tus resumable-upload session table.
-- See docs/superpowers/specs/2026-05-29-phase1-m4-upload-pipeline-design.md
-- § "Data model addition". Short-lived, GC'd, not partitioned. No filename
-- column (data minimization; blobs stores none either).

CREATE TYPE upload_session_state AS ENUM ('in_progress', 'finalized', 'aborted');

CREATE TABLE upload_sessions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id        uuid REFERENCES users (id),
    declared_length bigint NOT NULL CHECK (declared_length >= 0),
    offset_bytes    bigint NOT NULL DEFAULT 0 CHECK (offset_bytes >= 0),
    mime_type       text,
    product         blob_product NOT NULL DEFAULT 'raw',
    collection_id   uuid REFERENCES collections (id),
    state           upload_session_state NOT NULL DEFAULT 'in_progress',
    blob_cid        text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,

    CONSTRAINT offset_within_length CHECK (offset_bytes <= declared_length)
);

CREATE INDEX upload_sessions_gc_idx
    ON upload_sessions (expires_at)
    WHERE state = 'in_progress';

CREATE TRIGGER upload_sessions_updated_at
    BEFORE UPDATE ON upload_sessions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE upload_sessions;
DROP TYPE upload_session_state;
-- +goose StatementEnd
