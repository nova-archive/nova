package migrations

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Migration obligations (P2-M7.3, D-M7.3-8).
//
// `MANIFEST.sha256` freezes the BYTES of every shipped migration. Nothing
// recorded their MEANING, so `migrate up` was an undifferentiated sweep and
// neither an operator nor a preflight could answer §23's three questions: what
// order do I upgrade, how do I prove it worked, and exactly how do I go back.
//
// # Why six independent dimensions and not one class
//
// A single ordinal severity with strictest-wins cannot express what
// 0003_partitions.sql actually is: it DROPS two tables (so reverting needs a
// restore) while the tables it recreates may still serve an older binary. Those
// are independent facts about one migration, and collapsing them loses the one
// an operator needs. So the dimensions compose separately across a range —
// conjunction of the permissive flags, disjunction of the restrictive ones,
// union of the procedures.
//
// # OldBinaryCompatible is not intrinsic
//
// It is compatibility with a NAMED predecessor. A schema can suit N-1 and not
// N-2, so the flag carries the predecessor it is relative to, and version-pair
// assertions live in release scenarios rather than here.
//
// # Why this lives outside the SQL
//
// Annotating 0001-0018 in place would rewrite frozen bytes and force a
// MANIFEST.sha256 regeneration, which would establish that `migrations-frozen`
// is negotiable. The table is a separate, unexported Go value exposed through
// read-only lookups.

// Obligations records what applying one migration commits the operator to.
type Obligations struct {
	// File is the migration's filename, e.g. "0003_partitions.sql".
	File string
	// Schema is the numeric prefix — the goose version this migration produces.
	Schema int64

	// OldBinaryCompatible reports whether RelativeTo's binary can run against
	// the schema this migration produces. This is what makes a coordinator
	// rollback survivable: the schema stays forward, and the previous binary
	// must tolerate it. False is the conservative answer.
	OldBinaryCompatible bool
	// RelativeTo names the predecessor OldBinaryCompatible is about. Empty when
	// the flag is meaningless (a bootstrap migration has no predecessor).
	RelativeTo string

	// OnlineApplicable reports whether the migration can be applied while the
	// coordinator keeps serving. False means it takes a lock, rewrites a table,
	// or backfills at corpus scale.
	OnlineApplicable bool
	// RequiresMaintenance reports whether the operator must schedule a window.
	// Implies !OnlineApplicable; the two are checked for consistency.
	RequiresMaintenance bool

	// RestoreToRevert reports whether undoing this migration requires restoring
	// from backup because going FORWARD destroyed information, or because the
	// down migration would destroy information the old binary depends on.
	//
	// Purely additive migrations are false: the old binary does not know the
	// new column exists, so dropping it loses nothing that binary had.
	RestoreToRevert bool

	// BootstrapOnly marks a migration that only ever runs on an empty database.
	// Its obligations are vacuous on a fresh install — there is no old binary
	// and nothing to revert to — and callers must not let it block one.
	BootstrapOnly bool

	// Procedures are operator actions this migration requires, in the order
	// they must be performed. Empty for most.
	Procedures []string

	// Note records the reasoning where a dimension is arguable, so a later
	// reader can revise it with evidence rather than by re-guessing.
	Note string
}

// Boundary is the composition of a range's obligations.
type Boundary struct {
	// From and To are the exclusive-inclusive bounds: (From, To].
	From, To int64

	// OldBinaryCompatible is the CONJUNCTION: the whole range is old-binary
	// compatible only if every member is.
	OldBinaryCompatible bool
	// RelativeTo is the union of the predecessors the members' flags are
	// relative to, in range order. More than one entry means the composed
	// claim spans predecessors and is only as strong as its weakest member.
	RelativeTo []string

	// OnlineApplicable is the conjunction.
	OnlineApplicable bool
	// RequiresMaintenance and RestoreToRevert are DISJUNCTIONS: one member is
	// enough to bind the whole range.
	RequiresMaintenance bool
	RestoreToRevert     bool

	// BootstrapOnly reports whether the range CONTAINS a bootstrap-only
	// migration — i.e. this is a fresh install, not an upgrade.
	BootstrapOnly bool

	// Procedures is the union, deduplicated, in range order.
	Procedures []string
}

