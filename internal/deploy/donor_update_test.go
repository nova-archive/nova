package deploy

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The volunteer's update script (P2-M7.3, D-M7.3-15).
//
// Driven as a real subprocess with a stub docker, because every property worth
// asserting here is about WHAT IT RUNS and IN WHAT ORDER — which cannot be read
// off the source without re-implementing the shell.

// donorUpdateEnv renders the script into a scratch bundle, alongside a lock and
// a stub docker that records its invocations.
type donorUpdateEnv struct {
	dir       string
	script    string
	dockerLog string
	dockerBin string
	lock      string
}

func newDonorUpdateEnv(t *testing.T, safe map[string]bool, predecessor string) *donorUpdateEnv {
	t.Helper()
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}

	files, err := RenderDonorBundle(DonorParams{
		Name: "t", NebulaIP: "10.42.0.10/24", CoordinatorOverlayIP: "10.42.0.1",
		NodeImage:   "ghcr.io/nova-archive/nova-node@sha256:" + strings.Repeat("a", 64),
		NebulaImage: DefaultNebulaImage, KuboImage: DefaultKuboImage,
		BandwidthBudgetBytesPerDay: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(bundle, "donor-update.sh")
	if err := os.WriteFile(script, files["donor-update.sh"], 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "compose.yaml"), files["compose.yaml"], 0o644); err != nil {
		t.Fatal(err)
	}

	comp := map[string]any{}
	for _, name := range []string{"nova-node", "nebula", "kubo"} {
		comp[name] = map[string]any{
			"ref": "ghcr.io/nova-archive/" + name + "@sha256:" + strings.Repeat("b", 64),
			"rollback": map[string]any{
				"safe": safe[name], "predecessor": predecessor,
				"reason": "test fixture",
			},
		}
	}
	body, err := json.MarshalIndent(map[string]any{
		"schema": 1, "release": "v0.3.0",
		"release_lock_digest": "sha256:" + strings.Repeat("c", 64),
		"components":          comp,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(bundle, "donor-lock.json")
	if err := os.WriteFile(lock, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	dockerLog := filepath.Join(root, "docker.log")
	// The stub records every invocation IN ORDER and answers `compose ps` so
	// the health wait terminates.
	stub := "#!/usr/bin/env bash\n" +
		"printf '%s\\n' \"$*\" >> '" + dockerLog + "'\n" +
		"if [ \"${2:-}\" = ps ]; then echo 'nova-node healthy'; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	return &donorUpdateEnv{
		dir: bundle, script: script, dockerLog: dockerLog,
		dockerBin: filepath.Join(bin, "docker"), lock: lock,
	}
}

func (e *donorUpdateEnv) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(e.script, args...)
	cmd.Dir = e.dir
	cmd.Env = append(os.Environ(), "NOVA_DONOR_DOCKER="+e.dockerBin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *donorUpdateEnv) dockerCalls(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(e.dockerLog)
	if err != nil {
		return nil
	}
	var out []string
	for l := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// ---------------------------------------------------------------------------

// TestUpdatePersistsRefBeforeRecreate. Compose reads .env when it renders,
// which happens at `up`. Writing the refs afterwards recreates the containers
// on the OLD digest and then leaves a file claiming the new one — an update
// that reports success and changed nothing.
func TestUpdatePersistsRefBeforeRecreate(t *testing.T) {
	e := newDonorUpdateEnv(t, nil, "")

	// A docker stub that captures .env AS IT WAS when `up` ran. Reading the
	// file afterwards cannot distinguish "written first" from "written last".
	captured := filepath.Join(e.dir, "..", "env-at-up.txt")
	stub := "#!/usr/bin/env bash\n" +
		"printf '%s\\n' \"$*\" >> '" + e.dockerLog + "'\n" +
		"if [ \"${2:-}\" = up ]; then cp .env '" + captured + "' 2>/dev/null || true; fi\n" +
		"if [ \"${2:-}\" = ps ]; then echo 'nova-node healthy'; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(e.dockerBin, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := e.run(t, "apply")
	if err != nil {
		t.Fatalf("apply failed: %v\n%s", err, out)
	}

	b, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf(".env did not exist when `compose up` ran: %v\n%s", err, out)
	}
	if !strings.Contains(string(b), "NOVA_NODE_REF=ghcr.io/nova-archive/nova-node@sha256:bbbb") {
		t.Errorf(".env at `up` time did not carry the locked ref:\n%s", b)
	}
}

// TestUpdateNeverRemovesVolumes. `down -v` on this topology deletes the Kubo
// blockstore holding the replicas and the node-data volume holding the
// registration. An update that can destroy the thing being updated is not one.
func TestUpdateNeverRemovesVolumes(t *testing.T) {
	e := newDonorUpdateEnv(t, map[string]bool{"nova-node": true, "nebula": true, "kubo": true},
		"ghcr.io/nova-archive/old@sha256:"+strings.Repeat("d", 64))

	for _, action := range []string{"check", "apply", "rollback"} {
		if _, err := e.run(t, action); err != nil && action != "check" {
			t.Logf("%s: %v", action, err)
		}
	}
	for _, call := range e.dockerCalls(t) {
		if strings.Contains(call, " -v") || strings.Contains(call, "down") {
			t.Errorf("the update script invoked `docker %s`; volumes are the replicas", call)
		}
	}

	// And the source must not contain it either, for the reader who greps.
	b, err := os.ReadFile(e.script)
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "down -v") || strings.Contains(trimmed, "volume rm") {
			t.Errorf("donor-update.sh contains a volume-destroying command: %s", trimmed)
		}
	}
}

