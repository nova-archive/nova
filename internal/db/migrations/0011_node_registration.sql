-- +goose Up
-- +goose StatementBegin
-- P2-M2: node-registration columns. trust_state is text+CHECK (not an enum):
-- the classification is young and a text domain is cheaper to amend than
-- ALTER TYPE. D8 failure-domain / placement_weight columns are deferred to P2-M5
-- (nothing places pins in M2); pin_changes/assignment generation are P2-M3.
ALTER TABLE nodes
    ADD COLUMN trust_state text NOT NULL DEFAULT 'probationary'
        CHECK (trust_state IN ('probationary', 'trusted', 'suspended')),
    ADD COLUMN selected_protocol       text,
    ADD COLUMN advertised_capabilities text[] NOT NULL DEFAULT '{}',
    ADD COLUMN required_capabilities   text[] NOT NULL DEFAULT '{}',
    ADD COLUMN client_version          text,
    ADD COLUMN cert_revoked_at          timestamptz,
    ADD COLUMN cert_rotation_started_at timestamptz,
    ADD COLUMN cert_rotated_at          timestamptz,
    ADD COLUMN last_free_bytes   bigint CHECK (last_free_bytes   IS NULL OR last_free_bytes   >= 0),
    ADD COLUMN last_stored_bytes bigint CHECK (last_stored_bytes IS NULL OR last_stored_bytes >= 0);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE nodes
    DROP COLUMN trust_state,
    DROP COLUMN selected_protocol,
    DROP COLUMN advertised_capabilities,
    DROP COLUMN required_capabilities,
    DROP COLUMN client_version,
    DROP COLUMN cert_revoked_at,
    DROP COLUMN cert_rotation_started_at,
    DROP COLUMN cert_rotated_at,
    DROP COLUMN last_free_bytes,
    DROP COLUMN last_stored_bytes;
-- +goose StatementEnd
