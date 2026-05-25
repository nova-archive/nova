-- +goose Up
-- +goose StatementBegin
-- Migration 0002: job queue table (partitioned by created_at, monthly).
-- See docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md
-- § "Job lifecycle".

CREATE TYPE job_state AS ENUM (
    'pending',
    'leased',
    'completed',
    'failed',
    'dead'
);

CREATE TABLE jobs (
    id            uuid NOT NULL DEFAULT gen_random_uuid(),
    kind          text NOT NULL,
    payload       jsonb NOT NULL DEFAULT '{}'::jsonb,
    state         job_state NOT NULL DEFAULT 'pending',
    attempts      int NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts  int NOT NULL DEFAULT 5 CHECK (max_attempts > 0),
    lease_until   timestamptz,
    not_before    timestamptz NOT NULL DEFAULT now(),
    last_error    text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

-- Initial partition for the current month + the next month so the table
-- accepts rows immediately and the partition-rotation job has headroom.
CREATE TABLE jobs_default PARTITION OF jobs
    FOR VALUES FROM (MINVALUE) TO ('2026-06-01 00:00:00+00');
CREATE TABLE jobs_2026_06 PARTITION OF jobs
    FOR VALUES FROM ('2026-06-01 00:00:00+00') TO ('2026-07-01 00:00:00+00');

-- Worker leasing index: pending jobs ordered by created_at, with not_before
-- guarding scheduled-future work. Partial index for the hot path.
CREATE INDEX jobs_lease_idx
    ON jobs (created_at, not_before)
    WHERE state = 'pending';

-- State + kind index for admin introspection.
CREATE INDEX jobs_state_kind_idx ON jobs (state, kind, created_at DESC);

-- Lease reclaim index for stuck-job recovery.
CREATE INDEX jobs_lease_reclaim_idx
    ON jobs (lease_until)
    WHERE state = 'leased';

CREATE TRIGGER jobs_updated_at
    BEFORE UPDATE ON jobs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE jobs CASCADE;
DROP TYPE job_state;
-- +goose StatementEnd
