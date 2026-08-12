package release

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestMatrixIsDeterministicUnderShuffledInputs. A generator whose output
// depends on map iteration order cannot have a drift gate: every run would be
// drift, and the gate would be turned off within a week.
func TestMatrixIsDeterministicUnderShuffledInputs(t *testing.T) {
	b, in := readCommittedIntent(t)
	cat := CatalogFrom(in, b)

	first := RenderMatrix(in, cat, Coverage())
	for range 8 {
		// Re-rendering exercises Go's randomized map iteration over the role
		// map and the coverage table without needing to shuffle them by hand.
		if got := RenderMatrix(in, cat, Coverage()); string(got) != string(first) {
			t.Fatalf("two renders of identical inputs differ:\n--- a ---\n%s\n--- b ---\n%s",
				first, got)
		}
	}

	// And an explicitly reordered coverage table produces the same bytes,
	// because the renderer sorts.
	shuffled := Coverage()
	slices.Reverse(shuffled)
	if got := RenderMatrix(in, cat, shuffled); string(got) != string(first) {
		t.Error("reversing the coverage table changed the output; the renderer is not sorting")
	}
}

// TestMatrixStatesWhatEachGateDoesNotProve. An entry that lists only what a
// gate proves invites the reader to assume everything else, and the reader is
// an operator deciding whether to upgrade.
func TestMatrixStatesWhatEachGateDoesNotProve(t *testing.T) {
	b, in := readCommittedIntent(t)
	out := string(RenderMatrix(in, CatalogFrom(in, b), Coverage()))

	if !strings.Contains(out, "What the gates do NOT prove") {
		t.Fatal("the matrix does not record any gate's limits")
	}
	for _, c := range Coverage() {
		if !strings.Contains(out, "`"+c.Gate+"`") {
			t.Errorf("gate %s is missing from the matrix", c.Gate)
		}
		for _, l := range c.DoesNotProve {
			if !strings.Contains(out, l) {
				t.Errorf("gate %s: the matrix omits the limit %q", c.Gate, l)
			}
		}
	}
}

// TestMatrixMarksPlaceholderCoverageInTheDocument. An operator reading the
// matrix has to be able to see that a claim is backed by an intention rather
// than a result — that is the difference between "tested" and "meant to be".
func TestMatrixMarksPlaceholderCoverageInTheDocument(t *testing.T) {
	b, in := readCommittedIntent(t)
	out := string(RenderMatrix(in, CatalogFrom(in, b), Coverage()))

	var anyPlaceholder bool
	for _, c := range Coverage() {
		if c.Placeholder {
			anyPlaceholder = true
		}
	}
	if !anyPlaceholder {
		t.Skip("no placeholders remain; this assertion has nothing to check")
	}
	if !strings.Contains(out, "placeholder — blocks a release candidate") {
		t.Error("coverage is still a placeholder for some gate and the matrix does not say so")
	}
}

// TestSpliceRefusesMissingOrDuplicateMarkers. Zero markers means the generated
// section silently stopped being checked; two means the generator has to guess
// which block it owns, and guessing is how it overwrites hand-written text.
func TestSpliceRefusesMissingOrDuplicateMarkers(t *testing.T) {
	body := []byte("\nbody\n")
	for name, doc := range map[string]string{
		"no markers at all": "# Doc\n\ntext\n",
		"only begin":        "# Doc\n" + MatrixBegin + "\ntext\n",
		"only end":          "# Doc\n" + MatrixEnd + "\ntext\n",
		"two begins": "# Doc\n" + MatrixBegin + "\n" + MatrixBegin + "\n" +
			MatrixEnd + "\n",
		"two ends": "# Doc\n" + MatrixBegin + "\n" + MatrixEnd + "\n" + MatrixEnd + "\n",
		"reversed": "# Doc\n" + MatrixEnd + "\n" + MatrixBegin + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Splice([]byte(doc), body); err == nil {
				t.Fatal("accepted a document the generator cannot own unambiguously")
			}
		})
	}
}

// TestSpliceLeavesHandWrittenTextAlone. Everything outside the markers is
// written by a person, and a generator that reformats it will eventually be run
// by someone who then commits the reformatting.
func TestSpliceLeavesHandWrittenTextAlone(t *testing.T) {
	doc := "# Upgrading\n\nbefore\n\n" + MatrixBegin + "\nSTALE\n" + MatrixEnd + "\n\nafter\n"
	out, err := Splice([]byte(doc), []byte("\nfresh\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.HasPrefix(got, "# Upgrading\n\nbefore\n\n") {
		t.Errorf("text before the block changed:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n\nafter\n") {
		t.Errorf("text after the block changed:\n%s", got)
	}
	if strings.Contains(got, "STALE") {
		t.Error("the old generated content survived")
	}
	if !strings.Contains(got, "fresh") {
		t.Error("the new generated content is missing")
	}

	// Idempotent: splicing the same body twice is a no-op, so the drift gate
	// cannot fail on a document that was just generated.
	twice, err := Splice(out, []byte("\nfresh\n"))
	if err != nil {
		t.Fatal(err)
	}
	if string(twice) != got {
		t.Error("splicing twice changed the document; the gate would never be green")
	}
}

// TestCommittedUpgradingDocIsCurrent is the golden test: the checked-in document
// is exactly what the generator produces from the checked-in inputs.
func TestCommittedUpgradingDocIsCurrent(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "UPGRADING.md")
	doc, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b, in := readCommittedIntent(t)

	want, err := Splice(doc, RenderMatrix(in, CatalogFrom(in, b), Coverage()))
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(doc) {
		t.Error("docs/UPGRADING.md has drifted from the intent or the gate coverage; run " +
			"`make release-docs` and commit the result")
	}
}

// TestUpgradingDocCarriesTheDecisionsItTurnsOn. Each of these sentences was a
// decision, and a document that loses one loses the decision with it. The
// shell gate checks the same phrases; this one fails faster and in the package
// that owns them.
func TestUpgradingDocCarriesTheDecisionsItTurnsOn(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "UPGRADING.md"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(doc)
	for _, phrase := range []string{
		"A backup that has never passed restore verification does not count",
		"`migrate down` does not exist",
		"Unknown is not unsupported",
		"Acknowledgement waives a SKIP",
		"the release env is LAST",
		novaIdentity,
	} {
		if !strings.Contains(body, phrase) {
			t.Errorf("docs/UPGRADING.md no longer says: %s", phrase)
		}
	}
	if strings.Contains(body, "--certificate-identity-regexp") {
		t.Error("the documented bootstrap uses an identity REGEXP; it is one typo away from " +
			"admitting a workflow nobody meant to trust")
	}
}