// identityBoundary is what an empty range composes to: nothing is applied, so
// nothing is owed.
func identityBoundary(from, to int64) Boundary {
	return Boundary{From: from, To: to, OldBinaryCompatible: true, OnlineApplicable: true}
}

// table is the classification. It is unexported: callers read it through
// Lookup, Between and Compose, and cannot mutate it.
//
// Every judgement below was made by reading the migration. Where a dimension is
// arguable the conservative value is chosen and Note says why, so revising it
// later requires evidence rather than a fresh opinion.
var table = []Obligations{
	{
		File: "0001_init.sql", Schema: 1,
		BootstrapOnly: true, OnlineApplicable: true, RestoreToRevert: true,
		OldBinaryCompatible: false, RelativeTo: "",
		Note: "The bootstrap schema. OldBinaryCompatible is meaningless — there is no " +
			"predecessor binary — so it is false with no named predecessor. RestoreToRevert " +
			"because reverting it means an empty database.",
	},
	{
		File: "0002_jobs.sql", Schema: 2,
		OldBinaryCompatible: true, RelativeTo: "0001", OnlineApplicable: true,
		Note: "Additive: a new partitioned jobs table and its enum. A 0001-era binary does " +
			"not know jobs exists.",
	},
	{
		File: "0003_partitions.sql", Schema: 3,
		OldBinaryCompatible: false, RelativeTo: "0002",
		OnlineApplicable: true, RestoreToRevert: true,
		Procedures: []string{
			"Back up the database. This migration DROPS integrity_audits and audit_log; " +
				"their history is not recoverable afterwards.",
		},
		Note: "The example the composable model exists for. Forward it drops two tables and " +
			"recreates them partitioned, so reverting needs a restore. OldBinaryCompatible is " +
			"CONSERVATIVELY FALSE pending a historical binary test: the recreated tables look " +
			"similar, but composite primary keys and historical queries may differ, and " +
			"deriving the flag by inspection is not evidence (D-M7.3-8, open item).",
	},
	{
		File: "0004_envelope_version.sql", Schema: 4,
		OldBinaryCompatible: true, RelativeTo: "0003", OnlineApplicable: true,
		Note: "ADD COLUMN with a constant default: metadata-only in Postgres 11+, no rewrite.",
	},
	{
		File: "0005_upload_sessions.sql", Schema: 5,
		OldBinaryCompatible: true, RelativeTo: "0004", OnlineApplicable: true,
		Note: "Additive table, enum and trigger.",
	},
	{
		File: "0006_auth.sql", Schema: 6,
		OldBinaryCompatible: true, RelativeTo: "0005", OnlineApplicable: true,
		Note: "Two nullable/defaulted columns on users plus a new refresh_tokens table.",
	},
	{
		File: "0007_refresh_token_gc_index.sql", Schema: 7,
		OldBinaryCompatible: true, RelativeTo: "0006", OnlineApplicable: true,
		Note: "One index on refresh_tokens. Not CONCURRENTLY, but the table is bounded by " +
			"active sessions rather than by the corpus.",
	},
	{
		File: "0008_moderation.sql", Schema: 8,
		OldBinaryCompatible: true, RelativeTo: "0007", OnlineApplicable: true,
		Note: "Additive blocklist table.",
	},
	{
		File: "0009_blob_soft_delete.sql", Schema: 9,
		OldBinaryCompatible: true, RelativeTo: "0008",
		OnlineApplicable: false, RequiresMaintenance: true,
		Procedures: []string{
			"Expect writes to blobs to block while blobs_soft_delete_sweep_idx builds; " +
				"the index is not created CONCURRENTLY.",
		},
		Note: "The column is nullable and additive, but the partial index is built on blobs, " +
			"which grows with the corpus, and CREATE INDEX without CONCURRENTLY holds a lock " +
			"that blocks writes for its duration.",
	},
	{
		File: "0010_upload_tokens.sql", Schema: 10,
		OldBinaryCompatible: true, RelativeTo: "0009", OnlineApplicable: true,
		Note: "New table plus a nullable FK column on upload_sessions. Validation is trivial " +
			"because every existing row is NULL.",
	},
	{
		File: "0011_node_registration.sql", Schema: 11,
		OldBinaryCompatible: true, RelativeTo: "0010", OnlineApplicable: true,
		Note: "Ten columns on nodes, every one nullable or constant-defaulted, so no rewrite. " +
			"nodes is donor-scale, not corpus-scale.",
	},
	{
		File: "0012_assignment_sync.sql", Schema: 12,
		OldBinaryCompatible: true, RelativeTo: "0011",
		OnlineApplicable: false, RequiresMaintenance: true,
		Procedures: []string{
			"Expect a full rewrite of pin_assignments: assignment_id defaults to " +
				"gen_random_uuid(), which is VOLATILE, so Postgres cannot add it as metadata only.",
		},
		Note: "Additive to an old binary — it neither writes pin_changes nor reads " +
			"assignment_id — but the volatile default forces an ACCESS EXCLUSIVE rewrite " +
			"proportional to the assignment count.",
	},
	{
		File: "0013_storage_read_redirect.sql", Schema: 13,
		OldBinaryCompatible: true, RelativeTo: "0012",
		OnlineApplicable: false, RequiresMaintenance: true, RestoreToRevert: true,
		Procedures: []string{
			"Expect a backfill proportional to the blob count: blob_storage_state is " +
				"populated from blobs, and pin_changes.byte_size is rewritten from the manifests.",
		},
		Note: "RestoreToRevert because the UPDATE overwrites existing pin_changes.byte_size " +
			"values in place and the down migration does not restore them — a forward data " +
			"mutation, not an additive one.",
	},
	{
		File: "0014_liveness_healing.sql", Schema: 14,
		OldBinaryCompatible: true, RelativeTo: "0013",
		OnlineApplicable: false, RequiresMaintenance: true,
		Procedures: []string{
			"Expect a backfill proportional to the blob count while blob_replication_state " +
				"is populated.",
		},
		Note: "All column additions are nullable or constant-defaulted; the cost is the " +
			"seed INSERT ... SELECT over blobs.",
	},
	{
		File: "0015_possession_audits.sql", Schema: 15,
		OldBinaryCompatible: true, RelativeTo: "0014",
		OnlineApplicable: false, RequiresMaintenance: true,
		Procedures: []string{
			"Expect writes to pin_assignments and pin_audits to block while their new " +
				"indexes build; neither is created CONCURRENTLY.",
		},
		Note: "The nodes backfill (trust_epoch_started_at = joined_at) writes a column the " +
			"old binary does not know, so it is additive; the indexes are the cost.",
	},
	{
		File: "0016_node_draining.sql", Schema: 16,
		OldBinaryCompatible: true, RelativeTo: "0015", OnlineApplicable: true,
		Note: "One nullable column and one partial index on nodes, which is donor-scale.",
	},
	{
		File: "0017_partition_provisioning.sql", Schema: 17,
		OldBinaryCompatible: true, RelativeTo: "0016", OnlineApplicable: true,
		Note: "Provisions the current and next two months of partitions with CREATE TABLE IF " +
			"NOT EXISTS. Purely additive; its down is deliberately a no-op because the " +
			"partitions are data-bearing and 0002/0003's downs drop the parents with them.",
	},
	{
		File: "0018_below_floor.sql", Schema: 18,
		OldBinaryCompatible: true, RelativeTo: "0017", OnlineApplicable: true,
		Note: "One nullable column and one index on nodes.",
	},
	{
		File: "0019_upgrade_runs.sql", Schema: 19,
		OldBinaryCompatible: true, RelativeTo: "0018", OnlineApplicable: true,
		Note: "P2-M7.3. Two new tables (upgrade_runs, upgrade_events) and ten nullable columns " +
			"on nodes, plus a backfill of effective_capabilities from advertised_capabilities " +
			"that touches only donor-scale rows. The baseline coordinator neither reads nor " +
			"writes any of it, which is what makes a coordinator rollback across this " +
			"boundary survivable — and this range must stay auto-appliable, or every " +
			"existing deployment stops at a runbook to cross it.",
	},
}

