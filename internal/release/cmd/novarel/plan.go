package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nova-archive/nova/internal/release"
)

// Resolving WHICH intent, and the subcommands the release workflow drives
// (P2-M7.3, amendment).
//
// # Why --version exists at all
//
// Every subcommand used to call CurrentIntentPath, which returns the greatest
// version by SemVer precedence in releases/intent. That is right for the
// catalog — the compiled-in identity is the newest reviewed decision — and
// wrong for the release workflow, which is dispatched for an EXACT version.
//
// Concretely: prepare v0.4.0's intent, then re-dispatch v0.3.0 to recover an
// interrupted release, and every command would silently operate on v0.4.0. The
// digests would be v0.3.0's, the intent v0.4.0's, and the lock would refuse at
// the very end — after three images had been pushed and signed.
//
// # Why --intent-dir exists
//
// The reusability gate drives this whole pipeline against a synthetic second
// release, to prove the logic is not first-release-only. Its intent cannot live
// in releases/intent: the catalog is generated from the greatest version there,
// so a fixture would rewrite the compiled-in identity of the real product.

// intentSelector is the common way a subcommand says which reviewed decision it
// is operating on.
type intentSelector struct {
	dir     *string
	version *string
}

func addIntentFlags(fs *flag.FlagSet) intentSelector {
	return intentSelector{
		dir: fs.String("intent-dir", defaultIntentDir, "directory of checked-in intents"),
		version: fs.String("version", "", "the EXACT version being released (default: the "+
			"greatest by SemVer precedence, which is right for the catalog and wrong for a "+
			"workflow dispatched for a specific version)"),
	}
}

type resolvedIntent struct {
	path   string
	bytes  []byte
	intent release.Intent
}

