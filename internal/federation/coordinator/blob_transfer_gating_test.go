package coordinator

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// P2-M7.3 D-M7.3-22. Production required {pin-change-log, snapshot,
// blob-transfer} while compat_matrix_test declared the canonical profile
// {pin-change-log, snapshot} and asserted the two sets were disjoint — using a
// profile it built itself. The invariant was therefore never evaluated against
// the wiring it was meant to constrain.

// TestProductionRequiredProfileHonoursTheInvariant evaluates the invariant
// against the PRODUCTION profile, which is the whole point.
func TestProductionRequiredProfileHonoursTheInvariant(t *testing.T) {
	for _, c := range ProductionRequiredCapabilities {
		if slices.Contains(RouteGatedCapabilities, c) {
			t.Errorf("%s is both required at register and route-gated; a capability can only be one", c)
		}
	}
	if !slices.Contains(RouteGatedCapabilities, wire.CapBlobTransfer) {
		t.Errorf("%s must be route-gated: requiring it refuses registration to an older donor "+
			"outright, which D-M7.3-21b forbids", wire.CapBlobTransfer)
	}
	known := []string{
		wire.CapPinChangeLog, wire.CapSnapshot, wire.CapBlobTransfer,
		wire.CapReadSource, wire.CapRepairStream, wire.CapAuditBlockHash,
	}
	for _, c := range slices.Concat(ProductionRequiredCapabilities, RouteGatedCapabilities) {
		if !slices.Contains(known, c) {
			t.Errorf("capability %q is not a wire-known constant", c)
		}
	}
}

// TestProductionWiringUsesTheSharedProfile: an exported profile that
// cmd/coordinator does not consume is decoration. Reading the source is the
// only way to assert this without importing package main.
func TestProductionWiringUsesTheSharedProfile(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "cmd", "coordinator", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "RequiredCapabilities: fedcoord.ProductionRequiredCapabilities") {
		t.Error("cmd/coordinator does not set RequiredCapabilities from the shared profile; " +
			"the invariant above would again be testing a profile nothing uses")
	}
}

// assignmentEntryPoints are the functions that create pin_assignments rows.
// Every one must filter on blob-transfer/v1, or "assignments never target a
// donor that cannot fetch" is simply false.
var assignmentEntryPoints = []string{"AssignPin", "AssignPinWithSource"}

// TestAssignmentEntryPointsAreEnumerated walks this package's AST for functions
// that write pin_assignments and fails if one is not in the covered set. A
// fourth entry point must not be addable silently.
func TestAssignmentEntryPointsAreEnumerated(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	// The generated upsert is the single write path into pin_assignments; any
	// function calling it is an assignment entry point.
	const upsertPrefix = "UpsertPinAssignmentAssign"

	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				calls := false
				ast.Inspect(fn, func(n ast.Node) bool {
					sel, ok := n.(*ast.SelectorExpr)
					if ok && strings.HasPrefix(sel.Sel.Name, upsertPrefix) {
						calls = true
					}
					return true
				})
				if calls && !slices.Contains(assignmentEntryPoints, fn.Name.Name) {
					t.Errorf("%s (%s) creates pin_assignments rows but is not in assignmentEntryPoints; "+
						"add it AND give it a blob-transfer/v1 filter", fn.Name.Name, filepath.Base(path))
				}
			}
		}
	}
}

