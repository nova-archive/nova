-- name: ListAckedNodeDimensions :many
-- Per-node acked pin count + operator-VERIFIED placement dimensions (NULL when the
-- node is unverified or the field is blank). The Go side collapses NULL → "unknown"
-- before grouping, so a node cannot manufacture diversity (D-M5-3a/10).
SELECT n.id AS node_id, count(pa.cid)::bigint AS pins,
       COALESCE(CASE WHEN n.operator_verified_at IS NOT NULL THEN n.failure_domain_id END, '')::text AS failure_domain,
       COALESCE(CASE WHEN n.operator_verified_at IS NOT NULL THEN n.donor_principal_id END, '')::text AS donor_principal,
       COALESCE(CASE WHEN n.operator_verified_at IS NOT NULL THEN n.provider END, '')::text AS provider,
       COALESCE(CASE WHEN n.operator_verified_at IS NOT NULL THEN n.asn END, '')::text AS asn,
       COALESCE(CASE WHEN n.operator_verified_at IS NOT NULL THEN n.region END, '')::text AS region
FROM nodes n
JOIN pin_assignments pa ON pa.node_id = n.id AND pa.state = 'acked'
GROUP BY n.id;

-- name: SumCorpusBytesByClass :many
-- Repair-eligible corpus bytes per durability class (active + quarantined, D-M5-RE),
-- the basis of desired_replicated_bytes = Σ corpus_bytes_c × R_c.
SELECT bss.durability_class, COALESCE(SUM(m.envelope_size), 0)::bigint AS bytes
FROM blob_storage_state bss
JOIN blobs b ON b.cid = bss.cid
JOIN blob_manifests m ON m.cid = bss.cid
WHERE b.state IN ('active', 'quarantined')
GROUP BY bss.durability_class;

-- name: SurvivingCapacity :one
-- Surviving (active/suspect) donor egress + free capacity + count, for the
-- slow-attrition metrics (D-M5-11). Egress prefers the reported capacity telemetry,
-- falling back to the registered daily budget.
SELECT
  COALESCE(SUM(COALESCE(last_egress_capacity_bytes, bandwidth_budget_bytes_per_day)), 0)::bigint AS daily_egress,
  COALESCE(SUM(COALESCE(last_free_bytes, 0)), 0)::bigint AS free_bytes,
  count(*)::bigint AS active_count
FROM nodes WHERE status IN ('active', 'suspect');

-- name: CountRecentlyUnreachable :one
-- Nodes that went unreachable within the mass-casualty window (D-M5-11): an
-- active→unreachable burst signal.
SELECT count(*)::bigint AS n FROM nodes
WHERE status = 'unreachable'
  AND last_status_change_at > now() - make_interval(secs => sqlc.arg(window_seconds)::int);
