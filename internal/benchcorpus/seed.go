// Package benchcorpus is the P2-M7 (D-M7-2) corpus-scale benchmark: a LOCAL
// milestone-exit gate (BENCH_PROFILE=release at ~9.8M blob_blocks) plus a small
// CI regression profile. SAFETY: scratch DB only — an external DSN is refused
// unless its database name contains "scratch" or BENCH_ALLOW_NON_SCRATCH=1.
// Artifacts are lightweight JSON/MD reports, never DB dumps.
package benchcorpus

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/dbtest"
)

// checkScratchDSN refuses an external DSN whose database name does not look
// scratch, unless the explicit override is set. The bench floods tables — it
// must never run against a production database by a pasted-DSN accident.
func checkScratchDSN(dsn string, allow bool) error {
	if allow {
		return nil
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("benchcorpus: parse BENCH_DATABASE_URL: %w", err)
	}
	if db := cfg.ConnConfig.Database; !strings.Contains(db, "scratch") {
		return fmt.Errorf("benchcorpus: refusing non-scratch database %q — the bench floods tables; use a dbname containing \"scratch\" or set BENCH_ALLOW_NON_SCRATCH=1", db)
	}
	return nil
}

// resolveBenchPool returns the bench pool: BENCH_DATABASE_URL (scratch-guarded,
// schema assumed migrated) or a fresh testcontainers DB (inherently scratch,
// migrations applied by dbtest).
func resolveBenchPool(t *testing.T, ctx context.Context) (*pgxpool.Pool, error) {
	dsn := os.Getenv("BENCH_DATABASE_URL")
	if dsn == "" {
		return dbtest.New(t, ctx), nil
	}
	if err := checkScratchDSN(dsn, os.Getenv("BENCH_ALLOW_NON_SCRATCH") == "1"); err != nil {
		return nil, err
	}
	return pgxpool.New(ctx, dsn)
}

// SeedStats reports what SeedCorpus built, for the bench loops.
type SeedStats struct {
	Blobs          int64
	Blocks         int64
	Donors         int
	DrainingNodeID string // one donor is seeded draining (drain-debt paths)
	NodeIDs        []string
	SampleCIDs     []string // evenly-spaced blob CIDs for random-key iteration
}

const blocksPerBlob = 8

