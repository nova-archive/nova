package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCatalogIsCompiledIn. T1.22 forbids fetching release metadata, so what a
// build claims about itself must be linked in — no filesystem, no network, and
// no releases/ directory copied into any image.
func TestCatalogIsCompiledIn(t *testing.T) {
	c := Compiled()
	if !c.Stamped() {
		t.Fatal("the generated catalog is empty; run `make catalog`")
	}
	if c.Version == "" || c.IntentDigest == "" {
		t.Fatalf("catalog = %+v, want a version and an intent digest", c)
	}
	if err := validateSHA256(c.IntentDigest); err != nil {
		t.Errorf("intent digest: %v", err)
	}
	if c.TargetSchema <= 0 {
		t.Errorf("target schema = %d", c.TargetSchema)
	}
}

// TestCatalogCarriesSupportWindowMetadata: the census and the deprecation
// channel evaluate the support window with no network, so everything they need
// has to be here.
func TestCatalogCarriesSupportWindowMetadata(t *testing.T) {
	c := Compiled()
	if c.SupportEpoch.IsZero() {
		t.Error("no support epoch; the window has nothing to measure from")
	}
	if len(c.SupportedPredecessors) == 0 {
		t.Error("no supported predecessors; the predecessor branch of the window is empty")
	}
	for _, p := range c.SupportedPredecessors {
		if p.SupportEpoch.IsZero() {
			t.Errorf("predecessor %s carries no epoch, so its age branch cannot be evaluated", p.ID())
		}
	}
	if len(c.Profiles.Core) == 0 {
		t.Error("no core capability profile; profile compliance cannot be classified")
	}
}

// TestCatalogMatchesCommittedIntent is the drift gate in test form. `make
// catalog-check` runs the same comparison over bytes; this one catches the case
// where the file was regenerated from a DIFFERENT intent than the current one.
func TestCatalogMatchesCommittedIntent(t *testing.T) {
	path, err := CurrentIntentPath(intentsDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	in, err := ParseIntent(b)
	if err != nil {
		t.Fatal(err)
	}
	want := CatalogFrom(in, b)
	got := Compiled()

	if got.Version != want.Version {
		t.Errorf("catalog version = %q, intent says %q", got.Version, want.Version)
	}
	if got.IntentDigest != want.IntentDigest {
		t.Errorf("catalog intent digest = %s, but %s hashes to %s; run `make catalog`",
			got.IntentDigest, filepath.Base(path), want.IntentDigest)
	}
	if got.TargetSchema != want.TargetSchema {
		t.Errorf("catalog target schema = %d, intent says %d", got.TargetSchema, want.TargetSchema)
	}
}

// TestRenderedCatalogIsDeterministic. The drift gate compares bytes, so a
// renderer that iterated a map in Go's randomized order would fail at random.
func TestRenderedCatalogIsDeterministic(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(intentsDir(), "v0.3.0.json"))
	if err != nil {
		t.Fatal(err)
	}
	in, err := ParseIntent(b)
	if err != nil {
		t.Fatal(err)
	}
	c := CatalogFrom(in, b)

	first, err := RenderCatalog(c)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		again, err := RenderCatalog(c)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatal("RenderCatalog is not byte-stable; the drift gate would fail at random")
		}
	}
}

func TestCurrentIntentPathPicksHighestPrecedence(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"v0.2.0", "v0.10.0", "v0.3.0"} {
		if err := os.WriteFile(filepath.Join(dir, v+".json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := CurrentIntentPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	// SemVer precedence, not lexical order: v0.10.0 > v0.3.0 > v0.2.0.
	if filepath.Base(got) != "v0.10.0.json" {
		t.Errorf("got %s, want v0.10.0.json — precedence is SemVer, not string order", filepath.Base(got))
	}
}

// TestCatalogIsLinkedIntoEveryBinaryThatClaimsIt.
//
// A package test proves the package compiles, NOT that any shipped binary
// references it — Go drops packages nothing links. So this builds each binary
// and executes it, asserting the real intent digest appears in its output. It
// is also what catches a binary that imports the catalog but never prints it,
// which would leave an operator with no way to ask.
func TestCatalogIsLinkedIntoEveryBinaryThatClaimsIt(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries")
	}
	want := Compiled().IntentDigest
	if want == "" {
		t.Fatal("no catalog to look for")
	}

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	// cmd/coordinator needs cgo and libvips; the image build covers it. These
	// three are the pure-Go binaries a unit test can build.
	for _, cmd := range []string{"node", "novactl", "migrate"} {
		bin := filepath.Join(dir, cmd)
		build := exec.Command("go", "build", "-o", bin, "./cmd/"+cmd)
		build.Dir = root
		if out, berr := build.CombinedOutput(); berr != nil {
			t.Fatalf("build %s: %v\n%s", cmd, berr, out)
		}
		out, _ := exec.Command(bin, "--version").CombinedOutput()
		if !strings.Contains(string(out), want) {
			t.Errorf("%s --version does not report the catalog intent digest %s:\n%s",
				cmd, want, strings.TrimSpace(string(out)))
		}
	}
}

// TestCoordinatorLinksTheCatalog covers the one binary the test above cannot
// build here, at the source level. The image build executes it for real.
func TestCoordinatorLinksTheCatalog(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "cmd", "coordinator", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "release.Compiled()") {
		t.Error("cmd/coordinator does not print the release catalog; Go would drop the package " +
			"and the coordinator could not answer what release it belongs to")
	}
}