// Lookup returns the obligations declared for a migration filename.
func Lookup(file string) (Obligations, error) {
	for _, o := range table {
		if o.File == file {
			return o, nil
		}
	}
	return Obligations{}, fmt.Errorf("migrations: no obligations declared for %q", file)
}

// All returns every declared migration in schema order. The slice is a copy;
// the table itself is not writable by callers.
func All() []Obligations {
	out := make([]Obligations, len(table))
	copy(out, table)
	slices.SortFunc(out, func(a, b Obligations) int { return int(a.Schema - b.Schema) })
	return out
}

// SchemaOf parses the numeric prefix of a migration filename. A file without
// one is rejected rather than silently sorted to zero.
func SchemaOf(file string) (int64, error) {
	prefix, _, found := strings.Cut(file, "_")
	if !found {
		return 0, fmt.Errorf("migrations: %q has no NNNN_ schema prefix", file)
	}
	n, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("migrations: %q has no numeric schema prefix: %w", file, err)
	}
	return n, nil
}

// Between composes the obligations of the range (applied, target].
//
// The range is half-open on the left because `applied` is already in the
// database: crossing from 18 to 19 owes what 0019 owes, not what 0018 owed.
//
// A fresh install is (0, N], which necessarily contains the bootstrap
// migration. Callers must treat that case explicitly — see Boundary.
// BootstrapOnly — rather than letting the historical range's obligations block
// a deployment that has no old binary and nothing to revert to.
func Between(applied, target int64) (Boundary, error) {
	if target < applied {
		return Boundary{}, fmt.Errorf("migrations: target schema %d is behind applied schema %d; "+
			"the schema is forward-only and rollback is restore-from-backup", target, applied)
	}
	var files []string
	for _, o := range All() {
		if o.Schema > applied && o.Schema <= target {
			files = append(files, o.File)
		}
	}
	b, err := Compose(files)
	if err != nil {
		return Boundary{}, err
	}
	b.From, b.To = applied, target
	return b, nil
}

