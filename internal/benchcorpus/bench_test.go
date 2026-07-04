package benchcorpus

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/orchestrator"
	"github.com/stretchr/testify/require"
)

// profiles holds the per-path p95 gates (D-M7-2). Lifecycle is explicit:
//
//	BENCH_CALIBRATE=1 make bench-corpus   # reports suggested thresholds
//	                                      # (measured p95 ×2), never fails
//	# → commit the suggested values into profiles["release"]
//	make bench-corpus                     # the enforceable gate
//
// "release" values are MEASURED numbers from the calibration run at the
// release row count (~9.8M blob_blocks); "ci" is a small-corpus regression
// profile (asymptotic shape, generous absolute slack for shared runners).
var profiles = map[string]map[string]time.Duration{
	"ci": {
		"recompute_counts":      150 * time.Millisecond,
		"sourceable_holders":    150 * time.Millisecond,
		"repair_source":         150 * time.Millisecond,
		"placement_candidates":  150 * time.Millisecond,
		"reconcile_batch":       250 * time.Millisecond,
		"audit_select":          500 * time.Millisecond,
		"drain_pending":         2 * time.Second,
		"below_floor":           1 * time.Second,
		"delete_cascade":        500 * time.Millisecond,
		"projection_rebuild_1k": 60 * time.Second,
	},
	// Calibrated 2026-07-03 (BENCH_CALIBRATE=1, 9.8M blob_blocks profile run);
	// values are measured p95 ×2 headroom from the final-verification run.
	"release": {
		"recompute_counts":      150 * time.Millisecond,
		"sourceable_holders":    150 * time.Millisecond,
		"repair_source":         150 * time.Millisecond,
		"placement_candidates":  150 * time.Millisecond,
		"reconcile_batch":       250 * time.Millisecond,
		"audit_select":          500 * time.Millisecond,
		"drain_pending":         2 * time.Second,
		"below_floor":           1 * time.Second,
		"delete_cascade":        500 * time.Millisecond,
		"projection_rebuild_1k": 60 * time.Second,
	},
}

type pathResult struct {
	Name      string  `json:"name"`
	Samples   int     `json:"samples"`
	P50Millis float64 `json:"p50_ms"`
	P95Millis float64 `json:"p95_ms"`
	P99Millis float64 `json:"p99_ms"`
	Threshold float64 `json:"threshold_ms"`
	Suggested float64 `json:"suggested_threshold_ms"` // measured p95 ×2
	Pass      bool    `json:"pass"`
	Explain   string  `json:"explain,omitempty"`
}

type artifact struct {
	Date      string       `json:"date"`
	Profile   string       `json:"profile"`
	Calibrate bool         `json:"calibrate"`
	Rows      int64        `json:"bench_rows"`
	Blobs     int64        `json:"blobs"`
	Donors    int          `json:"donors"`
	Seed      int64        `json:"seed"`
	GoVersion string       `json:"go_version"`
	NumCPU    int          `json:"num_cpu"`
	PGVersion string       `json:"pg_version"`
	Paths     []pathResult `json:"paths"`
	AllPass   bool         `json:"all_pass"`
}

func percentile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q * float64(len(sorted)-1))
	return sorted[i]
}

// repoRoot walks up from the test's working directory to the module root so
// the artifact always lands in <repo>/reports/benchmarks.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "go.mod not found above test dir")
		dir = parent
	}
}

func measure(t *testing.T, n int, f func(i int)) (p50, p95, p99 time.Duration, samples []time.Duration) {
	t.Helper()
	samples = make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		f(i)
		samples = append(samples, time.Since(start))
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a] < sorted[b] })
	return percentile(sorted, 0.50), percentile(sorted, 0.95), percentile(sorted, 0.99), samples
}

func explainOnce(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) string {
	rows, err := pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+sql, args...)
	if err != nil {
		return "explain failed: " + err.Error()
	}
	defer rows.Close()
	out := ""
	for rows.Next() {
		var line string
		if rows.Scan(&line) == nil {
			out += line + "\n"
		}
	}
	return out
}

