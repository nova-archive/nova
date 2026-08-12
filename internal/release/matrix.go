package release

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// The generated compatibility matrix (P2-M7.3, D-M7.3-19).
//
// # Why generated
//
// UPGRADING.md's matrix is the first thing an operator reads and the last thing
// anyone updates. Prose that restates the intent, the gate coverage and the
// support window is prose that will disagree with them, and the disagreement
// will be discovered by an operator mid-upgrade rather than by a reviewer.
//
// So the table between the markers is rendered from those sources and checked
// by a gate. Everything OUTSIDE the markers is written by a person, because the
// sequences and the reasoning are not derivable from a data structure.
//
// # The markers
//
// Exactly one BEGIN and one END, in that order. Zero means the document lost
// its generated section and would silently stop being checked; two means the
// generator has to guess which one it owns, and guessing is how a generated
// block ends up overwriting hand-written text.

const (
	MatrixBegin = "<!-- BEGIN GENERATED COMPATIBILITY MATRIX -->"
	MatrixEnd   = "<!-- END GENERATED COMPATIBILITY MATRIX -->"
)

// RenderMatrix produces the generated block's BODY — the marker lines are added
// by Splice, so a caller cannot accidentally emit a block with no end.
//
// Deterministic: every list is sorted and no timestamp appears. A generator
// whose output changes between two runs over the same inputs cannot have a
// drift gate, because every run would be drift.
func RenderMatrix(in Intent, cat Catalog, table []GateCoverage) []byte {
	var b bytes.Buffer

	fmt.Fprintf(&b, "\n### %s\n\n", in.Version)
	fmt.Fprintf(&b, "| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| Product version | `%s` |\n", in.Version)
	fmt.Fprintf(&b, "| Database schema | %d |\n", in.TargetSchema)
	fmt.Fprintf(&b, "| Platforms | %s |\n", joinCode(in.Platforms))
	fmt.Fprintf(&b, "| Support epoch | %s |\n", in.SupportEpoch.UTC().Format("2006-01-02"))

	// The support window, stated as the rule rather than as a date somebody has
	// to recompute. Two predecessors OR six months, whichever is more generous.
	fmt.Fprintf(&b, "| Support window | %d immediate predecessors, **or** %d days since a "+
		"predecessor's declared support epoch — whichever still admits it |\n",
		PredecessorDepth, int(AgeBranchWindow.Hours()/24))

	b.WriteString("\n**Supported predecessors.** An artifact outside this list is *untested*, " +
		"which is not the same as *rejected*: the interop contract is negotiated protocol plus " +
		"capabilities, and a version string was never it.\n\n")
	b.WriteString("| Predecessor | Kind | Schema | Support epoch | Age-branch deadline |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, p := range in.SupportedPredecessors {
		fmt.Fprintf(&b, "| `%s` | %s | %d | %s | %s |\n",
			p.ID(), p.Kind, p.Schema,
			p.SupportEpoch.UTC().Format("2006-01-02"),
			AgeBranchDeadlineFor(p).UTC().Format("2006-01-02"))
	}

	b.WriteString("\n**Capability profile.** Core capabilities are required at registration. " +
		"Everything else gates a ROLE, not admission: a donor missing one is excluded from that " +
		"role and from nothing else.\n\n")
	fmt.Fprintf(&b, "- **core** — %s\n", joinCode(sorted(cat.Profiles.Core)))
	roles := make([]string, 0, len(cat.Profiles.Roles))
	for r := range cat.Profiles.Roles {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	for _, r := range roles {
		fmt.Fprintf(&b, "- **%s** — %s\n", r, joinCode(sorted(cat.Profiles.Roles[r])))
	}

	b.WriteString("\n**Claims this release makes, and the gate that proves each.** A claim " +
		"bound to a gate whose coverage is still a placeholder cannot reach a signed lock.\n\n")
	b.WriteString("| Claim | Gate | Runner | Coverage |\n|---|---|---|---|\n")
	for _, c := range in.Claims {
		cov, ok := CoverageForIn(table, c.ProvenByGate)
		state := "**NO COVERAGE ENTRY**"
		runner := "—"
		if ok {
			runner = string(cov.Runner)
			state = "derived from an executed run"
			if cov.Placeholder {
				state = "**placeholder — blocks a release candidate**"
			}
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | %s |\n", c.ID, c.ProvenByGate, runner, state)
	}

	b.WriteString("\n**What the gates do NOT prove.** Recorded because the absence of an entry " +
		"is not a statement, and a reader who sees only what a gate proves will assume the rest.\n\n")
	gates := make([]GateCoverage, len(table))
	copy(gates, table)
	sort.Slice(gates, func(i, j int) bool { return gates[i].Gate < gates[j].Gate })
	for _, g := range gates {
		fmt.Fprintf(&b, "- `%s`\n", g.Gate)
		for _, l := range g.DoesNotProve {
			fmt.Fprintf(&b, "  - %s\n", l)
		}
	}

	b.WriteString("\n**Rollback.** `migrate down` does not exist. Going back means one of two " +
		"things, and `novactl upgrade status` prints which one before you start:\n\n")
	b.WriteString("- **redeploy the previous binary** — the schema stays forward and the named " +
		"predecessor runs against it;\n")
	b.WriteString("- **restore from backup** — a migration in the range destroyed information, " +
		"or the range is not compatible with the predecessor.\n")

	return b.Bytes()
}

// Splice replaces the generated block in doc, leaving everything else byte
// identical.
func Splice(doc, body []byte) ([]byte, error) {
	begin := bytes.Index(doc, []byte(MatrixBegin))
	end := bytes.Index(doc, []byte(MatrixEnd))

	switch {
	case begin < 0 || end < 0:
		return nil, fmt.Errorf("release: the document is missing %s or %s; without both, the "+
			"generated section silently stops being checked", MatrixBegin, MatrixEnd)
	case bytes.Count(doc, []byte(MatrixBegin)) != 1 || bytes.Count(doc, []byte(MatrixEnd)) != 1:
		return nil, fmt.Errorf("release: %s and %s must each appear exactly once; with two, the "+
			"generator has to guess which block it owns", MatrixBegin, MatrixEnd)
	case end < begin:
		return nil, fmt.Errorf("release: %s appears before %s", MatrixEnd, MatrixBegin)
	}

	var out bytes.Buffer
	out.Write(doc[:begin])
	out.WriteString(MatrixBegin)
	out.WriteString("\n<!-- Generated by `novarel matrix`. Edit the intent or the gate coverage,")
	out.WriteString("\n     not this block: `make release-docs-check` fails on a hand edit. -->\n")
	out.Write(body)
	out.WriteString("\n")
	out.WriteString(MatrixEnd)
	out.Write(doc[end+len(MatrixEnd):])
	return out.Bytes(), nil
}

func sorted(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}

func joinCode(in []string) string {
	if len(in) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(in))
	for _, s := range in {
		parts = append(parts, "`"+s+"`")
	}
	return strings.Join(parts, ", ")
}