// TestGuardedEntryPointsCallTheGuard: being enumerated is not the same as being
// filtered.
func TestGuardedEntryPointsCallTheGuard(t *testing.T) {
	body, err := os.ReadFile("assignments.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range assignmentEntryPoints {
		idx := strings.Index(string(body), "func "+name+"(")
		if idx < 0 {
			t.Fatalf("%s not found in assignments.go", name)
		}
		// The guard must run before any write; checking the first few lines of
		// the body is enough to catch it being appended after the upsert.
		head := string(body)[idx:]
		if end := strings.Index(head, "\n}\n"); end > 0 {
			head = head[:end]
		}
		if !strings.Contains(head, "assertDestCanFetch(") {
			t.Errorf("%s does not call assertDestCanFetch", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Database-backed behaviour.
// ---------------------------------------------------------------------------

// seedGatingNode inserts a donor that is eligible in every respect EXCEPT
// possibly the capability under test, so a failure isolates that one predicate.
func seedGatingNode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, caps []string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(ctx, `
		INSERT INTO nodes (id, display_name, federation_cert_fingerprint, nebula_cert_fingerprint,
		                   selected_protocol, capacity_bytes, bandwidth_budget_bytes_per_day,
		                   status, trust_state, advertised_capabilities, effective_capabilities,
		                   source_nebula_addr, last_seen_at, assignment_sync_state, last_free_bytes)
		VALUES ($1::uuid, 'gating', 'fed:'||$1::text, 'neb:'||$1::text,
		        'fed/v1', 1073741824, 1073741824, 'active', 'trusted',
		        $2::text[], $2::text[], '10.0.0.9:9443', now(), 'current', 1073741824)`,
		pgtype.UUID{Bytes: id, Valid: true}, caps)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestAdmissionExcludesDonorsWithoutBlobTransfer covers the INITIAL-PLACEMENT
// path (federation.sql ListAdmissionCandidates ← admission/assigner.go).
func TestAdmissionExcludesDonorsWithoutBlobTransfer(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	q := gen.New(pool)

	canFetch := seedGatingNode(t, ctx, pool, []string{wire.CapReadSource, wire.CapBlobTransfer})
	cannotFetch := seedGatingNode(t, ctx, pool, []string{wire.CapReadSource})

	rows, err := q.ListAdmissionCandidates(ctx, gen.ListAdmissionCandidatesParams{
		MinFreeBytes: pgtype.Int8{Int64: 1, Valid: true}, Lim: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []uuid.UUID
	for _, r := range rows {
		got = append(got, r.NodeID.Bytes)
	}
	if !slices.Contains(got, canFetch) {
		t.Error("a fetch-capable donor was excluded from admission candidates")
	}
	if slices.Contains(got, cannotFetch) {
		t.Error("a donor without blob-transfer/v1 was offered as an admission target; " +
			"it would be assigned work it cannot perform")
	}
}

// TestPlacementExcludesDonorsWithoutBlobTransfer covers the HEALING/REPAIR path
// (replication.sql ListPlacementCandidates).
func TestPlacementExcludesDonorsWithoutBlobTransfer(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	q := gen.New(pool)

	const cid = "bafy-gating-placement"
	seedBlob(t, ctx, pool, cid, 1048576)
	canFetch := seedGatingNode(t, ctx, pool, []string{wire.CapRepairStream, wire.CapBlobTransfer})
	cannotFetch := seedGatingNode(t, ctx, pool, []string{wire.CapRepairStream})

	rows, err := q.ListPlacementCandidates(ctx, gen.ListPlacementCandidatesParams{
		Cid: cid, BelowFloorGraceSecs: 86400,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []uuid.UUID
	for _, r := range rows {
		got = append(got, r.NodeID.Bytes)
	}
	if !slices.Contains(got, canFetch) {
		t.Error("a fetch-capable donor was excluded from placement candidates")
	}
	if slices.Contains(got, cannotFetch) {
		t.Error("a donor without blob-transfer/v1 was offered as a repair destination")
	}
}

// TestAssignPinRefusesDonorsWithoutBlobTransfer covers the DB-direct seam that
// bypasses both candidate queries.
func TestAssignPinRefusesDonorsWithoutBlobTransfer(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	const cid = "bafy-gating-assign"
	seedBlob(t, ctx, pool, cid, 1048576)
	cannotFetch := seedGatingNode(t, ctx, pool, []string{wire.CapReadSource})

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := AssignPin(ctx, tx, cid, cannotFetch); !errors.Is(err, ErrDestCannotFetch) {
		t.Fatalf("AssignPin error = %v, want ErrDestCannotFetch", err)
	}
}

// TestAssignPinAcceptsTheNamedTestOverride: fixtures that predate route-gating
// need a seam, and it must be one grep finds.
func TestAssignPinAcceptsTheNamedTestOverride(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	const cid = "bafy-gating-override"
	seedBlob(t, ctx, pool, cid, 1048576)
	cannotFetch := seedGatingNode(t, ctx, pool, []string{wire.CapReadSource})

	AllowAssignmentToDonorsWithoutBlobTransfer = true
	t.Cleanup(func() { AllowAssignmentToDonorsWithoutBlobTransfer = false })

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := AssignPin(ctx, tx, cid, cannotFetch); err != nil {
		t.Fatalf("the named override did not bypass the guard: %v", err)
	}
}

// TestAckedReplicasPreservedOnDonorsLackingBlobTransfer. Holding existing data
// and accepting new fetch-requiring work are different things — D-M7.3-21b
// requires an older donor to stay a valid holder.
func TestAckedReplicasPreservedOnDonorsLackingBlobTransfer(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	q := gen.New(pool)

	const cid = "bafy-gating-holder"
	seedBlob(t, ctx, pool, cid, 1048576)
	holder := seedGatingNode(t, ctx, pool, []string{wire.CapReadSource, wire.CapBlobTransfer})

	// Assign and ack while the donor could fetch.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AssignPin(ctx, tx, cid, holder); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE pin_assignments SET state = 'acked' WHERE cid = $1 AND node_id = $2::uuid`,
		cid, holder); err != nil {
		t.Fatal(err)
	}

	// The donor rolls back to a build without blob-transfer/v1.
	if _, err := pool.Exec(ctx,
		`UPDATE nodes SET advertised_capabilities = ARRAY['read-source/v1'],
		                 effective_capabilities  = ARRAY['read-source/v1'] WHERE id = $1::uuid`,
		holder); err != nil {
		t.Fatal(err)
	}

	holders, err := q.ListVerifiedHoldersByCID(ctx, cid)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range holders {
		if h.NodeID.Bytes == holder {
			found = true
		}
	}
	if !found {
		t.Error("losing blob-transfer/v1 dropped an acked replica; durability must not " +
			"depend on a donor's ability to accept NEW work")
	}
}