func TestCorpusBench(t *testing.T) {
	rowsStr := os.Getenv("BENCH_ROWS")
	if rowsStr == "" {
		t.Skip("set BENCH_ROWS to run the corpus bench (D-M7-2)")
	}
	rows, err := strconv.ParseInt(rowsStr, 10, 64)
	require.NoError(t, err)
	profile := os.Getenv("BENCH_PROFILE")
	if profile == "" {
		profile = "ci"
	}
	thresholds, ok := profiles[profile]
	require.Truef(t, ok, "unknown BENCH_PROFILE %q", profile)
	calibrate := os.Getenv("BENCH_CALIBRATE") == "1"

	ctx := context.Background()
	pool, err := resolveBenchPool(t, ctx)
	require.NoError(t, err)

	const donors, seed = 16, 42
	st, err := SeedCorpus(ctx, pool, rows, donors, seed)
	require.NoError(t, err)
	q := gen.New(pool)
	drainingID := pgUUIDFromString(t, st.DrainingNodeID)
	cidAt := func(i int) string { return st.SampleCIDs[i%len(st.SampleCIDs)] }

	// Sacrificial blobs for the delete-cascade path (never in SampleCIDs' rotation
	// risk: use the tail of the corpus).
	const deleteN = 50
	require.Greater(t, st.Blobs, int64(deleteN*2), "corpus too small for the delete path")

	art := artifact{
		Date: time.Now().Format("2006-01-02"), Profile: profile, Calibrate: calibrate,
		Rows: rows, Blobs: st.Blobs, Donors: donors, Seed: seed,
		GoVersion: runtime.Version(), NumCPU: runtime.NumCPU(), AllPass: true,
	}
	require.NoError(t, pool.QueryRow(ctx, `SELECT version()`).Scan(&art.PGVersion))

	type benchPath struct {
		name    string
		n       int
		run     func(i int)
		explain func() string
	}
	paths := []benchPath{
		{"recompute_counts", 200, func(i int) {
			_, err := q.RecomputeReplicationCounts(ctx, cidAt(i))
			require.NoError(t, err)
		}, func() string {
			return explainOnce(ctx, pool, `SELECT count(*) FROM pin_assignments pa JOIN nodes n ON n.id = pa.node_id WHERE pa.cid = $1`, cidAt(0))
		}},
		{"sourceable_holders", 200, func(i int) {
			_, err := q.ListSourceableHolders(ctx, gen.ListSourceableHoldersParams{Cid: cidAt(i), StaleSecs: 3600})
			require.NoError(t, err)
		}, func() string {
			return explainOnce(ctx, pool, `SELECT n.id FROM pin_assignments pa JOIN nodes n ON n.id = pa.node_id WHERE pa.cid = $1 AND pa.state = 'acked'`, cidAt(0))
		}},
		{"repair_source", 200, func(i int) {
			if _, err := q.ListRepairSourceHolders(ctx, gen.ListRepairSourceHoldersParams{
				Cid: cidAt(i), Size: pgtype.Int8{Int64: 100, Valid: true},
			}); err != nil && err != pgx.ErrNoRows {
				t.Fatal(err)
			}
		}, nil},
		{"placement_candidates", 200, func(i int) {
			_, err := q.ListPlacementCandidates(ctx, cidAt(i))
			require.NoError(t, err)
		}, func() string {
			return explainOnce(ctx, pool, `SELECT n.id FROM nodes n WHERE n.status = 'active' AND NOT EXISTS (SELECT 1 FROM pin_assignments pa WHERE pa.cid = $1 AND pa.node_id = n.id)`, cidAt(0))
		}},
		{"reconcile_batch", 200, func(i int) {
			batch, err := q.ListReconcileBatch(ctx, 500)
			require.NoError(t, err)
			if len(batch) > 0 && i%10 == 0 {
				require.NoError(t, q.DeleteReconciled(ctx, batch[0]))
			}
		}, func() string {
			return explainOnce(ctx, pool, `SELECT cid FROM blob_replication_reconcile_queue ORDER BY enqueued_at LIMIT 500`)
		}},
		{"audit_select", 100, func(i int) {
			nodes, err := q.SelectDueAuditNodes(ctx, 32)
			require.NoError(t, err)
			require.NotEmpty(t, nodes)
			pin, err := q.SelectAckedPinForAudit(ctx, nodes[i%len(nodes)].NodeID)
			if err == pgx.ErrNoRows {
				return
			}
			require.NoError(t, err)
			if _, err := q.SelectRandomBlockForCID(ctx, gen.SelectRandomBlockForCIDParams{
				BlobCid: pin.Cid, BlockSize: 262144,
			}); err != nil && err != pgx.ErrNoRows {
				t.Fatal(err)
			}
		}, nil},
		{"drain_pending", 20, func(i int) {
			_, err := q.CountDrainPendingCIDs(ctx, drainingID)
			require.NoError(t, err)
		}, nil},
		{"below_floor", 20, func(i int) {
			_, err := q.CountBelowFloorReplicas(ctx, 0.5)
			require.NoError(t, err)
		}, nil},
		{"delete_cascade", deleteN, func(i int) {
			_, err := pool.Exec(ctx, `DELETE FROM blobs WHERE cid = $1`, benchCID(st.Blobs-1-int64(i)))
			require.NoError(t, err)
		}, nil},
		{"projection_rebuild_1k", 1, func(i int) {
			targets := orchestrator.ReplicationTargets{Important: 5, Normal: 3, Cache: 2}
			tx, err := pool.Begin(ctx)
			require.NoError(t, err)
			for j := 0; j < 1000; j++ {
				require.NoError(t, orchestrator.RecomputeCID(ctx, tx, cidAt(j), targets))
			}
			require.NoError(t, tx.Commit(ctx))
		}, nil},
	}

	for _, p := range paths {
		p50, p95, p99, _ := measure(t, p.n, p.run)
		res := pathResult{
			Name: p.name, Samples: p.n,
			P50Millis: float64(p50.Microseconds()) / 1000,
			P95Millis: float64(p95.Microseconds()) / 1000,
			P99Millis: float64(p99.Microseconds()) / 1000,
			Threshold: float64(thresholds[p.name].Microseconds()) / 1000,
			Suggested: 2 * float64(p95.Microseconds()) / 1000,
			Pass:      p95 <= thresholds[p.name],
		}
		if p.explain != nil {
			res.Explain = p.explain()
		}
		if !res.Pass {
			art.AllPass = false
		}
		art.Paths = append(art.Paths, res)
		t.Logf("%-24s p50=%8.2fms p95=%8.2fms p99=%8.2fms (gate %8.2fms, pass=%v)",
			res.Name, res.P50Millis, res.P95Millis, res.P99Millis, res.Threshold, res.Pass)
	}

	writeArtifact(t, art)

	if calibrate {
		t.Log("BENCH_CALIBRATE=1: reporting only — commit the suggested thresholds into profiles[\"release\"], then run the plain gate")
		return
	}
	require.True(t, art.AllPass, "one or more bench paths exceeded the %s profile p95 gates — see the artifact", profile)
}