func (s intentSelector) resolve() (resolvedIntent, error) {
	path := ""
	if *s.version != "" {
		path = filepath.Join(*s.dir, release.FilenameFor(*s.version))
		if _, err := os.Stat(path); err != nil {
			return resolvedIntent{}, fmt.Errorf("no intent at %s. The reviewed decision is "+
				"checked in BEFORE the build; a release with none behind it is not a release", path)
		}
	} else {
		p, err := release.CurrentIntentPath(*s.dir)
		if err != nil {
			return resolvedIntent{}, err
		}
		path = p
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return resolvedIntent{}, err
	}
	in, err := release.ParseIntent(b)
	if err != nil {
		return resolvedIntent{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := release.ValidateFilename(path, in); err != nil {
		return resolvedIntent{}, err
	}
	if *s.version != "" && in.Version != *s.version {
		return resolvedIntent{}, fmt.Errorf("%s declares version %s, not the requested %s",
			path, in.Version, *s.version)
	}
	return resolvedIntent{path: path, bytes: b, intent: in}, nil
}

// planCmd emits the gates the workflow must execute, derived from the claims
// the intent declares rather than from a list somebody maintains in YAML.
func planCmd(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	sel := addIntentFlags(fs)
	out := fs.String("out", "", "write the plan here (default: stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ri, err := sel.resolve()
	if err != nil {
		return err
	}
	p, err := release.BuildPlan(ri.intent, ri.bytes, ri.path, release.Coverage())
	if err != nil {
		return err
	}
	body, err := p.Render()
	if err != nil {
		return err
	}
	if *out == "" {
		os.Stdout.Write(body)
		return nil
	}
	return os.WriteFile(*out, body, 0o644)
}

// LoadPlan reads a plan back. The workflow writes it once and every later step
// reads it, so a step cannot disagree with the plan the evidence job ran.
func loadPlan(path string) (release.ReleasePlan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return release.ReleasePlan{}, fmt.Errorf("%s: %w (run `novarel plan` first)", path, err)
	}
	var p release.ReleasePlan
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return release.ReleasePlan{}, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

func plannedGate(p release.ReleasePlan, gate string) (release.PlannedGate, error) {
	for _, g := range append(append([]release.PlannedGate{}, p.PrePublication...), p.PostPublication...) {
		if g.Gate == gate {
			return g, nil
		}
	}
	return release.PlannedGate{}, fmt.Errorf("the plan for %s does not run gate %q; a statement "+
		"from a gate the plan never scheduled is a conclusion about something nobody asked for",
		p.Version, gate)
}

// summaryCmd renders what a reviewer sees at the approval gate.
func summaryCmd(args []string) error {
	fs := flag.NewFlagSet("summary", flag.ContinueOnError)
	sel := addIntentFlags(fs)
	lockPath := fs.String("lock", "lock.json", "the finalized lock")
	evDir := fs.String("evidence", "", "directory of evidence statements (optional detail)")
	out := fs.String("out", "", "write the Markdown here (default: stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ri, err := sel.resolve()
	if err != nil {
		return err
	}
	lockBytes, err := os.ReadFile(*lockPath)
	if err != nil {
		return err
	}
	l, err := release.ParseLock(lockBytes, ri.intent, ri.bytes)
	if err != nil {
		return err
	}

	var statements []release.EvidenceStatement
	if *evDir != "" {
		entries, rerr := os.ReadDir(*evDir)
		if rerr != nil {
			return rerr
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			b, rerr := os.ReadFile(filepath.Join(*evDir, e.Name()))
			if rerr != nil {
				return rerr
			}
			st, rerr := release.ParseEvidence(b)
			if rerr != nil {
				return fmt.Errorf("%s: %w", e.Name(), rerr)
			}
			statements = append(statements, st)
		}
	}

	body := release.RenderApprovalSummary(l, release.LockDigest(lockBytes), statements)
	if *out == "" {
		os.Stdout.Write(body)
		return nil
	}
	return os.WriteFile(*out, body, 0o644)
}

// completionCmd answers the only question the ROADMAP row turns on: have the
// post-publication gates run?
//
// It is deliberately separate from every pre-publication check. A release can
// exist with this outstanding — that is completion state 3 — and conflating the
// two would either block releases on evidence that cannot exist yet, or mark a
// milestone done on evidence nobody gathered.
func completionCmd(args []string) error {
	fs := flag.NewFlagSet("completion", flag.ContinueOnError)
	evDir := fs.String("evidence", "", "directory of POST-publication evidence statements")
	release_ := fs.String("release", "", "the published release these statements are about")
	if err := fs.Parse(args); err != nil {
		return err
	}

	table := release.Coverage()
	if *evDir == "" {
		// No statements supplied: report the checked-in state, which is what
		// the ROADMAP row reflects between releases.
		if err := release.CompletionReady(table); err != nil {
			return err
		}
		fmt.Println("completion state 4: every post-publication gate has run")
		return nil
	}

	// Statements supplied: they must cover every post-publication gate, and
	// each must be a PASS about the named release. A skip is a recorded
	// absence, which is exactly what completion is waiting on.
	passed := map[string]release.EvidenceStatement{}
	entries, err := os.ReadDir(*evDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(*evDir, e.Name()))
		if rerr != nil {
			return rerr
		}
		st, rerr := release.ParseEvidence(b)
		if rerr != nil {
			return fmt.Errorf("%s: %w", e.Name(), rerr)
		}
		if *release_ != "" && st.Release != *release_ {
			return fmt.Errorf("%s is about %s, not the published %s", e.Name(), st.Release, *release_)
		}
		if st.Outcome == "passed" {
			passed[st.Gate] = st
		}
	}

	var missing []string
	for _, c := range table {
		if !c.PostPublication {
			continue
		}
		if _, ok := passed[c.Gate]; !ok {
			missing = append(missing, c.Gate)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("completion state 4 not reached: no passing statement for %s. The "+
			"release remains published and state 3 stands; only the post-publication job is "+
			"retried, and the lock is never re-cut", strings.Join(missing, ", "))
	}
	fmt.Printf("completion state 4 reached for %s\n", *release_)
	for gate, st := range passed {
		fmt.Printf("  %-24s %s  scenarios %s\n", gate, st.RunnerClass,
			strings.Join(st.Scenarios, ","))
	}
	return nil
}

// printAllPredecessors writes every supported predecessor's git ref, newest
// first, one per line.
//
// The mixed-fleet gate consumes this: "the new coordinator against every donor
// version still inside the support window" is a property of the intent, and a
// gate that stands up one hard-coded older donor stops testing the window the
// moment a second predecessor is declared.
func printAllPredecessors(intentDir string) error {
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
	for _, p := range in.SupportedPredecessors {
		switch p.Kind {
		case "commit":
			fmt.Println(p.Commit)
		default:
			fmt.Println(p.Version)
		}
	}
	return nil
}