// SeedCorpus seeds a synthetic corpus sized by blob_blocks rows: nodes with
// mixed reputation/trust (one draining, one below-floor), blobs + manifests +
// blocks, Zipf-skewed pin_assignments (hubs, not uniform — s=1.2), per-blob
// blob_replication_state, a partially-filled reconcile queue, and a sprinkle
// of pin_audits. Deterministic for a given seed. Batches stay ≤ 10k rows.
func SeedCorpus(ctx context.Context, pool *pgxpool.Pool, rows int64, donors int, seed int64) (SeedStats, error) {
	var st SeedStats
	if donors < 2 {
		return st, fmt.Errorf("benchcorpus: need ≥ 2 donors, got %d", donors)
	}
	numBlobs := rows / blocksPerBlob
	if numBlobs < 1 {
		numBlobs = 1
	}
	rng := rand.New(rand.NewPCG(uint64(seed), 0x6e6f7661))
	zipf := rand.NewZipf(rng, 1.2, 1, uint64(donors-1))

	// Nodes: active/current, full caps, fresh; reputation spread 0.2..1.0 so a
	// below-floor donor exists; donor 0 draining; trust mixed.
	st.Donors = donors
	for i := 0; i < donors; i++ {
		id := fmt.Sprintf("beeeeeee-0000-4000-8000-%012d", i)
		st.NodeIDs = append(st.NodeIDs, id)
		rep := 0.2 + 0.8*float64(i)/float64(donors-1)
		trust := "trusted"
		if i%3 == 0 {
			trust = "probationary"
		}
		draining := ""
		if i == 0 {
			draining = "now()"
			st.DrainingNodeID = id
		} else {
			draining = "NULL"
		}
		if _, err := pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO nodes (id, nebula_cert_fingerprint, federation_cert_fingerprint,
			                   capacity_bytes, bandwidth_budget_bytes_per_day, policy_filters,
			                   status, assignment_sync_state, trust_state, reputation_score,
			                   advertised_capabilities, source_nebula_addr, last_seen_at,
			                   last_stored_bytes, last_egress_remaining_bytes, draining_at)
			VALUES ($1::uuid, $2, $3, 1099511627776, 1099511627776, '{}',
			        'active', 'current', $4, $5,
			        '{read-source/v1,repair-stream/v1,audit-block-hash/v1,pin-change-log/v1,snapshot/v1}',
			        $6, now(), $7, 1099511627776, %s)`, draining),
			id, id+"-nfp", id+"-ffp", trust, rep,
			fmt.Sprintf("10.42.0.%d:9443", 10+i), int64(1<<30)*int64(i+1)); err != nil {
			return st, fmt.Errorf("seed nodes: %w", err)
		}
	}

	// Bulk tables via CopyFrom in ≤10k-row batches.
	const batch = 10_000
	classes := []string{"important", "normal", "cache"}
	targets := map[string]int{"important": 5, "normal": 3, "cache": 2}
	tiers := []string{"healthy", "tier2", "tier1", "donor_lost"}

	for lo := int64(0); lo < numBlobs; lo += batch {
		hi := min(lo+batch, numBlobs)
		n := int(hi - lo)
		blobRows := make([][]any, 0, n)
		manifestRows := make([][]any, 0, n)
		blockRows := make([][]any, 0, n*blocksPerBlob)
		pinRows := make([][]any, 0, n*2)
		brsRows := make([][]any, 0, n)

		for i := lo; i < hi; i++ {
			cid := benchCID(i)
			class := classes[int(i)%len(classes)]
			envSize := int64(blocksPerBlob * 262144)
			blobRows = append(blobRows, []any{cid, "image/jpeg", envSize, "active", "image", 2})
			manifestRows = append(manifestRows, []any{cid, "sha2-256", "dag-pb", "size-262144", envSize, envSize, blocksPerBlob})
			for b := 0; b < blocksPerBlob; b++ {
				blockRows = append(blockRows, []any{cid, fmt.Sprintf("%s-blk-%d", cid, b), b, 262144})
			}
			// Zipf-skewed primary holder; ~50% of blobs get a second replica on
			// the next donor (distinct by construction).
			primary := int(zipf.Uint64())
			pinRows = append(pinRows, []any{cid, st.NodeIDs[primary], "acked"})
			healthy := 1
			if i%2 == 0 {
				second := (primary + 1) % donors
				pinRows = append(pinRows, []any{cid, st.NodeIDs[second], "acked"})
				healthy = 2
			}
			tier := tiers[int(i)%len(tiers)]
			brsRows = append(brsRows, []any{cid, healthy, healthy, 0, targets[class], tier, false, class, false})
		}

		type bulk struct {
			table pgx.Identifier
			cols  []string
			rows  [][]any
		}
		for _, b := range []bulk{
			{pgx.Identifier{"blobs"}, []string{"cid", "mime_type", "byte_size", "state", "product", "envelope_version"}, blobRows},
			{pgx.Identifier{"blob_manifests"}, []string{"cid", "hash_alg", "codec", "chunker", "plaintext_size", "envelope_size", "block_count"}, manifestRows},
			{pgx.Identifier{"blob_blocks"}, []string{"blob_cid", "block_cid", "block_index", "block_size"}, blockRows},
			{pgx.Identifier{"pin_assignments"}, []string{"cid", "node_id", "state"}, pinRows},
			{pgx.Identifier{"blob_replication_state"}, []string{"cid", "healthy_acked_count", "sourceable_acked_count", "in_flight_count", "target_count", "safety_tier", "local_recoverable", "durability_class", "dirty"}, brsRows},
		} {
			if _, err := pool.CopyFrom(ctx, b.table, b.cols, pgx.CopyFromRows(b.rows)); err != nil {
				return st, fmt.Errorf("seed %s: %w", b.table.Sanitize(), err)
			}
		}
	}

	// Partially-filled reconcile queue (every 20th blob) + a sprinkle of
	// decided audits (every 50th blob, on the primary holder's row).
	if _, err := pool.Exec(ctx, `
		INSERT INTO blob_replication_reconcile_queue (cid, reason)
		SELECT cid, 'node_unreachable' FROM blobs WHERE right(cid, 1) = '0' AND right(cid, 2) <> '00'
		ON CONFLICT (cid) DO NOTHING`); err != nil {
		return st, fmt.Errorf("seed queue: %w", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO pin_audits (blob_cid, node_id, challenge_kind, nonce, deadline, result, decided_at)
		SELECT pa.cid, pa.node_id, 'block_hash', 'bench', now(), 'pass'::audit_result, now()
		FROM pin_assignments pa WHERE right(pa.cid, 2) = '50'`); err != nil {
		return st, fmt.Errorf("seed audits: %w", err)
	}

	st.Blobs = numBlobs
	st.Blocks = numBlobs * blocksPerBlob
	step := numBlobs / 256
	if step < 1 {
		step = 1
	}
	for i := int64(0); i < numBlobs; i += step {
		st.SampleCIDs = append(st.SampleCIDs, benchCID(i))
	}
	return st, nil
}

func benchCID(i int64) string { return fmt.Sprintf("bench-cid-%09d", i) }
