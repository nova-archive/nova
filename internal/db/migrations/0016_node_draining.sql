-- +goose Up
-- +goose StatementBegin
-- P2-M7 (D-M7-6a): voluntary graceful drain. draining_at is the AUTHORITATIVE
-- drain marker (NULL = not draining). It is distinct from placement_weight = 0
-- (a placement throttle that still counts toward durability). Safety counts
-- (healthy/sourceable/prune/commit) exclude draining nodes; read/repair source
-- SELECTION may still use them, deprioritized. Set/cleared ONLY by
-- novactl node drain/undrain — never by register/heartbeat.
ALTER TABLE nodes ADD COLUMN draining_at timestamptz;

-- Partial covering index: ListDrainingNodes is `WHERE draining_at IS NOT NULL
-- ORDER BY id`, so key on (id) with draining_at INCLUDEd — an index-only scan
-- in id order (an index keyed on draining_at would NOT serve that ORDER BY,
-- and the Task-7 EXPLAIN gate asserts this exact index is used). The draining
-- population is tiny; the partial predicate keeps the index tiny.
CREATE INDEX nodes_draining_idx ON nodes (id) INCLUDE (draining_at)
    WHERE draining_at IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX nodes_draining_idx;
ALTER TABLE nodes DROP COLUMN draining_at;
-- +goose StatementEnd
