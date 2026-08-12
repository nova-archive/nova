// Command novarel is the BUILD-TIME release tool (P2-M7.3).
//
// It lives under internal/release/cmd rather than in the repository's cmd/
// directory on purpose: nothing here ships in a runtime image. The coordinator,
// the donor and novactl read their release identity from the compiled-in
// catalog and from an operator-supplied verified lock — never by running a tool
// that could fetch or recompute one (T1.22).
//
// Subcommands:
//
//	novarel validate [dir]        every checked-in intent parses and validates
//	novarel digest <file>         the sha256 an intent or lock is referenced by
//	novarel show <intent>         the intent as the release workflow reads it
//	novarel catalog [--check]     generate (or verify) the compiled-in catalog
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nova-archive/nova/internal/release"
)

const (
	defaultIntentDir = "releases/intent"
	catalogOut       = "internal/release/catalog/catalog_gen.go"
	upgradingDoc     = "docs/UPGRADING.md"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "novarel:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return fmt.Errorf("a subcommand is required")
	}
	switch args[0] {
	case "validate":
		dir := defaultIntentDir
		if len(args) > 1 {
			dir = args[1]
		}
		return validate(dir)
	case "digest":
		if len(args) != 2 {
			return fmt.Errorf("usage: novarel digest <file>")
		}
		b, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		// Intent and lock digests are both sha256 over the exact bytes, so one
		// implementation covers both and cannot drift between them.
		fmt.Println(release.IntentDigest(b))
		return nil
	case "show":
		if len(args) != 2 {
			return fmt.Errorf("usage: novarel show <intent>")
		}
		return show(args[1])
	case "catalog":
		check := len(args) > 1 && args[1] == "--check"
		return generateCatalog(defaultIntentDir, catalogOut, check)
	case "matrix":
		check := len(args) > 1 && args[1] == "--check"
		return generateMatrix(defaultIntentDir, upgradingDoc, check)
	case "predecessor":
		index := 0
		if len(args) > 1 {
			n, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("usage: novarel predecessor [index]")
			}
			index = n
		}
		return printPredecessor(defaultIntentDir, index)
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: novarel <validate|digest|show|catalog>

  validate [dir]   every intent under dir (default `+defaultIntentDir+`) parses,
                   validates, and is named for the version it declares
  digest <file>    the sha256 the release workflow references this file by
  show <intent>    the parsed intent, as the workflow reads it
  catalog          regenerate internal/release/catalog_gen.go from the current intent
  catalog --check  fail if the generated catalog has drifted from the intent
  matrix           regenerate the compatibility matrix in `+upgradingDoc+`
  matrix --check   fail if that block has drifted from the intent or the coverage
  predecessor [i]  the GIT REF of the i-th supported predecessor (default 0, the
                   immediate one). Cross-version gates check this out, so the ref
                   comes from the reviewed intent rather than a shell variable
                   somebody has to remember to bump.
`)
}

// printPredecessor writes one git ref on stdout.
//
// The cross-version gate used to carry `PRIOR_TAG="${PRIOR_TAG:-p2-m6-...}"`.
// That is the wrong shape twice over: a default nobody updates goes stale
// silently — it was three milestones behind — and the FIRST predecessor is
// commit-anchored, because the deployment in the field is a local build of a
// commit with no product tag at all. A variable that can only hold a tag cannot
// name it.
func printPredecessor(intentDir string, index int) error {
	path, err := release.CurrentIntentPath(intentDir)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	in, err := release.ParseIntent(b)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if index < 0 || index >= len(in.SupportedPredecessors) {
		return fmt.Errorf("%s declares %d supported predecessor(s); there is no index %d",
			path, len(in.SupportedPredecessors), index)
	}
	p := in.SupportedPredecessors[index]
	switch p.Kind {
	case "commit":
		fmt.Println(p.Commit)
	default:
		fmt.Println(p.Version)
	}
	return nil
}

// generateMatrix renders the compatibility matrix into UPGRADING.md between its
// markers, or verifies that it has not drifted.
//
// It compares BYTES, like the catalog gate, for the same reason: comparing
// meaning would let a hand edit that happens to say the same thing pass, and
// the point is that this block is generated rather than that it is currently
// accurate.
func generateMatrix(intentDir, doc string, checkOnly bool) error {
	path, err := release.CurrentIntentPath(intentDir)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	in, err := release.ParseIntent(b)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	current, err := os.ReadFile(doc)
	if err != nil {
		return err
	}
	want, err := release.Splice(current,
		release.RenderMatrix(in, release.CatalogFrom(in, b), release.Coverage()))
	if err != nil {
		return fmt.Errorf("%s: %w", doc, err)
	}

	if checkOnly {
		if !bytes.Equal(current, want) {
			return fmt.Errorf("%s has drifted from %s or from the gate coverage; run "+
				"`make release-docs` and commit the result", doc, path)
		}
		fmt.Printf("ok  %s matrix matches %s and the gate coverage\n", doc, path)
		return nil
	}
	if err := os.WriteFile(doc, want, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote the compatibility matrix into %s from %s\n", doc, path)
	return nil
}

func validate(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		in, err := release.ParseIntent(b)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if err := release.ValidateFilename(path, in); err != nil {
			return err
		}
		fmt.Printf("ok  %-28s %s  target schema %d\n", e.Name(), release.IntentDigest(b), in.TargetSchema)
		checked++
	}
	if checked == 0 {
		return fmt.Errorf("no intents found under %s; a passing run over an empty directory "+
			"would be a gate that proves nothing", dir)
	}
	return nil
}

func show(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	in, err := release.ParseIntent(b)
	if err != nil {
		return err
	}
	out, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

// generateCatalog writes (or verifies) the compiled-in catalog.
//
// The drift gate compares BYTES. Comparing meaning would let a regeneration
// that reorders a map pass while producing a different binary, and the whole
// point of the catalog is that the binary and the reviewed intent agree.
func generateCatalog(intentDir, out string, checkOnly bool) error {
	path, err := release.CurrentIntentPath(intentDir)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	in, err := release.ParseIntent(b)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	want, err := release.RenderCatalog(release.CatalogFrom(in, b))
	if err != nil {
		return err
	}

	if checkOnly {
		got, err := os.ReadFile(out)
		if err != nil {
			return fmt.Errorf("%s: %w (run `make catalog`)", out, err)
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("%s has drifted from %s; run `make catalog` and commit the result", out, path)
		}
		fmt.Printf("ok  %s matches %s\n", out, path)
		return nil
	}

	if err := os.WriteFile(out, want, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s from %s (%s)\n", out, path, release.IntentDigest(b))
	return nil
}
