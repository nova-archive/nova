package metrics

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

// Fixture: one draining node D holding a CID (target 2) alone → drain debt 1;
// one below-floor node F with 2 acked replicas; a replication-state row and a
// reconcile-queue row so the projection families are non-empty.
func seedMetricsFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (drainingID string) {
	t.Helper()
	const (
		nodeD = "dddddddd-dddd-dddd-dddd-000000000001"
		nodeF = "ffffffff-ffff-ffff-ffff-000000000002"
	)
	for _, n := range []string{nodeD, nodeF} {
		_, err := pool.Exec(ctx, `
			INSERT INTO nodes (id, nebula_cert_fingerprint, federation_cert_fingerprint, capacity_bytes,
			                   bandwidth_budget_bytes_per_day, policy_filters, status,
			                   assignment_sync_state, trust_state)
			VALUES ($1::uuid, $2, $3, 1073741824, 1073741824, '{}', 'active', 'current', 'trusted')`,
			n, n+"-nfp", n+"-ffp")
		require.NoError(t, err)
	}
	_, err := pool.Exec(ctx, `UPDATE nodes SET draining_at = now() WHERE id = $1::uuid`, nodeD)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE nodes SET reputation_score = 0.3 WHERE id = $1::uuid`, nodeF)
	require.NoError(t, err)
	// F has carried a below-floor marker for 2h — SUSTAINED against the 1h grace
	// the scrape uses, so nova_below_floor_nodes{state="sustained"} == 1.
	_, err = pool.Exec(ctx, `UPDATE nodes SET below_floor_since = now() - interval '2 hours' WHERE id = $1::uuid`, nodeF)
	require.NoError(t, err)

	for _, cid := range []string{"m-cid-1", "m-cid-2", "m-cid-3"} {
		_, err = pool.Exec(ctx, `
			INSERT INTO blobs (cid, mime_type, byte_size, state, product, envelope_version)
			VALUES ($1, 'image/jpeg', 1000, 'active', 'image', 2)`, cid)
		require.NoError(t, err)
	}
	// D is the SOLE acked holder of m-cid-1 (target 2) → drain debt 1.
	_, err = pool.Exec(ctx, `INSERT INTO pin_assignments (cid, node_id, state) VALUES ('m-cid-1', $1::uuid, 'acked')`, nodeD)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO blob_replication_state
			(cid, healthy_acked_count, sourceable_acked_count, in_flight_count,
			 target_count, safety_tier, local_recoverable, durability_class, dirty)
		VALUES ('m-cid-1', 0, 0, 0, 2, 'donor_lost', true, 'normal', false)`)
	require.NoError(t, err)
	// F holds 2 acked replicas below the floor.
	for _, cid := range []string{"m-cid-2", "m-cid-3"} {
		_, err = pool.Exec(ctx, `INSERT INTO pin_assignments (cid, node_id, state) VALUES ($1, $2::uuid, 'acked')`, cid, nodeF)
		require.NoError(t, err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO blob_replication_reconcile_queue (cid, reason) VALUES ('m-cid-1', 'node_draining')`)
	require.NoError(t, err)
	// One durable audit row so the restart-stable counter family materializes
	// (a const-metric collector emits nothing for zero-row families).
	_, err = pool.Exec(ctx, `
		INSERT INTO pin_audits (blob_cid, node_id, challenge_kind, nonce, deadline, result)
		VALUES ('m-cid-1', $1::uuid, 'block_hash', 'n-1', now() + interval '1 minute', 'pass')`, nodeD)
	require.NoError(t, err)
	return nodeD
}

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}

func metricValue(t *testing.T, body, family, labelFragment string) float64 {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(family) + `(\{[^}]*\})? ([0-9.e+-]+)$`)
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		if labelFragment == "" || strings.Contains(m[1], labelFragment) {
			v, err := strconv.ParseFloat(m[2], 64)
			require.NoError(t, err)
			return v
		}
	}
	t.Fatalf("family %s (labels containing %q) not found in scrape:\n%s", family, labelFragment, body)
	return 0
}

func TestScrapeFamiliesAndValues(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	drainingID := seedMetricsFixture(t, ctx, pool)

	m := New(pool, 0.5, 3600)
	body := scrape(t, m)

	require.GreaterOrEqual(t, metricValue(t, body, "nova_nodes", `status="active"`), 1.0)
	require.Equal(t, 1.0, metricValue(t, body, "nova_node_draining", drainingID))
	require.Equal(t, 1.0, metricValue(t, body, "nova_node_drain_pending_cids", drainingID))
	require.Equal(t, 0.0, metricValue(t, body, "nova_node_drain_ready", drainingID))
	require.Greater(t, metricValue(t, body, "nova_node_drain_pending_oldest_seconds", drainingID), 0.0)
	require.Equal(t, 2.0, metricValue(t, body, "nova_below_floor_replica_debt", ""))
	require.Equal(t, 1.0, metricValue(t, body, "nova_below_floor_nodes", `state="sustained"`))
	require.Equal(t, 0.0, metricValue(t, body, "nova_below_floor_nodes", `state="in_grace"`))
	require.Contains(t, body, "nova_replication_cids")
	require.Contains(t, body, "nova_reconcile_queue_depth")
	require.Equal(t, 1.0, metricValue(t, body, "nova_audit_results_total", `result="pass"`),
		"durable audit family must reflect pin_audits rows (restart-stable)")
}

func TestLabelDiscipline(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedMetricsFixture(t, ctx, pool)

	m := New(pool, 0.5, 3600)
	// Touch every process-local hook once so their families materialize.
	m.ObserveRegisterFailure("missing_capability")
	m.ObserveTrustTransition("probationary", "trusted", "graduated")
	m.ObserveReputationMove("up")
	m.ObserveAuditLatency(0.05)
	m.ObserveDonorFetch("ok", "none", 0.02)
	m.ObserveEgressRefusal("budget_exhausted")
	m.ObserveSourceSelectionFailure("no_sourceable_holder")
	m.ObserveBelowFloorRequeue(3)

	families, err := m.Registry().Gather()
	require.NoError(t, err)
	require.NotEmpty(t, families)

	denied := map[string]bool{
		"cid": true, "blob": true, "path": true,
		"filename": true, "url": true, "collection": true,
	}
	for _, f := range families {
		for _, metric := range f.GetMetric() {
			for _, l := range metric.GetLabel() {
				require.Falsef(t, denied[l.GetName()],
					"family %s uses denied label %q (unbounded-cardinality guard, D-M7-1)",
					f.GetName(), l.GetName())
			}
		}
	}
}

func TestBindFailureIsFatal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	err = ListenAndServe(context.Background(), ln.Addr().String(), http.NotFoundHandler())
	require.Error(t, err, "binding an occupied port must return the error (startup-fatal contract, D-M7-1)")
	require.Contains(t, err.Error(), "bind")
}