func writeArtifact(t *testing.T, art artifact) {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "reports", "benchmarks")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	base := fmt.Sprintf("p2-m7-corpus-%s", art.Date)

	j, err := json.MarshalIndent(art, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, base+".json"), j, 0o644))

	md := fmt.Sprintf("# P2-M7 corpus bench — %s\n\n- profile: `%s` (calibrate=%v)\n- rows (blob_blocks): %d, blobs: %d, donors: %d, seed: %d\n- go: %s, cpus: %d\n- postgres: %s\n\n| path | samples | p50 ms | p95 ms | p99 ms | gate ms | pass |\n|---|---|---|---|---|---|---|\n",
		art.Date, art.Profile, art.Calibrate, art.Rows, art.Blobs, art.Donors, art.Seed,
		art.GoVersion, art.NumCPU, art.PGVersion)
	for _, p := range art.Paths {
		md += fmt.Sprintf("| %s | %d | %.2f | %.2f | %.2f | %.2f | %v |\n",
			p.Name, p.Samples, p.P50Millis, p.P95Millis, p.P99Millis, p.Threshold, p.Pass)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, base+".md"), []byte(md), 0o644))
	t.Logf("artifact written: %s", filepath.Join(dir, base+".{json,md}"))
}

func pgUUIDFromString(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	require.NoError(t, id.Scan(s))
	return id
}
