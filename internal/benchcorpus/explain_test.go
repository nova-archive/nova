package benchcorpus

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// TestExplainPlans is the deterministic query-plan gate (D-M7-2): no timing,
// just "the expected index serves the keyed lookup". Runs on a small seeded
// fixture inside a transaction with SET LOCAL enable_seqscan = off — on a
// tiny corpus the planner may LEGITIMATELY prefer a seq scan even when the
// index is correct, so this gate proves index AVAILABILITY (the plan CAN use
// the index), not planner cost choice. Accepts Index Scan, Index Only Scan,
// or Bitmap Index Scan on the expected index; never asserts one exact node.
func TestExplainPlans(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	t.Setenv("BENCH_DATABASE_URL", "")
	pool, err := resolveBenchPool(t, ctx)
	require.NoError(t, err)
	st, err := SeedCorpus(ctx, pool, 20_000, 8, 42)
	require.NoError(t, err)
	cid := st.SampleCIDs[len(st.SampleCIDs)/2]

	cases := []struct {
		name string
		sql  string
		args []any
		// anyOf: the keyed lookup must be served by one of these indexes (a
		// query can be legitimately served by more than one keyed index —
		// asserting a single name would make the gate brittle across planner
		// versions and index additions).
		anyOf []string
	}{
		{
			name:  "list_draining_nodes uses nodes_draining_idx",
			sql:   `SELECT id, draining_at FROM nodes WHERE draining_at IS NOT NULL ORDER BY id`,
			anyOf: []string{"nodes_draining_idx"},
		},
		{
			name:  "pin_assignments keyed lookup uses cid_state index",
			sql:   `SELECT node_id FROM pin_assignments WHERE cid = $1 AND state = 'acked'`,
			args:  []any{cid},
			anyOf: []string{"pin_assignments_cid_state_idx", "pin_assignments_pkey"},
		},
		{
			name:  "pin_assignments per-node lookup uses node_state index",
			sql:   `SELECT cid FROM pin_assignments WHERE node_id = (SELECT id FROM nodes LIMIT 1) AND state = 'acked'`,
			anyOf: []string{"pin_assignments_node_state_idx"},
		},
		{
			name:  "blob_blocks keyed lookup uses the primary key",
			sql:   `SELECT block_cid FROM blob_blocks WHERE blob_cid = $1`,
			args:  []any{cid},
			anyOf: []string{"blob_blocks_pkey"},
		},
		{
			name: "pin_audits per-node lookup is index-served",
			sql:  `SELECT count(*) FROM pin_audits WHERE node_id = (SELECT id FROM nodes LIMIT 1) AND result = 'pass'`,
			anyOf: []string{
				"pin_audits_node_result_idx",
				"pin_audits_recent_pass_node_blob_idx", // 0015 partial index also keyed on (node_id) for result='pass'
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := explainJSON(t, ctx, pool, c.sql, c.args...)
			indexes, nodeTypes := collectPlanIndexes(plan)
			for _, want := range c.anyOf {
				for _, got := range indexes {
					if got == want {
						return
					}
				}
			}
			t.Fatalf("expected one of %v in plan (indexes seen: %v; node types: %v)",
				c.anyOf, indexes, nodeTypes)
		})
	}
}

// explainJSON runs EXPLAIN (FORMAT JSON) inside a tx with SET LOCAL
// enable_seqscan = off (index-availability proof; see the file comment).
func explainJSON(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) map[string]any {
	t.Helper()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `SET LOCAL enable_seqscan = off`)
	require.NoError(t, err)

	var raw []byte
	require.NoError(t, tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+sql, args...).Scan(&raw))
	var parsed []map[string]any
	require.NoError(t, json.Unmarshal(raw, &parsed))
	require.NotEmpty(t, parsed)
	plan, ok := parsed[0]["Plan"].(map[string]any)
	require.True(t, ok, "no Plan node in EXPLAIN output")
	return plan
}

// collectPlanIndexes walks the plan tree collecting index names + node types.
func collectPlanIndexes(plan map[string]any) (indexes []string, nodeTypes []string) {
	if nt, ok := plan["Node Type"].(string); ok {
		nodeTypes = append(nodeTypes, nt)
	}
	if idx, ok := plan["Index Name"].(string); ok {
		indexes = append(indexes, idx)
	}
	if kids, ok := plan["Plans"].([]any); ok {
		for _, k := range kids {
			if km, ok := k.(map[string]any); ok {
				ki, kn := collectPlanIndexes(km)
				indexes = append(indexes, ki...)
				nodeTypes = append(nodeTypes, kn...)
			}
		}
	}
	return indexes, nodeTypes
}
