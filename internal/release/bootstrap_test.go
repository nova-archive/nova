package release

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// The out-of-band trust bootstrap (P2-M7.3, D-M7.3-2a).
//
// Trust must not originate in the material being verified. These tests
// substitute each thing an attacker would have to substitute and assert refusal
// BEFORE anything from the bundle is executed — which is checked directly, by
// asserting the stub docker was never invoked.
//
// scripts/nova-release is driven as a subprocess with stubbed cosign and docker.
// Stubbing them is not a weakening: PATH is already an environment variable, so
// the override the stubs use adds no attack surface that resolving `cosign`
// through PATH did not already have, and it is the only way to exercise the
// refusal paths without a registry.

const bootstrapScript = "../../scripts/nova-release"

type bootstrapEnv struct {
	bundle     string
	dockerLog  string
	cosignLog  string
	lockDigest string
	binDir     string
}

// newBootstrapEnv writes a complete, internally consistent bundle plus stub
// tooling, and returns the handles a test needs to corrupt one thing at a time.
func newBootstrapEnv(t *testing.T) *bootstrapEnv {
	t.Helper()
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")

	script, err := os.ReadFile(bootstrapScript)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := os.ReadFile(filepath.Join("..", "..", "releases", "verification-policy.txt"))
	if err != nil {
		t.Fatal(err)
	}
	intentBytes, err := os.ReadFile(filepath.Join("..", "..", "releases", "intent", "v0.3.0.json"))
	if err != nil {
		t.Fatal(err)
	}
	in, err := ParseIntent(intentBytes)
	if err != nil {
		t.Fatal(err)
	}

	evidence := []byte(`{"gate":"crossversion","outcome":"passed"}`)
	files := []BundleFile{
		{Path: MemberIntent, Bytes: intentBytes},
		{Path: MemberPolicy, Bytes: policy},
		{Path: MemberComposeEnv, Bytes: []byte("NOVA_ADMIN_REF=ghcr.io/nova-archive/nova-admin@sha256:" + strings.Repeat("a", 64) + "\n")},
		{Path: MemberUpgrading, Bytes: []byte("# Upgrading to v0.3.0\n")},
		{Path: MemberBootstrap, Bytes: script},
		{Path: "evidence/crossversion.json", Bytes: evidence},
	}

	// hashes.txt is DERIVED from the payload map, so the map is built first,
	// the file rendered from it, and only then does its own digest join the map.
	// It can never disagree with the authority, because it is not one.
	payload, err := BuildPayload(files)
	if err != nil {
		t.Fatal(err)
	}
	hashes := RenderHashes(payload)
	files = append(files, BundleFile{Path: MemberHashes, Bytes: hashes})
	payload, err = BuildPayload(files)
	if err != nil {
		t.Fatal(err)
	}

	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageIndex,
		Digest:    digest.Digest("sha256:" + strings.Repeat("a", 64)),
		Size:      1024,
	}
	arts := map[string]LockedArtifact{}
	for _, name := range ArtifactNames {
		arts[name] = LockedArtifact{Repository: in.Repositories[name], Descriptor: desc}
	}
	lock := Lock{
		Schema: LockSchema, Version: in.Version,
		IntentDigest: IntentDigest(intentBytes),
		SourceCommit: "143c4590000", BuiltAt: time.Now().UTC(),
		TargetSchema: in.TargetSchema,
		Artifacts:    arts, Sidecars: in.Sidecars,
		Payload:         payload,
		DonorLockDigest: "sha256:" + strings.Repeat("7", 64),
	}
	lockBytes, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range files {
		writeUnder(t, bundle, f.Path, f.Bytes, 0o644)
	}
	// The bootstrap has to be executable where it lands, or the operator's
	// third step fails for a reason that has nothing to do with trust.
	if err := os.Chmod(filepath.Join(bundle, MemberBootstrap), 0o755); err != nil {
		t.Fatal(err)
	}
	writeUnder(t, bundle, "lock.json", lockBytes, 0o644)
	writeUnder(t, bundle, AuthRoot, []byte(`{"note":"a real Sigstore bundle goes here"}`), 0o644)

	env := &bootstrapEnv{
		bundle:     bundle,
		binDir:     filepath.Join(root, "bin"),
		dockerLog:  filepath.Join(root, "docker.log"),
		cosignLog:  filepath.Join(root, "cosign.log"),
		lockDigest: LockDigest(lockBytes),
	}
	if err := os.MkdirAll(env.binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, filepath.Join(env.binDir, "cosign"), env.cosignLog, "")
	// `docker inspect` has to answer with the digest it was asked about, or the
	// host plane would report a mismatch against nothing and every orchestrated
	// run would fail for a reason that is not about the release.
	writeStub(t, filepath.Join(env.binDir, "docker"), env.dockerLog,
		`if [ "${1:-}" = "inspect" ]; then printf '%s\n' "${!#}"; exit 0; fi`)
	return env
}

