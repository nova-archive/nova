-- +goose Up
-- +goose StatementBegin
-- Migration 0009: owner soft-delete lifecycle (M11).
-- Records WHEN a blob entered soft_deleted so the in-process lifecycle sweep can
-- tombstone + crypto-shred it after the grace window. The blob_state enum (incl.
-- 'soft_deleted') and the blobs table already ship in 0001_init.sql; this adds
-- only the timestamp the sweep ages against and the partial index that serves
-- the sweep's overdue claim.

ALTER TABLE blobs ADD COLUMN soft_deleted_at timestamptz;

CREATE INDEX blobs_soft_delete_sweep_idx
    ON blobs (soft_deleted_at)
    WHERE state = 'soft_deleted';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS blobs_soft_delete_sweep_idx;
ALTER TABLE blobs DROP COLUMN soft_deleted_at;
-- +goose StatementEnd
