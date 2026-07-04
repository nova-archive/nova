-- +goose Up
-- +goose StatementBegin
-- Migration 0017: month-relative partition provisioning (P2-M7.1, D-M7.1-2).
-- 0002/0003 committed fixed partitions ending 2026-07-01; a fresh install in
-- any later month was insert-broken, and nothing create-aheads jobs at
-- runtime. Provision current month + 2 for all three partitioned parents,
-- idempotently, using the retention.go naming/bounds convention
-- (parent_YYYY_MM, bounds 'YYYY-MM-DD 00:00:00+00').
--
-- p_start/p_end are timestamp (not timestamptz): date_trunc over
-- now() AT TIME ZONE 'UTC' yields a plain UTC-wall-clock timestamp, and
-- keeping it plain makes to_char() independent of the session TimeZone. The
-- '+00' suffix is appended explicitly so the bound literals match
-- retention.go's boundLiteral exactly.
DO $$
DECLARE
    parent  text;
    m       int;
    p_start timestamp;
    p_end   timestamp;
    p_name  text;
BEGIN
    FOREACH parent IN ARRAY ARRAY['jobs','integrity_audits','audit_log'] LOOP
        FOR m IN 0..2 LOOP
            p_start := date_trunc('month', now() AT TIME ZONE 'UTC') + make_interval(months => m);
            p_end   := p_start + interval '1 month';
            p_name  := format('%s_%s', parent, to_char(p_start, 'YYYY_MM'));
            EXECUTE format(
                'CREATE TABLE IF NOT EXISTS %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)',
                p_name, parent,
                to_char(p_start, 'YYYY-MM-DD') || ' 00:00:00+00',
                to_char(p_end,   'YYYY-MM-DD') || ' 00:00:00+00');
        END LOOP;
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Forward-only: the provisioned partitions are data-bearing; down is a no-op
-- by design (0002/0003's downs drop the parents, and partitions with them).
SELECT 1;
-- +goose StatementEnd
