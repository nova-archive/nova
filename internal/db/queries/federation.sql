-- name: GetNodeByID :one
SELECT * FROM nodes WHERE id = $1;

-- name: RegisterNode :one
INSERT INTO nodes (
    id, nebula_cert_fingerprint, federation_cert_fingerprint, display_name,
    geo_declared, capacity_bytes, bandwidth_budget_bytes_per_day, policy_filters,
    status, trust_state, selected_protocol, advertised_capabilities,
    required_capabilities, client_version
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8,
    'active', 'probationary', $9, $10, $11, $12
)
ON CONFLICT (id) DO UPDATE SET
    nebula_cert_fingerprint        = EXCLUDED.nebula_cert_fingerprint,
    display_name                   = EXCLUDED.display_name,
    geo_declared                   = EXCLUDED.geo_declared,
    capacity_bytes                 = EXCLUDED.capacity_bytes,
    bandwidth_budget_bytes_per_day = EXCLUDED.bandwidth_budget_bytes_per_day,
    policy_filters                 = EXCLUDED.policy_filters,
    selected_protocol              = EXCLUDED.selected_protocol,
    advertised_capabilities        = EXCLUDED.advertised_capabilities,
    required_capabilities          = EXCLUDED.required_capabilities,
    client_version                 = EXCLUDED.client_version
RETURNING *;

-- name: UpdateNodeHeartbeat :one
UPDATE nodes
SET last_seen_at = now(), last_free_bytes = $2, last_stored_bytes = $3
WHERE id = $1
RETURNING *;

-- name: RevokeNode :execrows
UPDATE nodes
SET status = 'revoked', cert_revoked_at = now(), last_status_change_at = now()
WHERE id = $1 AND status <> 'revoked';

-- name: RotateNodeCert :execrows
UPDATE nodes
SET federation_cert_fingerprint = $2,
    cert_rotation_started_at = now(),
    cert_rotated_at = now()
WHERE id = $1;

-- name: ListNodes :many
SELECT id, display_name, status, trust_state, selected_protocol, last_seen_at
FROM nodes
ORDER BY joined_at DESC;
