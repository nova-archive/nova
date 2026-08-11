-- P2-M7.3: upgrade runs and events, reported and expected donor metadata, and
-- the runtime-contract observation marker.
--
-- Nothing recorded what was upgraded, when, or from what. Field-findings §23's
-- three questions — what order do I upgrade, how do I prove it worked, and
-- exactly how do I go back — were unanswerable because the system had no place
-- to state the answers.

-- +goose Up
-- +goose StatementBegin

-- Upgrade history as a RUN with PHASES. One flat row cannot describe an
-- interrupted upgrade, which is exactly when the record matters most.
CREATE TABLE upgrade_runs (
    id uuid PRIMARY KEY,
    from_release text NOT NULL DEFAULT '',
    to_release   text NOT NULL,
    from_schema bigint NOT NULL,
    to_schema   bigint NOT NULL,
    release_lock_digest text NOT NULL DEFAULT '',
    expected_artifacts jsonb NOT NULL DEFAULT '{}'::jsonb,
    obligations        jsonb NOT NULL DEFAULT '{}'::jsonb,
    config_fingerprint_before text NOT NULL DEFAULT '',
    config_fingerprint_after  text NOT NULL DEFAULT '',
    state text NOT NULL CHECK (state IN ('started','passed','failed','aborted','skipped','interrupted')),
    actor text NOT NULL DEFAULT '',
    started_at   timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz
);

-- Upgrade history is evidence (T1.24). ON DELETE CASCADE would let deleting a
-- run erase its own events, so the FK RESTRICTs; removing history is a
-- privileged action and belongs in audit_log.
--
-- (run_id, sequence) is the REPLAY KEY. The local journal is replayed into this
-- table after 0019 applies, and a crash halfway through that backfill would
-- otherwise duplicate every event already written when the operator restarts. A
-- bigserial id alone cannot express "this journal event is already here".
CREATE TABLE upgrade_events (
    id bigserial PRIMARY KEY,
    run_id uuid NOT NULL REFERENCES upgrade_runs(id) ON DELETE RESTRICT,
    sequence bigint NOT NULL,
    phase text NOT NULL CHECK (phase IN ('preflight','apply','verify','rollback')),
    state text NOT NULL CHECK (state IN ('started','passed','failed','aborted','skipped','interrupted')),
    detail jsonb NOT NULL DEFAULT '{}'::jsonb,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, sequence)
);

CREATE INDEX upgrade_runs_started_at_idx ON upgrade_runs (started_at DESC);
CREATE INDEX upgrade_events_run_idx      ON upgrade_events (run_id, recorded_at);

-- IDENTITY CLAIMS: census-only. These never gate protocol access, placement,
-- durability counting, trust graduation, drain or eviction. A mismatch against
-- the operator's expected_* value is a supply-chain warning, nothing more. A
-- process cannot discover its own OCI manifest digest without a Docker socket,
-- which Nova gives neither coordinator nor doctor, so these are CONFIGURED
-- declarations and a donor may report anything.
--
-- Image digest and bundle-lock digest are separate relations because a donor
-- can match one and not the other.
ALTER TABLE nodes ADD COLUMN reported_client_version     text;
ALTER TABLE nodes ADD COLUMN reported_image_digest       text;
ALTER TABLE nodes ADD COLUMN reported_bundle_lock_digest text;

-- CAPABILITY ADVERTISEMENTS: these DO gate routing — a donor that cannot serve
-- reads must not be sent read work — which is a routing decision, not a
-- security authorization. A lying donor can only deny itself work or accept
-- work it will fail, and both already have failure paths.
--
-- effective_capabilities is the SINGLE operational source. federation.sql,
-- replication.sql, storage_state.sql and possession.sql migrate onto it
-- together; keeping a second source is how a role predicate silently goes on
-- consulting a stale set. The raw reported claim is kept only for the census.
--
-- Donor-advertised protocols are stored separately from selected_protocol,
-- which is the coordinator's negotiated outcome and never a donor claim.
ALTER TABLE nodes ADD COLUMN effective_capabilities text[];
ALTER TABLE nodes ADD COLUMN reported_protocols     text[];
UPDATE nodes SET effective_capabilities = advertised_capabilities;

-- Legacy omission and rollback produce the IDENTICAL wire message: a heartbeat
-- carrying no runtime_contract. This marker is the only thing that separates
-- them.
--
--   NULL     => this node has never sent a runtime contract, so omission means
--               a pre-M7.3 donor and its registration snapshot stands.
--   non-NULL => omission means a downgrade, and stale optional-role
--               eligibility must be dropped while replicas are retained.
--
-- Re-registration resets it to NULL. Without that, a legacy donor returning
-- after eviction inherits a stamped marker and its next contract-less heartbeat
-- reads as a rollback.
ALTER TABLE nodes ADD COLUMN runtime_contract_observed_at timestamptz;

-- EXPECTED: what the OPERATOR authorized for this node, taken from a verified
-- release lock via `novactl node rollout authorize`. Storing an expectation
-- with nothing to compare it against would leave the operator unable to confirm
-- the rollout they authorized.
ALTER TABLE nodes ADD COLUMN expected_image_digest       text;
ALTER TABLE nodes ADD COLUMN expected_bundle_lock_digest text;
ALTER TABLE nodes ADD COLUMN expected_at                 timestamptz;
ALTER TABLE nodes ADD COLUMN expected_by                 text;

-- nodes.client_version is deliberately untouched: an old registration still
-- writes it, and reported_client_version is the runtime-contract channel.

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE nodes DROP COLUMN expected_by;
ALTER TABLE nodes DROP COLUMN expected_at;
ALTER TABLE nodes DROP COLUMN expected_bundle_lock_digest;
ALTER TABLE nodes DROP COLUMN expected_image_digest;
ALTER TABLE nodes DROP COLUMN runtime_contract_observed_at;
ALTER TABLE nodes DROP COLUMN reported_protocols;
ALTER TABLE nodes DROP COLUMN effective_capabilities;
ALTER TABLE nodes DROP COLUMN reported_bundle_lock_digest;
ALTER TABLE nodes DROP COLUMN reported_image_digest;
ALTER TABLE nodes DROP COLUMN reported_client_version;
DROP INDEX IF EXISTS upgrade_events_run_idx;
DROP INDEX IF EXISTS upgrade_runs_started_at_idx;
DROP TABLE upgrade_events;
DROP TABLE upgrade_runs;
-- +goose StatementEnd
