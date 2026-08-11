package migrations

import (
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestEveryMigrationHasObligations checks BOTH directions. A shipped migration
// with no declared meaning makes the preflight silently incomplete; a declared
// entry with no file means the table has drifted from the embed.
func TestEveryMigrationHasObligations(t *testing.T) {
	shipped, err := fs.Glob(Migrations, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(shipped) == 0 {
		t.Fatal("no migrations found in the embed; this test would pass vacuously")
	}

	for _, f := range shipped {
		if _, err := Lookup(f); err != nil {
			t.Errorf("%s ships but declares no obligations", f)
		}
	}
	for _, o := range All() {
		if !slices.Contains(shipped, o.File) {
			t.Errorf("obligations declared for %s, which is not in the embed", o.File)
		}
		n, err := SchemaOf(o.File)
		if err != nil {
			t.Errorf("%s: %v", o.File, err)
			continue
		}
		if n != o.Schema {
			t.Errorf("%s declares Schema %d but its filename says %d", o.File, o.Schema, n)
		}
	}
}

// TestPartitionsMigrationIsConservative. 0003 drops integrity_audits and
// audit_log and documents the history loss, so RestoreToRevert is unambiguous.
// OldBinaryCompatible stays FALSE until a historical binary test says otherwise:
// the recreated tables look similar, but composite primary keys and historical
// queries may differ, and deriving the flag by inspection is not evidence.
func TestPartitionsMigrationIsConservative(t *testing.T) {
	o, err := Lookup("0003_partitions.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !o.RestoreToRevert {
		t.Error("0003 drops two tables forward; reverting it needs a restore")
	}
	if o.OldBinaryCompatible {
		t.Error("0003.OldBinaryCompatible must stay false until a historical binary test proves otherwise")
	}
	if len(o.Procedures) == 0 {
		t.Error("0003 destroys audit history and must carry an operator procedure")
	}
}

// TestOldBinaryCompatibleNamesItsPredecessor: the flag is not intrinsic to a
// migration. A schema can suit N-1 and not N-2, so a true claim that names no
// predecessor is not a claim about anything.
func TestOldBinaryCompatibleNamesItsPredecessor(t *testing.T) {
	for _, o := range All() {
		if o.OldBinaryCompatible && o.RelativeTo == "" {
			t.Errorf("%s claims OldBinaryCompatible but names no predecessor", o.File)
		}
		// Today every predecessor is the previous schema. RelativeTo is a free
		// string because a release identity ("v0.2.0", a commit) is the eventual
		// form; while it is a schema prefix, it must name one that exists.
		if n, err := strconv.ParseInt(o.RelativeTo, 10, 64); err == nil {
			found := false
			for _, p := range All() {
				if p.Schema == n {
					found = true
				}
			}
			if !found {
				t.Errorf("%s names predecessor schema %d, which no migration produces", o.File, n)
			}
		}
	}
}

// TestBootstrapMigrationIsNotAnUpgradeStep: 0001's obligations are vacuous on a
// fresh install, and a caller must be able to see that rather than inferring it.
func TestBootstrapMigrationIsNotAnUpgradeStep(t *testing.T) {
	o, err := Lookup("0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !o.BootstrapOnly {
		t.Error("0001 must be marked BootstrapOnly")
	}
	if o.RelativeTo != "" {
		t.Errorf("0001 has no predecessor binary, so RelativeTo must be empty, got %q", o.RelativeTo)
	}

	b, err := Between(0, 18)
	if err != nil {
		t.Fatal(err)
	}
	if !b.BootstrapOnly {
		t.Error("a range starting at schema 0 must report BootstrapOnly so a fresh install " +
			"is not blocked by the historical range's obligations")
	}
	b2, err := Between(2, 18)
	if err != nil {
		t.Fatal(err)
	}
	if b2.BootstrapOnly {
		t.Error("an upgrade range that excludes 0001 must not report BootstrapOnly")
	}
}

// TestComposeIsConjunctionAndUnion, not a max over one ordinal.
func TestComposeIsConjunctionAndUnion(t *testing.T) {
	// 0002 is permissive on every axis; 0003 is restrictive on two. Composing
	// them must keep BOTH facts, which a single severity class cannot.
	b, err := Compose([]string{"0002_jobs.sql", "0003_partitions.sql"})
	if err != nil {
		t.Fatal(err)
	}
	if b.OldBinaryCompatible {
		t.Error("conjunction: 0003 is not old-binary compatible, so the range is not")
	}
	if !b.RestoreToRevert {
		t.Error("disjunction: 0003 needs a restore to revert, so the range does")
	}
	if !b.OnlineApplicable {
		t.Error("both members are online-applicable, so the range is")
	}
	if len(b.Procedures) != 1 {
		t.Errorf("procedures = %v, want 0003's single procedure", b.Procedures)
	}

	// An all-permissive range stays permissive — otherwise the composition is
	// just pessimism rather than a computation.
	b2, err := Compose([]string{"0016_node_draining.sql", "0017_partition_provisioning.sql", "0018_below_floor.sql"})
	if err != nil {
		t.Fatal(err)
	}
	if !b2.OldBinaryCompatible || !b2.OnlineApplicable || b2.RequiresMaintenance || b2.RestoreToRevert {
		t.Errorf("all-permissive range composed to %+v", b2)
	}
}

// TestBetweenIsBounded: (applied, target] — applied is already in the database,
// so its obligations are already paid.
func TestBetweenIsBounded(t *testing.T) {
	b, err := Between(17, 18)
	if err != nil {
		t.Fatal(err)
	}
	if b.From != 17 || b.To != 18 {
		t.Errorf("bounds = (%d, %d], want (17, 18]", b.From, b.To)
	}
	// 0009 requires maintenance. A range starting after it must not inherit that.
	b2, err := Between(10, 12)
	if err != nil {
		t.Fatal(err)
	}
	if !b2.RequiresMaintenance {
		t.Error("0012 rewrites pin_assignments, so (10, 12] does require maintenance")
	}
	b3, err := Between(10, 11)
	if err != nil {
		t.Fatal(err)
	}
	if b3.RequiresMaintenance {
		t.Error("(10, 11] is one metadata-only ALTER and must not inherit 0009's or 0012's cost")
	}
}

func TestBetweenRejectsInvalidRange(t *testing.T) {
	if _, err := Between(18, 17); err == nil {
		t.Fatal("a target behind the applied schema must be rejected; the schema is forward-only")
	}
}

func TestEmptyRangeIsIdentity(t *testing.T) {
	b, err := Between(18, 18)
	if err != nil {
		t.Fatal(err)
	}
	if !b.OldBinaryCompatible || !b.OnlineApplicable ||
		b.RequiresMaintenance || b.RestoreToRevert || b.BootstrapOnly || len(b.Procedures) != 0 {
		t.Errorf("nothing is applied, so nothing is owed; got %+v", b)
	}
}

// TestProcedureOrderIsDeterministic: an operator follows these in order, and a
// map-iteration-shaped ordering would reorder them between runs.
func TestProcedureOrderIsDeterministic(t *testing.T) {
	first, err := Between(0, 18)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		again, err := Between(0, 18)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(first.Procedures, again.Procedures) {
			t.Fatalf("procedure order is not stable:\n %v\n %v", first.Procedures, again.Procedures)
		}
	}
	// And they arrive in schema order: 0003's precedes 0009's.
	idx3, idx9 := -1, -1
	for i, p := range first.Procedures {
		if strings.Contains(p, "integrity_audits") {
			idx3 = i
		}
		if strings.Contains(p, "blobs_soft_delete_sweep_idx") {
			idx9 = i
		}
	}
	if idx3 < 0 || idx9 < 0 || idx3 > idx9 {
		t.Errorf("procedures are not in schema order: 0003 at %d, 0009 at %d", idx3, idx9)
	}
}

// TestComposeDeduplicatesProcedures: two migrations owing the same action must
// not tell the operator to do it twice.
func TestComposeDeduplicatesProcedures(t *testing.T) {
	b, err := Compose([]string{"0003_partitions.sql", "0003_partitions.sql"})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Procedures) != 1 {
		t.Errorf("procedures = %v, want one", b.Procedures)
	}
}

