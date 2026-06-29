-- +goose Up
-- +goose StatementBegin
-- P2-M5: liveness & healing. The verified donor replica set is kept alive — donor
-- failure is detected (5-state liveness), Tier-1 durability is restored via
-- donor↔donor repair, and replica placement resists correlated loss. This adds the
-- donor-replica-health projection (distinct from M4.1's coordinator-local
-- blob_storage_state), the bounded reconcile queue + webhook suppression store, the
-- D8/D9 placement + sync-state + egress-telemetry columns, and the repair
-- source-binding columns.

-- D8/D9 placement (0011 deferred these here), sync-state countability guard,
-- revoked-observation marker, and egress scheduling-hint telemetry. geo_declared
-- stays informational; only operator_verified_at-backed dimensions are trusted by
-- anti-affinity.
ALTER TABLE nodes
    ADD COLUMN failure_domain_id     text,
    ADD COLUMN donor_principal_id    text,
    ADD COLUMN provider              text,
    ADD COLUMN asn                   text,
    ADD COLUMN region                text,
    ADD COLUMN operator_verified_at  timestamptz,
    ADD COLUMN placement_weight      real NOT NULL DEFAULT 1.0
        CHECK (placement_weight >= 0.0 AND placement_weight <= 1.0),
    ADD COLUMN assignment_sync_state text NOT NULL DEFAULT 'current'
        CHECK (assignment_sync_state IN ('current', 'snapshot_required', 'reconciling')),
    ADD COLUMN revoked_signaled_at   timestamptz,
    ADD COLUMN last_egress_remaining_bytes bigint
        CHECK (last_egress_remaining_bytes IS NULL OR last_egress_remaining_bytes >= 0),
    ADD COLUMN last_egress_capacity_bytes bigint
        CHECK (last_egress_capacity_bytes IS NULL OR last_egress_capacity_bytes >= 0),
    ADD COLUMN last_egress_refill_bps bigint
        CHECK (last_egress_refill_bps IS NULL OR last_egress_refill_bps >= 0);

-- Repair source binding (D-M5-8a). source_node_id is a NULLABLE FK to nodes(id):
-- NULL means coordinator-sourced. The reserved synthetic wire.CoordinatorSourceID
-- is a non-nodes constant and is NEVER stored here; /pins/changes translates NULL
-- into ChangeSource.NodeID = wire.CoordinatorSourceID on the wire.
-- source_attempts/source_next_attempt_at give durable requeue backoff for flapping
-- sources.
ALTER TABLE pin_assignments
    ADD COLUMN source_node_id         uuid REFERENCES nodes (id),
    ADD COLUMN source_attempts        integer NOT NULL DEFAULT 0,
    ADD COLUMN source_next_attempt_at timestamptz;
ALTER TABLE pin_changes ADD COLUMN source_node_id uuid;

-- Donor-replica-health projection (D-M5-2). Rebuildable cache: authority remains
-- pin_assignments ⨝ node liveness. healthy_acked_count drives the acked-only
-- Tier-1/Tier-2 trigger; sourceable_acked_count is the read-availability count
-- (read-source/v1). target_count is R(class); a config R change reconciles it.
CREATE TABLE blob_replication_state (
    cid                    text PRIMARY KEY REFERENCES blobs (cid) ON DELETE CASCADE,
    healthy_acked_count    integer NOT NULL DEFAULT 0,
    sourceable_acked_count integer NOT NULL DEFAULT 0,
    in_flight_count        integer NOT NULL DEFAULT 0,
    target_count           integer NOT NULL,
    safety_tier            text NOT NULL
        CHECK (safety_tier IN ('donor_lost', 'tier1', 'tier2', 'healthy')),
    local_recoverable      boolean NOT NULL DEFAULT false,
    durability_class       text NOT NULL
        CHECK (durability_class IN ('important', 'normal', 'cache')),
    dirty                  boolean NOT NULL DEFAULT false,
    updated_at             timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX blob_replication_safety_idx     ON blob_replication_state (safety_tier, updated_at);
CREATE INDEX blob_replication_class_tier_idx ON blob_replication_state (durability_class, safety_tier);
CREATE INDEX blob_replication_dirty_idx      ON blob_replication_state (dirty) WHERE dirty;

-- Durable bounded reconcile queue for bulk (provider-purge) liveness transitions
-- (D-M5-2d): a transition enqueues affected CIDs in its tx; a bounded async drain
-- recomputes them from authority. The scheduler treats queued/dirty CIDs as
-- recompute-before-schedule, never schedule-from-stale.
CREATE TABLE blob_replication_reconcile_queue (
    cid         text PRIMARY KEY REFERENCES blobs (cid) ON DELETE CASCADE,
    reason      text NOT NULL,
    enqueued_at timestamptz NOT NULL DEFAULT now()
);

-- Durable scoped suppression windows for the best-effort webhook dispatcher
-- (D-M5-9a): once-per-window keyed by event_type + destination + scope_key, so a
-- restart cannot reopen a just-closed window and node A's event never suppresses
-- node B's.
CREATE TABLE webhook_suppression (
    event_type    text NOT NULL,
    destination   text NOT NULL,
    scope_key     text NOT NULL,
    last_fired_at timestamptz NOT NULL,
    PRIMARY KEY (event_type, destination, scope_key)
);

-- Backfill the projection from blob_storage_state (M4.1's per-blob row,
-- authoritative for durability_class), LEFT JOIN nothing — anchoring on
-- blob_storage_state (not pin_assignments) means an active/quarantined blob with
-- ZERO donor holders is still represented (as donor_lost). Every row seeds
-- dirty=true so the Task-2 recompute establishes the authoritative counts/tier
-- without duplicating the count logic in SQL here.
INSERT INTO blob_replication_state
    (cid, healthy_acked_count, sourceable_acked_count, in_flight_count, target_count,
     safety_tier, local_recoverable, durability_class, dirty)
SELECT s.cid,
       0, 0, 0,
       CASE s.durability_class WHEN 'important' THEN 5 WHEN 'cache' THEN 2 ELSE 3 END,
       'donor_lost',
       (s.local_present AND s.local_role IN ('origin', 'staging', 'cache')),
       s.durability_class,
       true
FROM blob_storage_state s
JOIN blobs b ON b.cid = s.cid
WHERE b.state IN ('active', 'quarantined')
ON CONFLICT (cid) DO NOTHING;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE webhook_suppression;
DROP TABLE blob_replication_reconcile_queue;
DROP TABLE blob_replication_state;
ALTER TABLE pin_changes DROP COLUMN source_node_id;
ALTER TABLE pin_assignments
    DROP COLUMN source_next_attempt_at,
    DROP COLUMN source_attempts,
    DROP COLUMN source_node_id;
ALTER TABLE nodes
    DROP COLUMN last_egress_refill_bps,
    DROP COLUMN last_egress_capacity_bytes,
    DROP COLUMN last_egress_remaining_bytes,
    DROP COLUMN revoked_signaled_at,
    DROP COLUMN assignment_sync_state,
    DROP COLUMN placement_weight,
    DROP COLUMN operator_verified_at,
    DROP COLUMN region,
    DROP COLUMN asn,
    DROP COLUMN provider,
    DROP COLUMN donor_principal_id,
    DROP COLUMN failure_domain_id;
-- +goose StatementEnd
