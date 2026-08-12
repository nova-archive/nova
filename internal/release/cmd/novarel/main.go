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
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
	case "evidence":
		return emitEvidence(args[1:])
	case "lock":
		return buildLockCmd(args[1:])
	case "bundle":
		return assembleCmd(args[1:])
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
  evidence         emit a signed-able evidence statement from a gate result
  lock             assemble and validate a lock from descriptors + evidence
  bundle           assemble a release bundle directory and verify it
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

// emitEvidence writes one gate's conclusion as a statement.
//
//	novarel evidence --gate G --outcome passed --runner rc-docker \
//	  --release vX.Y.Z --commit SHA --test-revision SHA \
//	  --artifacts descriptors/ --claims a,b --out evidence/G.json
//
// The digest it prints is what the lock references.
func emitEvidence(args []string) error {
	fs := flag.NewFlagSet("evidence", flag.ContinueOnError)
	gate := fs.String("gate", "", "the gate that produced this conclusion")
	outcome := fs.String("outcome", "passed", "passed or skipped; a FAILED gate emits nothing")
	runner := fs.String("runner", "", "the tier it actually ran in")
	rel := fs.String("release", "", "the release this is about")
	commit := fs.String("commit", "", "the candidate source commit")
	testRev := fs.String("test-revision", "", "the commit of the test code that ran")
	descDir := fs.String("artifacts", "descriptors", "directory of <name>.json OCI descriptors")
	claims := fs.String("claims", "", "comma-separated claim ids this supports")
	scenarios := fs.String("scenarios", "", "comma-separated scenario ids exercised")
	detail := fs.String("detail", "", "context; REQUIRED for a skip")
	started := fs.String("started", "", "RFC3339 start time (default: now)")
	out := fs.String("out", "", "where to write the statement")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *gate == "" || *rel == "" || *commit == "" || *out == "" {
		return fmt.Errorf("usage: novarel evidence --gate G --release V --commit SHA --out PATH [...]")
	}
	if *testRev == "" {
		*testRev = *commit
	}

	digests, err := readDescriptorDigests(*descDir)
	if err != nil {
		return err
	}
	start := time.Now().UTC()
	if *started != "" {
		if start, err = time.Parse(time.RFC3339, *started); err != nil {
			return fmt.Errorf("--started: %w", err)
		}
	}

	st := release.EvidenceStatement{
		Schema: release.EvidenceSchema, Gate: *gate,
		RunnerClass: release.RunnerClass(*runner), Outcome: *outcome,
		Release: *rel, SourceCommit: *commit, ArtifactDigests: digests,
		Claims: splitList(*claims), Scenarios: splitList(*scenarios),
		TestRevision: *testRev,
		StartedAt:    start.UTC(), FinishedAt: time.Now().UTC(),
		Detail: *detail,
	}
	body, err := st.Render()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*out, body, 0o644); err != nil {
		return err
	}
	fmt.Println(release.EvidenceDigest(body))
	return nil
}

// buildLockCmd assembles the lock from things that already exist. It invents
// nothing: a builder that could fill in a missing digest could produce a lock
// for a release nobody built.
func buildLockCmd(args []string) error {
	fs := flag.NewFlagSet("lock", flag.ContinueOnError)
	commit := fs.String("commit", "", "the candidate source commit")
	descDir := fs.String("artifacts", "descriptors", "directory of <name>.json OCI descriptors")
	evDir := fs.String("evidence", "evidence", "directory of evidence statements")
	bundleDir := fs.String("bundle", "", "the assembled bundle whose payload map the lock carries")
	out := fs.String("out", "lock.json", "where to write the lock")
	builtAt := fs.String("built-at", "", "RFC3339 build time (default: now)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *commit == "" || *bundleDir == "" {
		return fmt.Errorf("usage: novarel lock --commit SHA --bundle DIR [--out lock.json]")
	}

	path, err := release.CurrentIntentPath(defaultIntentDir)
	if err != nil {
		return err
	}
	intentBytes, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	in, err := release.ParseIntent(intentBytes)
	if err != nil {
		return err
	}

	artifacts, err := readDescriptors(*descDir)
	if err != nil {
		return err
	}
	evidence, err := readEvidence(*evDir)
	if err != nil {
		return err
	}
	payload, err := payloadOf(*bundleDir)
	if err != nil {
		return err
	}

	at := time.Now().UTC()
	if *builtAt != "" {
		if at, err = time.Parse(time.RFC3339, *builtAt); err != nil {
			return fmt.Errorf("--built-at: %w", err)
		}
	}

	l, err := release.BuildLock(release.LockInputs{
		Intent: in, IntentBytes: intentBytes,
		SourceCommit: *commit, BuiltAt: at,
		Artifacts: artifacts, Sidecars: in.Sidecars,
		Evidence: evidence, Payload: payload,
		Coverage: release.Coverage(),
	})
	if err != nil {
		return err
	}
	body, err := release.RenderLock(l)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, body, 0o644); err != nil {
		return err
	}

	// Verify the assembled bundle against the lock NOW. The useful moment to
	// discover that they disagree is before the lock is signed and published.
	if err := release.VerifyAssembled(*bundleDir, l); err != nil {
		return fmt.Errorf("the bundle does not match the lock just built for it: %w", err)
	}
	fmt.Printf("wrote %s  %s\n", *out, release.LockDigest(body))
	fmt.Printf("donor lock digest %s\n", l.DonorLockDigest)
	return nil
}