// TestOnlineAndMaintenanceAreConsistent is the invariant that keeps the two
// dimensions from contradicting: a migration cannot both need a window and be
// applicable while serving.
func TestOnlineAndMaintenanceAreConsistent(t *testing.T) {
	for _, o := range All() {
		if o.RequiresMaintenance && o.OnlineApplicable {
			t.Errorf("%s claims both RequiresMaintenance and OnlineApplicable", o.File)
		}
	}
}

// TestMaintenanceMigrationsExplainThemselves: a window an operator cannot plan
// for is just an outage.
func TestMaintenanceMigrationsExplainThemselves(t *testing.T) {
	for _, o := range All() {
		if o.RequiresMaintenance && len(o.Procedures) == 0 {
			t.Errorf("%s requires a maintenance window but tells the operator nothing about it", o.File)
		}
		if o.Note == "" {
			t.Errorf("%s declares no Note; a later reader would have to re-guess the reasoning", o.File)
		}
	}
}

func TestMissingSchemaNumberIsRejected(t *testing.T) {
	for _, bad := range []string{"partitions.sql", "abcd_partitions.sql", "0003-partitions.sql"} {
		if _, err := SchemaOf(bad); err == nil {
			t.Errorf("SchemaOf(%q) succeeded; a file with no NNNN_ prefix must be rejected "+
				"rather than silently sorted to zero", bad)
		}
	}
}

func TestLookupRejectsUnknownFile(t *testing.T) {
	if _, err := Lookup("9999_not_a_migration.sql"); err == nil {
		t.Fatal("Lookup of an undeclared file must fail")
	}
}

// TestAllReturnsACopy: the table is the classification of frozen bytes and must
// not be mutable through a read-only accessor.
func TestAllReturnsACopy(t *testing.T) {
	got := All()
	got[0].OldBinaryCompatible = !got[0].OldBinaryCompatible
	if again := All(); again[0].OldBinaryCompatible == got[0].OldBinaryCompatible {
		t.Fatal("All() exposes the underlying table; a caller can rewrite the classification")
	}
}