// Compose folds a set of migrations into one boundary. Permissive dimensions
// are conjunctions, restrictive ones disjunctions, procedures a deduplicated
// union in schema order.
func Compose(files []string) (Boundary, error) {
	if len(files) == 0 {
		return identityBoundary(0, 0), nil
	}

	obs := make([]Obligations, 0, len(files))
	for _, f := range files {
		o, err := Lookup(f)
		if err != nil {
			return Boundary{}, err
		}
		obs = append(obs, o)
	}
	slices.SortFunc(obs, func(a, b Obligations) int { return int(a.Schema - b.Schema) })

	out := Boundary{
		From:                obs[0].Schema - 1,
		To:                  obs[len(obs)-1].Schema,
		OldBinaryCompatible: true,
		OnlineApplicable:    true,
	}
	for _, o := range obs {
		out.OldBinaryCompatible = out.OldBinaryCompatible && o.OldBinaryCompatible
		out.OnlineApplicable = out.OnlineApplicable && o.OnlineApplicable
		out.RequiresMaintenance = out.RequiresMaintenance || o.RequiresMaintenance
		out.RestoreToRevert = out.RestoreToRevert || o.RestoreToRevert
		out.BootstrapOnly = out.BootstrapOnly || o.BootstrapOnly
		if o.RelativeTo != "" && !slices.Contains(out.RelativeTo, o.RelativeTo) {
			out.RelativeTo = append(out.RelativeTo, o.RelativeTo)
		}
		for _, p := range o.Procedures {
			if !slices.Contains(out.Procedures, p) {
				out.Procedures = append(out.Procedures, p)
			}
		}
	}
	return out, nil
}
