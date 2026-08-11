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
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nova-archive/nova/internal/release"
)

const defaultIntentDir = "releases/intent"

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
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: novarel <validate|digest|show>

  validate [dir]   every intent under dir (default `+defaultIntentDir+`) parses,
                   validates, and is named for the version it declares
  digest <file>    the sha256 the release workflow references this file by
  show <intent>    the parsed intent, as the workflow reads it
`)
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
