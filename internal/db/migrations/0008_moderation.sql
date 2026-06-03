-- +goose Up
-- +goose StatementBegin
-- Migration 0008: blocklist table for M9 moderation.
-- All other moderation tables (moderation_decisions, dmca_cases,
-- takedown_repeat_infringers) and their enums already ship in 0001_init.sql.

CREATE TABLE blocklist (
    cid         text PRIMARY KEY,
    reason      text NOT NULL,
    rule        moderation_rule NOT NULL DEFAULT 'operator_manual',
    added_by    uuid REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX blocklist_created_at_idx ON blocklist (created_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE blocklist;
-- +goose StatementEnd