// TestCheckChangesNothing. An operator tells a volunteer to run `check` first;
// if that mutated anything, the answer would be about a state it created.
func TestCheckChangesNothing(t *testing.T) {
	e := newDonorUpdateEnv(t, nil, "")
	out, err := e.run(t, "check")
	if err != nil {
		t.Fatalf("check failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(e.dir, ".env")); err == nil {
		t.Error("check wrote .env")
	}
	if calls := e.dockerCalls(t); len(calls) > 0 {
		t.Errorf("check invoked docker: %v", calls)
	}
}

// TestRollbackRequiresPerComponentEvidence. The topology holds node state, a
// persistent Kubo repo and Nebula config; reverting image refs is only safe
// where something says so, and that is a different answer per component.
func TestRollbackRequiresPerComponentEvidence(t *testing.T) {
	pred := "ghcr.io/nova-archive/old@sha256:" + strings.Repeat("d", 64)
	e := newDonorUpdateEnv(t, map[string]bool{"nova-node": true, "nebula": true, "kubo": true}, pred)

	out, err := e.run(t, "rollback")
	if err != nil {
		t.Fatalf("a fully-evidenced rollback must proceed: %v\n%s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(e.dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"NOVA_NODE_REF", "NOVA_NEBULA_REF", "NOVA_KUBO_REF"} {
		if !strings.Contains(string(b), v+"="+pred) {
			t.Errorf("%s was not reverted to the evidenced predecessor:\n%s", v, b)
		}
	}
}

// TestUnsafeRollbackStopsAndReports. It preserves volumes AND both lock files,
// emits a recovery report, and never starts older software against state newer
// software may already have written.
func TestUnsafeRollbackStopsAndReports(t *testing.T) {
	pred := "ghcr.io/nova-archive/old@sha256:" + strings.Repeat("d", 64)
	// nova-node evidenced, kubo NOT. A partial rollback is still a rollback of
	// the unevidenced component's peers, and must not happen piecemeal.
	e := newDonorUpdateEnv(t, map[string]bool{"nova-node": true, "nebula": true}, pred)

	out, err := e.run(t, "rollback")
	if err == nil {
		t.Fatalf("an unevidenced component must block the rollback:\n%s", out)
	}
	if !strings.Contains(out, "ROLLBACK BLOCKED") {
		t.Errorf("the refusal is not legible:\n%s", out)
	}

	reports, _ := filepath.Glob(filepath.Join(e.dir, "rollback-blocked-*.txt"))
	if len(reports) != 1 {
		t.Fatalf("expected exactly one recovery report, got %v", reports)
	}
	body, err := os.ReadFile(reports[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "kubo") {
		t.Errorf("the report does not name the component that blocked it:\n%s", body)
	}
	if !strings.Contains(string(body), "Nothing was changed") {
		t.Errorf("the report does not say the deployment is untouched:\n%s", body)
	}

	// Nothing ran, nothing was written, and both files survive.
	if calls := e.dockerCalls(t); len(calls) > 0 {
		t.Errorf("a blocked rollback invoked docker: %v", calls)
	}
	if _, err := os.Stat(filepath.Join(e.dir, ".env.next")); err == nil {
		t.Error("a partial .env.next survived a blocked rollback")
	}
	if _, err := os.Stat(e.lock); err != nil {
		t.Error("donor-lock.json did not survive a blocked rollback")
	}
}

// TestUpdateRefusesTargetsOutsideTheVerifiedLock. The digest is the identity: a
// lock file whose bytes are not the ones the operator authorized names refs
// nobody approved.
func TestUpdateRefusesTargetsOutsideTheVerifiedLock(t *testing.T) {
	e := newDonorUpdateEnv(t, nil, "")

	out, err := e.run(t, "apply", "--expect-lock-digest", "sha256:"+strings.Repeat("f", 64))
	if err == nil {
		t.Fatalf("a lock that does not hash to the operator's digest must be refused:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(e.dir, ".env")); err == nil {
		t.Error("the refusal still wrote .env")
	}
	if calls := e.dockerCalls(t); len(calls) > 0 {
		t.Errorf("the refusal still invoked docker: %v", calls)
	}
}

// TestUpdateWarnsWithoutADigest. Running without one is allowed — a volunteer
// whose operator forgot to send it should not be stuck — but it is not silent,
// because "the lock says so" means nothing about an unverified lock.
func TestUpdateWarnsWithoutADigest(t *testing.T) {
	e := newDonorUpdateEnv(t, nil, "")
	out, err := e.run(t, "check")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "WARNING: no --expect-lock-digest") {
		t.Errorf("no warning about an unverified lock:\n%s", out)
	}
}

// TestVolunteerScriptContainsNoAdminCommands. Two parties: the volunteer's
// script holds no coordinator admin authority. A script on a donor machine that
// could reconfigure the federation would be a federation with as many
// administrators as it has donors.
func TestVolunteerScriptContainsNoAdminCommands(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("templates", "donor-update.sh.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	// The test is about what the script CAN DO, not what it can mention. It has
	// to be able to tell a volunteer "your operator runs novactl node
	// convert-bundle" — that sentence is the two-party split being explained,
	// not violated. So the check is for INVOCATION positions.
	invoke := regexp.MustCompile(`(^|[;&|(\x60]|\$\()\s*(sudo\s+)?novactl\b`)
	for line := range strings.SplitSeq(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if invoke.MatchString(trimmed) {
			t.Errorf("the volunteer's script INVOKES novactl: %s", trimmed)
		}
		for _, forbidden := range []string{"DATABASE_URL", "/api/v1/admin", "--no-confirm"} {
			if strings.Contains(trimmed, forbidden) {
				t.Errorf("the volunteer's script references %q: %s", forbidden, trimmed)
			}
		}
	}
	// And it must carry no coordinator admin credential of any kind.
	if strings.Contains(string(b), "NOVA_ADMIN") {
		t.Error("the volunteer's script references an admin credential")
	}
}

// TestDonorBundleCarriesTheUpdateScript. A bundle without it is a donor who has
// to be sent a new bundle to change one digest.
func TestDonorBundleCarriesTheUpdateScript(t *testing.T) {
	files, err := RenderDonorBundle(DonorParams{
		Name: "t", NebulaIP: "10.42.0.10/24", CoordinatorOverlayIP: "10.42.0.1",
		NodeImage:   "ghcr.io/nova-archive/nova-node@sha256:" + strings.Repeat("a", 64),
		NebulaImage: DefaultNebulaImage, KuboImage: DefaultKuboImage,
		BandwidthBudgetBytesPerDay: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, ok := files["donor-update.sh"]
	if !ok {
		t.Fatal("no donor-update.sh in the rendered bundle")
	}
	if !strings.HasPrefix(string(body), "#!") {
		t.Error("donor-update.sh has no shebang")
	}

	// And the compose file takes its refs from .env, which is what makes an
	// update possible without reissuing the bundle.
	compose := string(files["compose.yaml"])
	for _, v := range []string{"${NOVA_NODE_REF:-", "${NOVA_NEBULA_REF:-", "${NOVA_KUBO_REF:-"} {
		if !strings.Contains(compose, v) {
			t.Errorf("compose.yaml does not read %s; the refs are baked in and cannot be updated", v)
		}
	}
	// The defaults are still the issued digests, so a bundle with no .env runs
	// exactly what the operator sent.
	if !strings.Contains(compose, "@sha256:"+strings.Repeat("a", 64)+"}") {
		t.Error("compose.yaml lost the issued digest as its default")
	}
}
