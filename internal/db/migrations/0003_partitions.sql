-- +goose Up
-- +goose StatementBegin
-- Migration 0003: convert integrity_audits and audit_log to partitioned tables.
-- Safe in Phase 1 because there is no prior data. Operators upgrading from
-- pre-Phase-1 dev databases will lose audit history; document in
-- docs/quickstart.md.

DROP TABLE integrity_audits;
DROP TABLE audit_log;

CREATE TABLE integrity_audits (
    id          bigserial NOT NULL,
    cid         text NOT NULL,
    audit_kind  audit_kind NOT NULL,
    result      audit_result NOT NULL,
    error       text,
    audited_at  timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (id, audited_at)
) PARTITION BY RANGE (audited_at);

CREATE TABLE integrity_audits_default PARTITION OF integrity_audits
    FOR VALUES FROM (MINVALUE) TO ('2026-06-01 00:00:00+00');
CREATE TABLE integrity_audits_2026_06 PARTITION OF integrity_audits
    FOR VALUES FROM ('2026-06-01 00:00:00+00') TO ('2026-07-01 00:00:00+00');

CREATE INDEX integrity_audits_cid_kind_idx ON integrity_audits (cid, audit_kind, audited_at DESC);
CREATE INDEX integrity_audits_failures_idx
    ON integrity_audits (audit_kind, audited_at DESC)
    WHERE result <> 'pass';

CREATE TABLE audit_log (
    id           bigserial NOT NULL,
    actor_id     uuid REFERENCES users (id),
    action       text NOT NULL,
    target_type  text NOT NULL,
    target_id    text NOT NULL,
    payload      jsonb NOT NULL DEFAULT '{}'::jsonb,
    at           timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (id, at)
) PARTITION BY RANGE (at);

CREATE TABLE audit_log_default PARTITION OF audit_log
    FOR VALUES FROM (MINVALUE) TO ('2026-06-01 00:00:00+00');
CREATE TABLE audit_log_2026_06 PARTITION OF audit_log
    FOR VALUES FROM ('2026-06-01 00:00:00+00') TO ('2026-07-01 00:00:00+00');

CREATE INDEX audit_log_target_idx ON audit_log (target_type, target_id);
CREATE INDEX audit_log_actor_idx ON audit_log (actor_id, at DESC);
CREATE INDEX audit_log_at_idx ON audit_log (at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE integrity_audits CASCADE;
DROP TABLE audit_log CASCADE;
-- Recreate the original unpartitioned forms (matches 0001).
CREATE TABLE integrity_audits (
    id          bigserial PRIMARY KEY,
    cid         text NOT NULL,
    audit_kind  audit_kind NOT NULL,
    result      audit_result NOT NULL,
    error       text,
    audited_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX integrity_audits_cid_kind_idx ON integrity_audits (cid, audit_kind, audited_at DESC);
CREATE INDEX integrity_audits_failures_idx
    ON integrity_audits (audit_kind, audited_at DESC)
    WHERE result <> 'pass';
CREATE TABLE audit_log (
    id           bigserial PRIMARY KEY,
    actor_id     uuid REFERENCES users (id),
    action       text NOT NULL,
    target_type  text NOT NULL,
    target_id    text NOT NULL,
    payload      jsonb NOT NULL DEFAULT '{}'::jsonb,
    at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_target_idx ON audit_log (target_type, target_id);
CREATE INDEX audit_log_actor_idx ON audit_log (actor_id, at DESC);
CREATE INDEX audit_log_at_idx ON audit_log (at DESC);
-- +goose StatementEnd