// assembleCmd writes the bundle directory. The lock is NOT written here: it
// carries the payload map, so it does not exist until this returns.
func assembleCmd(args []string) error {
	fs := flag.NewFlagSet("bundle", flag.ContinueOnError)
	out := fs.String("out", "", "the bundle directory to write")
	evDir := fs.String("evidence", "evidence", "directory of evidence statements")
	composeEnv := fs.String("release-env", "docker/release.env.example", "the release env")
	upgrading := fs.String("upgrading", upgradingDoc, "the release's UPGRADING.md")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("usage: novarel bundle --out DIR")
	}

	path, err := release.CurrentIntentPath(defaultIntentDir)
	if err != nil {
		return err
	}
	read := func(p string) []byte {
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			err = rerr
		}
		return b
	}
	inputs := release.BundleInputs{
		IntentBytes: read(path),
		Policy:      read("releases/verification-policy.txt"),
		ComposeEnv:  read(*composeEnv),
		Upgrading:   read(*upgrading),
		Bootstrap:   read("scripts/nova-release"),
		Evidence:    map[string][]byte{},
	}
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(*evDir)
	if err != nil {
		return fmt.Errorf("%s: %w (run the gates and `novarel evidence` first)", *evDir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(*evDir, e.Name()))
		if rerr != nil {
			return rerr
		}
		if _, rerr := release.ParseEvidence(b); rerr != nil {
			return fmt.Errorf("%s: %w", e.Name(), rerr)
		}
		inputs.Evidence["evidence/"+e.Name()] = b
	}

	payload, err := release.AssembleBundle(*out, inputs)
	if err != nil {
		return err
	}
	fmt.Printf("assembled %s with %d covered member(s)\n", *out, len(payload))
	fmt.Println("next: novarel lock --commit <sha> --bundle " + *out + " --out " + *out + "/lock.json")
	return nil
}

// ---------------------------------------------------------------------------

func readDescriptorDigests(dir string) (map[string]string, error) {
	arts, err := readDescriptors(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for name, a := range arts {
		out[name] = a.Descriptor.Digest.String()
	}
	return out, nil
}

func readDescriptors(dir string) (map[string]release.LockedArtifact, error) {
	out := map[string]release.LockedArtifact{}
	for _, name := range release.ArtifactNames {
		b, err := os.ReadFile(filepath.Join(dir, name+".json"))
		if err != nil {
			return nil, fmt.Errorf("%s: %w (the candidate job writes these)", name, err)
		}
		var a release.LockedArtifact
		if err := json.Unmarshal(b, &a); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out[name] = a
	}
	return out, nil
}

func readEvidence(dir string) (map[string]release.EvidencePair, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	out := map[string]release.EvidencePair{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			return nil, rerr
		}
		st, rerr := release.ParseEvidence(b)
		if rerr != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), rerr)
		}
		d := release.EvidenceDigest(b)
		for _, claim := range st.Claims {
			out[claim] = release.EvidencePair{Statement: st, Digest: d}
		}
	}
	return out, nil
}

// payloadOf recomputes the payload map from a bundle already on disk, so the
// lock describes what is actually there rather than what the assembler
// intended.
func payloadOf(dir string) (map[string]string, error) {
	files, err := release.ReadBundle(os.DirFS(dir))
	if err != nil {
		return nil, err
	}
	keep := files[:0]
	for _, f := range files {
		switch f.Path {
		case "lock.json", release.AuthRoot, "lock.pem", "lock.crt":
			continue
		}
		keep = append(keep, f)
	}
	return release.BuildPayload(keep)
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
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