func writeUnder(t *testing.T, root, rel string, body []byte, mode os.FileMode) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, body, mode); err != nil {
		t.Fatal(err)
	}
}

// writeStub creates a tool that records that it ran and honours a per-run exit
// code, so a test can make cosign refuse without making it disappear.
func writeStub(t *testing.T, path, logPath, extra string) {
	t.Helper()
	body := "#!/usr/bin/env bash\n" +
		"printf '%s\\n' \"$*\" >> " + shellQuote(logPath) + "\n" +
		extra + "\n" +
		"exit \"${STUB_EXIT:-0}\"\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// run invokes the bundle's own copy of the bootstrap, which is what an operator
// runs and what the self-hash check is about.
func (e *bootstrapEnv) run(t *testing.T, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	full := append([]string{
		"--bundle", e.bundle, "--lock-digest", e.lockDigest,
	}, args[1:]...)
	cmd := exec.Command(filepath.Join(e.bundle, MemberBootstrap), append([]string{args[0]}, full...)...)
	cmd.Env = append(os.Environ(),
		"NOVA_RELEASE_COSIGN="+filepath.Join(e.binDir, "cosign"),
		"NOVA_RELEASE_DOCKER="+filepath.Join(e.binDir, "docker"),
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *bootstrapEnv) ranDocker() bool {
	_, err := os.Stat(e.dockerLog)
	return err == nil
}

// corrupt rewrites one member in place, leaving everything else consistent.
func (e *bootstrapEnv) corrupt(t *testing.T, rel string, body []byte) {
	t.Helper()
	full := filepath.Join(e.bundle, filepath.FromSlash(rel))
	info, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, body, info.Mode()); err != nil {
		t.Fatal(err)
	}
}

// reseal rebuilds the lock over whatever is on disk now and hands back its new
// digest, modelling an attacker who controls the WHOLE bundle — contents, lock
// and signature alike.
//
// Without this, a corrupted member is caught by the payload hash and the checks
// downstream of it never run. Those checks exist for the self-consistent case:
// a bundle that is internally perfect and still must not be trusted, because
// what it asks to be trusted against came from inside it.
func (e *bootstrapEnv) reseal(t *testing.T) {
	t.Helper()
	old, err := os.ReadFile(filepath.Join(e.bundle, "lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lock Lock
	if err := json.Unmarshal(old, &lock); err != nil {
		t.Fatal(err)
	}

	var files []BundleFile
	err = filepath.WalkDir(e.bundle, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, rerr := filepath.Rel(e.bundle, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "lock.json" || rel == AuthRoot {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		files = append(files, BundleFile{Path: rel, Bytes: b})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	payload, err := BuildPayload(files)
	if err != nil {
		t.Fatal(err)
	}
	lock.Payload = payload
	b, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.bundle, "lock.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	e.lockDigest = LockDigest(b)
}

// ---------------------------------------------------------------------------

// TestBootstrapAcceptsAnIntactBundle is the control. Without it, every refusal
// below could be a script that refuses everything.
func TestBootstrapAcceptsAnIntactBundle(t *testing.T) {
	e := newBootstrapEnv(t)
	out, err := e.run(t, nil, "verify")
	if err != nil {
		t.Fatalf("an intact bundle must verify: %v\n%s", err, out)
	}
	if !strings.Contains(out, "verify: OK") {
		t.Errorf("output does not confirm success:\n%s", out)
	}
	if !strings.Contains(out, "image signature and provenance verified") {
		t.Errorf("the image was not verified against the policy:\n%s", out)
	}
}

// TestSubstitutedBootstrapScriptRefused. The script's own hash is covered by
// the lock, so a modified copy stops before touching anything else.
func TestSubstitutedBootstrapScriptRefused(t *testing.T) {
	e := newBootstrapEnv(t)
	body, err := os.ReadFile(filepath.Join(e.bundle, MemberBootstrap))
	if err != nil {
		t.Fatal(err)
	}
	e.corrupt(t, MemberBootstrap, append(body, []byte("\n# and one more thing\n")...))

	out, err := e.run(t, nil, "verify")
	if err == nil {
		t.Fatalf("a modified bootstrap must be refused:\n%s", out)
	}
	if !strings.Contains(out, "the authenticated lock names") {
		t.Errorf("the refusal does not name the cause:\n%s", out)
	}
	if e.ranDocker() {
		t.Error("nothing may be invoked once verification has failed")
	}
}

// TestSubstitutedSignerPolicyRefused. The bundle's policy copy is auditable,
// never the authority: a difference means someone is trying to change what the
// script trusts by editing a file inside the thing being verified.
//
// The bundle here is RESEALED, so the lock covers the substituted policy and
// every hash agrees. That is the case worth testing — an internally perfect
// bundle asking to be trusted against a signer it chose for itself.
func TestSubstitutedSignerPolicyRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"a different signer",
			"issuer=https://token.actions.githubusercontent.com\n" +
				"identity=https://github.com/attacker/nova/.github/workflows/release.yml@refs/heads/main\n",
			"never the authority"},
		{"a different issuer",
			"issuer=https://accounts.google.com\n" +
				"identity=https://github.com/nova-archive/nova/.github/workflows/release.yml@refs/heads/main\n",
			"never the authority"},
		{"no policy stated at all", "# nothing here\n", "declares no issuer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newBootstrapEnv(t)
			e.corrupt(t, MemberPolicy, []byte(tc.body))
			e.reseal(t)

			out, err := e.run(t, nil, "verify")
			if err == nil {
				t.Fatalf("a substituted policy must be refused:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("refusal does not explain itself (want %q):\n%s", tc.want, out)
			}
			if e.ranDocker() {
				t.Error("nothing may be invoked once verification has failed")
			}
		})
	}
}

// TestTamperedLockRefused. Everything downstream trusts the lock, so if this
// comparison can be skipped the rest of the script verifies a document nobody
// checked. One appended byte is a different document.
func TestTamperedLockRefused(t *testing.T) {
	e := newBootstrapEnv(t)
	body, err := os.ReadFile(filepath.Join(e.bundle, "lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	e.corrupt(t, "lock.json", append(body, ' '))

	out, err := e.run(t, nil, "verify")
	if err == nil {
		t.Fatalf("a tampered lock must be refused:\n%s", out)
	}
	if !strings.Contains(out, "not the "+e.lockDigest) {
		t.Errorf("refusal does not name the digest that was expected:\n%s", out)
	}
	if e.ranDocker() {
		t.Error("nothing may be invoked once verification has failed")
	}
}

// TestTamperedPayloadMembersRefused covers the members an attacker would
// actually want: the reviewed decision, the environment the deployment reads,
// and the human-readable manifest people check by eye.
func TestTamperedPayloadMembersRefused(t *testing.T) {
	for _, member := range []string{MemberIntent, MemberComposeEnv, MemberHashes, MemberUpgrading} {
		t.Run(member, func(t *testing.T) {
			e := newBootstrapEnv(t)
			body, err := os.ReadFile(filepath.Join(e.bundle, filepath.FromSlash(member)))
			if err != nil {
				t.Fatal(err)
			}
			e.corrupt(t, member, append(body, '\n'))

			out, err := e.run(t, nil, "verify")
			if err == nil {
				t.Fatalf("a modified %s must be refused:\n%s", member, out)
			}
			if !strings.Contains(out, member+" has been modified") {
				t.Errorf("refusal does not name %s:\n%s", member, out)
			}
			if e.ranDocker() {
				t.Error("nothing may be invoked once verification has failed")
			}
		})
	}
}

// TestUncoveredMemberRefused. A file with no authenticated path is an unsigned
// file riding along, and "it was in the bundle" is not provenance.
func TestUncoveredMemberRefused(t *testing.T) {
	e := newBootstrapEnv(t)
	writeUnder(t, e.bundle, "scripts/helpful.sh", []byte("#!/bin/sh\nexit 0\n"), 0o755)

	out, err := e.run(t, nil, "verify")
	if err == nil {
		t.Fatalf("an uncovered member must be refused:\n%s", out)
	}
	if !strings.Contains(out, "the lock does not cover it") {
		t.Errorf("refusal does not explain itself:\n%s", out)
	}
}

// TestMissingMemberRefused. Deleting a member is as good as changing it if the
// deletion goes unnoticed.
func TestMissingMemberRefused(t *testing.T) {
	e := newBootstrapEnv(t)
	if err := os.Remove(filepath.Join(e.bundle, "evidence", "crossversion.json")); err != nil {
		t.Fatal(err)
	}
	out, err := e.run(t, nil, "verify")
	if err == nil {
		t.Fatalf("a missing member must be refused:\n%s", out)
	}
	if !strings.Contains(out, "but it is missing") {
		t.Errorf("refusal does not explain itself:\n%s", out)
	}
}

// TestWrongAdminDigestRefused. The digest comes from the authenticated lock, and
// the signature over it is checked against the SAME policy as the lock. A
// registry that serves something else fails here.
func TestWrongAdminDigestRefused(t *testing.T) {
	e := newBootstrapEnv(t)
	out, err := e.run(t, []string{"STUB_EXIT=1"}, "verify")
	if err == nil {
		t.Fatalf("an unverifiable image must be refused:\n%s", out)
	}
	if !strings.Contains(out, "Nova signing identity") {
		t.Errorf("refusal does not name the policy that rejected it:\n%s", out)
	}
	if e.ranDocker() {
		t.Error("the image must not be pulled or run once its signature failed")
	}

	log, err := os.ReadFile(e.cosignLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), novaIdentity) {
		t.Errorf("cosign was not given the hard-coded identity:\n%s", log)
	}
}

// TestTargetAdminNeverInvokedWhenVerificationFails is the property all of the
// above share, stated once on its own: the target artifact must not be the thing
// that authenticates the document authorizing the target artifact.
func TestTargetAdminNeverInvokedWhenVerificationFails(t *testing.T) {
	e := newBootstrapEnv(t)
	e.corrupt(t, MemberIntent, []byte("{}"))

	out, err := e.run(t, nil, "exec", "--", "upgrade", "status")
	if err == nil {
		t.Fatalf("exec on a broken bundle must be refused:\n%s", out)
	}
	if e.ranDocker() {
		t.Fatal("the target admin ran despite a failed verification")
	}
}

// TestExecRunsExactlyTheLockedDigest. Not a tag: a tag is a name someone else
// can repoint.
func TestExecRunsExactlyTheLockedDigest(t *testing.T) {
	e := newBootstrapEnv(t)
	out, err := e.run(t, nil, "exec", "--", "upgrade", "status")
	if err != nil {
		t.Fatalf("exec on an intact bundle must run: %v\n%s", err, out)
	}
	log, err := os.ReadFile(e.dockerLog)
	if err != nil {
		t.Fatalf("the target admin was never invoked: %v", err)
	}
	line := string(log)
	if !strings.Contains(line, "@sha256:") {
		t.Errorf("docker was not given a digest reference:\n%s", line)
	}
	if !strings.Contains(line, "upgrade status") {
		t.Errorf("the operator's command did not reach the image:\n%s", line)
	}
	if !strings.Contains(line, ":/release:ro") {
		t.Errorf("the bundle must be mounted read-only:\n%s", line)
	}
}

// TestSkipImageSignatureCannotRunAnything. An escape hatch that lets the
// operator audit a bundle offline must not become an escape hatch that runs
// unverified images.
func TestSkipImageSignatureCannotRunAnything(t *testing.T) {
	e := newBootstrapEnv(t)

	out, err := e.run(t, nil, "verify", "--skip-image-signature")
	if err != nil {
		t.Fatalf("auditing a bundle without a registry must be allowed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "is NOT") {
		t.Errorf("the output must say plainly what was not checked:\n%s", out)
	}

	out, err = e.run(t, nil, "exec", "--skip-image-signature", "--", "upgrade", "status")
	if err == nil {
		t.Fatalf("exec with the audit-only flag must be refused:\n%s", out)
	}
	if e.ranDocker() {
		t.Error("nothing may run on an unverified image")
	}
}

// TestVerifyPlanesCarriesOneRunIDEverywhere (P2-M7.3, D-M7.3-11). The planes
// genuinely cannot collapse into one process — host image inspection needs a
// Docker socket, nova-admin is `run --rm` — so the host orchestrates, and the
// run id is what makes the results one run rather than a pile.
func TestVerifyPlanesCarriesOneRunIDEverywhere(t *testing.T) {
	e := newBootstrapEnv(t)
	reports := filepath.Join(t.TempDir(), "reports")

	out, err := e.run(t, nil, "verify-planes", "--report-dir", reports, "--run-id", "run-42")
	if err != nil {
		t.Fatalf("verify-planes on an intact bundle must run: %v\n%s", err, out)
	}

	// The host plane is executed by the orchestrator, because only the host can
	// see what is actually running.
	body, err := os.ReadFile(filepath.Join(reports, "run-42-host.json"))
	if err != nil {
		t.Fatalf("no host-plane report: %v", err)
	}
	var host struct {
		RunID   string `json:"run_id"`
		Plane   string `json:"plane"`
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal(body, &host); err != nil {
		t.Fatal(err)
	}
	if host.RunID != "run-42" || host.Plane != "host" || host.Outcome != "passed" {
		t.Errorf("host report = %+v", host)
	}
	if entries, _ := filepath.Glob(filepath.Join(reports, "*.partial")); len(entries) > 0 {
		t.Errorf("a partial report survived: %v", entries)
	}

	log, err := os.ReadFile(e.dockerLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, plane := range []string{"admin", "coordinator", "database", "donor"} {
		want := "upgrade verify --plane " + plane + " --run-id run-42 --report-dir /reports"
		if !strings.Contains(string(log), want) {
			t.Errorf("the %s plane was not run under the shared id:\n%s", plane, log)
		}
	}
	if !strings.Contains(string(log), ":/reports") {
		t.Error("the report directory must be mounted; nova-admin runs --rm and a report on " +
			"its own filesystem evaporates exactly when it failed")
	}
}

// TestVerifyPlanesRequiresAReportDirectory. Same reason: without a mounted
// directory the evidence does not outlive the container that produced it.
func TestVerifyPlanesRequiresAReportDirectory(t *testing.T) {
	e := newBootstrapEnv(t)
	out, err := e.run(t, nil, "verify-planes")
	if err == nil {
		t.Fatalf("verify-planes without --report-dir must be refused:\n%s", out)
	}
	if !strings.Contains(out, "evaporates") {
		t.Errorf("refusal does not explain itself:\n%s", out)
	}
}

// TestLockDigestIsRequired. `--lock <file>` means nothing across a process
// boundary: a caller who can choose the path can choose the contents.
func TestLockDigestIsRequired(t *testing.T) {
	e := newBootstrapEnv(t)
	cmd := exec.Command(filepath.Join(e.bundle, MemberBootstrap), "verify", "--bundle", e.bundle)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a missing --lock-digest must be refused:\n%s", out)
	}
	if !strings.Contains(string(out), "a path is not an identity") {
		t.Errorf("refusal does not explain itself:\n%s", out)
	}
}

// ---------------------------------------------------------------------------

// The policy values, restated here so a change to either the script or the
// canonical file has to pass through a test that knows both.
const (
	novaIssuer   = "https://token.actions.githubusercontent.com"
	novaIdentity = "https://github.com/nova-archive/nova/.github/workflows/release.yml@refs/heads/main"
)

// TestPolicyMovesInOneCommit. The cosign identity policy changes with the
// workflow, and it has to change everywhere at once: the script that enforces
// it, the canonical file, and the documented bootstrap command an operator
// copies. Three copies that can drift is three chances to verify against the
// wrong signer.
func TestPolicyMovesInOneCommit(t *testing.T) {
	script, err := os.ReadFile(bootstrapScript)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := os.ReadFile(filepath.Join("..", "..", "releases", "verification-policy.txt"))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(policy), "issuer="+novaIssuer) {
		t.Errorf("%s does not declare issuer=%s", MemberPolicy, novaIssuer)
	}
	if !strings.Contains(string(policy), "identity="+novaIdentity) {
		t.Errorf("%s does not declare identity=%s", MemberPolicy, novaIdentity)
	}
	if !strings.Contains(string(script), `NOVA_ISSUER="`+novaIssuer+`"`) {
		t.Errorf("scripts/nova-release does not hard-code the issuer")
	}
	if !strings.Contains(string(script), `NOVA_IDENTITY="`+novaIdentity+`"`) {
		t.Errorf("scripts/nova-release does not hard-code the identity")
	}

	// The documented step-1 command is what runs BEFORE anything from the
	// bundle executes, so it is the copy that matters most.
	if !strings.Contains(string(script), "--certificate-identity "+novaIdentity) {
		t.Error("the documented cosign verify-blob command does not carry the identity, so an " +
			"operator following it would verify against whatever cosign defaults to")
	}
	if !strings.Contains(string(script), "--certificate-oidc-issuer "+novaIssuer) {
		t.Error("the documented cosign verify-blob command does not carry the issuer")
	}
}

// TestBootstrapIsCoveredByEveryLock. The script can only be authenticated if
// the lock names it, so it is a required member rather than an optional one.
func TestBootstrapIsCoveredByEveryLock(t *testing.T) {
	if _, err := BuildPayload([]BundleFile{
		{Path: MemberIntent, Bytes: []byte("{}")},
		{Path: MemberPolicy, Bytes: []byte("x")},
		{Path: MemberComposeEnv, Bytes: []byte("x")},
		{Path: MemberUpgrading, Bytes: []byte("x")},
	}); err == nil {
		t.Fatal("a payload map with no scripts/nova-release entry must be rejected: the " +
			"bootstrap cannot authenticate itself against a lock that does not name it")
	}
}
