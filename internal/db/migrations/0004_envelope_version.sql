-- +goose Up
-- +goose StatementBegin
-- Migration 0004: expose envelope_version on blobs.
-- v3.1 amendment. v1 = single-shot XChaCha20-Poly1305 (Phase 1).
-- v2 = streaming-AEAD chunked (Phase 2). The version is determined
-- at encryption time; reads dispatch via the version byte in the
-- envelope bytes themselves, but the column lets us index and
-- filter without parsing every envelope.

ALTER TABLE blobs
    ADD COLUMN envelope_version smallint NOT NULL DEFAULT 1
        CHECK (envelope_version IN (1, 2));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE blobs DROP COLUMN envelope_version;
-- +goose StatementEnd
