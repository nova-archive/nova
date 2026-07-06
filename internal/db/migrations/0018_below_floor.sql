-- +goose Up
-- +goose StatementBegin
-- P2-M7.1 (D-M7.1-3): sustained below-floor marker. below_floor_since is the
-- AUTHORITATIVE below-floor marker (NULL = not below floor). It records when
-- the node's reputation_score last crossed below the reputation floor; the
-- Task 11 hysteresis sweep sets it on entry and clears it only once the score
-- recovers past floor + hysteresis_margin. Safety counts (healthy/sourceable/
-- prune/commit) and placement exclude a node only once the marker is OLDER
-- than the grace window (sustained); read/repair source SELECTION may still
-- use below-floor nodes, deprioritized after draining. Set/cleared ONLY by
-- the below-floor sweep — never by register/heartbeat.
ALTER TABLE nodes ADD COLUMN below_floor_since timestamptz;

-- Partial index: the Task 11 sweep and the sustained-grace predicates key on
-- `below_floor_since IS NOT NULL AND below_floor_since <= now() - grace` — a
-- range scan on below_floor_since. The below-floor population is tiny; the
-- partial predicate keeps the index tiny.
CREATE INDEX nodes_below_floor_idx ON nodes (below_floor_since)
    WHERE below_floor_since IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Forward-only by convention (0017): shipped migrations are immutable and the
-- marker is data-bearing operational state; down is a no-op by design.
SELECT 1;
-- +goose StatementEnd
