-- +goose Up
-- +goose StatementBegin
-- Migration 0006: local-issuer auth — password credentials + rotating refresh
-- tokens. See docs/superpowers/specs/2026-05-30-phase1-m6-auth-design.md.
-- Migration-only (not backfilled into DATA_MODEL.sql; migrations are the
-- Phase-1 source of truth sqlc reads, per the M6 design reconciliation).

ALTER TABLE users ADD COLUMN password_hash text;
ALTER TABLE users ADD COLUMN disabled boolean NOT NULL DEFAULT false;

CREATE TABLE refresh_tokens (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash  bytea NOT NULL,
    issued_at   timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    rotated_to  uuid REFERENCES refresh_tokens (id),
    revoked_at  timestamptz,
    user_agent  text,

    UNIQUE (token_hash)
);

CREATE INDEX refresh_tokens_user_idx ON refresh_tokens (user_id);
CREATE INDEX refresh_tokens_gc_idx   ON refresh_tokens (expires_at) WHERE revoked_at IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE refresh_tokens;
ALTER TABLE users DROP COLUMN disabled;
ALTER TABLE users DROP COLUMN password_hash;
-- +goose StatementEnd
