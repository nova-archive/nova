package release

import (
	"bytes"
	"fmt"
	"sort"
)

// The approval summary (P2-M7.3, amendment).
//
// # What a reviewer is actually being asked
//
// The `release` environment's approval is the one human decision in the whole
// pipeline, and by default GitHub shows the reviewer a job name and a commit.
// Neither answers the question they are approving: *are these the artifacts,
// and is there evidence behind every claim?*
//
// So the lock job renders this into the run summary before the gate. It is
// deliberately the same data the lock carries — digests, descriptors, claims,
// evidence — rather than a prose recap, because a recap can be right about a
// document that is wrong.
//
// It is rendered from the FINALIZED lock, after signing and after the signature
// has been verified with the operator-facing policy. A summary produced from
// anything earlier would describe a document that could still change.

// RenderApprovalSummary produces the Markdown a reviewer reads at the approval
// gate.
func RenderApprovalSummary(l Lock, lockDigest string, evidence []EvidenceStatement) []byte {
	var b bytes.Buffer

	fmt.Fprintf(&b, "## Release candidate `%s`\n\n", l.Version)
	b.WriteString("| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| Lock digest | `%s` |\n", lockDigest)
	fmt.Fprintf(&b, "| Intent digest | `%s` |\n", l.IntentDigest)
	fmt.Fprintf(&b, "| Donor-lock digest | `%s` |\n", l.DonorLockDigest)
	fmt.Fprintf(&b, "| Source commit | `%s` |\n", l.SourceCommit)
	fmt.Fprintf(&b, "| Target schema | %d |\n", l.TargetSchema)
	fmt.Fprintf(&b, "| Built at | %s |\n", l.BuiltAt.UTC().Format("2006-01-02 15:04:05Z"))

	b.WriteString("\n### Artifacts\n\n")
	b.WriteString("These exact descriptors are what promotion will tag. Promotion does not " +
		"rebuild; if a read-back disagrees with one of these, the release stops before the " +
		"Git tag.\n\n")
	b.WriteString("| Artifact | Digest | Media type | Size | Platforms |\n|---|---|---|---|---|\n")
	for _, name := range ArtifactNames {
		a, ok := l.Artifacts[name]
		if !ok {
			fmt.Fprintf(&b, "| `%s` | **MISSING** | | | |\n", name)
			continue
		}
		plat, err := ArtifactPlatforms(name, a)
		platCell := joinOrNone(plat)
		if err != nil {
			platCell = "**" + err.Error() + "**"
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | %d | %s |\n",
			a.Repository, a.Descriptor.Digest, a.Descriptor.MediaType, a.Descriptor.Size, platCell)
	}

	sidecars := make([]string, 0, len(l.Sidecars))
	for k := range l.Sidecars {
		sidecars = append(sidecars, k)
	}
	sort.Strings(sidecars)
	if len(sidecars) > 0 {
		b.WriteString("\n**Sidecars**, carried forward from the intent unchanged:\n\n")
		for _, k := range sidecars {
			fmt.Fprintf(&b, "- `%s` → `%s`\n", k, l.Sidecars[k])
		}
	}

	b.WriteString("\n### Claims and the evidence behind them\n\n")
	b.WriteString("Every claim below is bound to a statement by CONTENT DIGEST, and every " +
		"statement was produced by a gate running against the digests above.\n\n")
	b.WriteString("| Claim | Gate | Runner | Outcome | Statement |\n|---|---|---|---|---|\n")
	for _, c := range l.ProvenClaims {
		for _, e := range c.Evidence {
			fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | %s | `%s` |\n",
				c.ID, c.Gate, e.RunnerClass, e.Outcome, short(e.StatementDigest))
		}
	}

	if len(evidence) > 0 {
		b.WriteString("\n### What each gate exercised\n\n")
		st := make([]EvidenceStatement, len(evidence))
		copy(st, evidence)
		sort.Slice(st, func(i, j int) bool { return st[i].Gate < st[j].Gate })
		b.WriteString("| Gate | Scenarios | Capabilities | Duration |\n|---|---|---|---|\n")
		for _, e := range st {
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n",
				e.Gate, joinOrNone(e.Scenarios), joinOrNone(e.Capabilities),
				e.FinishedAt.Sub(e.StartedAt).Round(1e9))
		}
	}

	b.WriteString("\n### What approving does\n\n")
	b.WriteString("1. Adds the `" + l.Version + "` OCI tag to each digest above — no rebuild — " +
		"and reads each tag back, comparing the full descriptor.\n")
	b.WriteString("2. Creates the annotated Git tag `" + l.Version + "` at `" + l.SourceCommit +
		"`.\n")
	b.WriteString("3. Publishes the release with the bundle attached.\n")
	b.WriteString("\nEverything before the Git tag converges on a retry. After it, a retry " +
		"resumes rather than recreates. Nothing rebuilds.\n")
	b.WriteString("\nPublication does **not** complete the milestone: the post-publication " +
		"transition gate runs afterwards, and its result is never folded back into this lock.\n")

	return b.Bytes()
}

func short(digest string) string {
	if len(digest) > 19 {
		return digest[:19] + "…"
	}
	return digest
}
