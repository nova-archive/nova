-- +goose Up
-- +goose StatementBegin
CREATE TABLE upload_tokens (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash    text NOT NULL,
    label         text,
    role          user_role NOT NULL DEFAULT 'uploader',
    collection_id uuid REFERENCES collections (id),
    product       blob_product,
    max_file_size bigint,
    expires_at    timestamptz,
    created_by    uuid NOT NULL REFERENCES users (id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz,
    revoked_at    timestamptz
);
CREATE UNIQUE INDEX upload_tokens_hash_idx ON upload_tokens (token_hash);

ALTER TABLE upload_sessions ADD COLUMN upload_token_id uuid REFERENCES upload_tokens (id);
CREATE INDEX upload_sessions_credential_active_idx
    ON upload_sessions (upload_token_id)
    WHERE state = 'in_progress';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE upload_sessions DROP COLUMN upload_token_id;
DROP TABLE upload_tokens;
-- +goose StatementEnd
