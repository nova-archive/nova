package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nova-archive/nova/internal/federation/bootstrap"
	"github.com/nova-archive/nova/internal/release"
)

// Donor bundle v2, by conversion (P2-M7.3, D-M7.3-15).
//
// The property under test throughout is that conversion is NOT a reissue.
// `node invite` mints a new UUID and new certificates; for an existing donor
// that is a re-enrollment, and the whole track exists to make an update not
// look like one.

// v1Bundle writes a bundle in the shape `node invite` produced before this
// milestone, and returns its directory plus a snapshot of every file.
func v1Bundle(t *testing.T) (string, map[string][]byte) {
	t.Helper()
	dir := t.TempDir()

	mf := bootstrap.InviteManifest{
		Version:        bootstrap.BundleSchemaV1,
		NodeID:         "19e8f7b9-2ddc-4d12-8260-14dea6d39edf",
		DisplayName:    "volunteer-one",
		Fingerprint:    "ab:cd",
		NebulaIP:       "10.42.0.10/24",
		CoordinatorURL: "https://10.42.0.1:9443",
		NodeImage:      "ghcr.io/nova-archive/nova-node@sha256:" + strings.Repeat("9", 64),
		NebulaImage:    "nebulaoss/nebula:1.11.0@sha256:" + strings.Repeat("8", 64),
		KuboImage:      "ipfs/kubo:v0.38.1@sha256:" + strings.Repeat("7", 64),
		CAFingerprint:  "ca:fp",
		SwarmKeyFP:     "swarm:fp",
	}
	body, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	files := map[string][]byte{
		"invite-manifest.json":             append(body, '\n'),
		"compose.yaml":                     []byte("name: nova-donor-volunteer-one\n"),
		"node.yaml":                        []byte("coordinator_url: https://10.42.0.1:9443\n"),
		"federation/federation.crt":        []byte("-----BEGIN CERTIFICATE-----\nnode\n-----END CERTIFICATE-----\n"),
		"federation/federation-ca.crt":     []byte("-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----\n"),
		"secrets/nova_node_federation_key": []byte("-----BEGIN PRIVATE KEY-----\nnode\n-----END PRIVATE KEY-----\n"),
		"nebula/nebula.crt":                []byte("nebula cert\n"),
		"secrets/ipfs_swarm_key":           []byte("/key/swarm/psk/1.0.0/\n"),
	}
	for name, b := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, files
}

// snapshot reads every file under dir, keyed by slash-relative path.
func snapshot(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		out[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func convert(t *testing.T, dir string) error {
	t.Helper()
	intentPath, lockPath, dgst := rolloutFixture(t)
	return cmdNodeConvertBundle([]string{
		"--bundle", dir, "--lock", lockPath, "--intent", intentPath,
		"--expect-lock-digest", dgst,
	})
}

// ---------------------------------------------------------------------------

// TestManifestVersionBumpsToTwo. InviteManifest already carried a Version, so
// this is a bump rather than a new field — the bundle format was versioned from
// the start and this is the first time that mattered.
func TestManifestVersionBumpsToTwo(t *testing.T) {
	dir, _ := v1Bundle(t)
	if err := convert(t, dir); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "invite-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var mf bootstrap.InviteManifest
	if err := json.Unmarshal(b, &mf); err != nil {
		t.Fatal(err)
	}
	if mf.Version != bootstrap.BundleSchemaV2 {
		t.Errorf("manifest version = %d, want %d", mf.Version, bootstrap.BundleSchemaV2)
	}
	if mf.DonorLockDigest == "" {
		t.Error("the manifest records no donor-lock digest, so nothing joins the bundle to " +
			"what the operator authorized")
	}
}

// TestConvertPreservesNodeIDAndBothCertificates. Reissuing mints a new UUID and
// new certificates: a new identity, an empty registration, and a fleet in which
// the volunteer's replicas belong to a node that no longer exists.
func TestConvertPreservesNodeIDAndBothCertificates(t *testing.T) {
	dir, before := v1Bundle(t)
	if err := convert(t, dir); err != nil {
		t.Fatal(err)
	}
	after := snapshot(t, dir)

	for _, keep := range []string{
		"federation/federation.crt", "federation/federation-ca.crt",
		"secrets/nova_node_federation_key", "nebula/nebula.crt",
		"secrets/ipfs_swarm_key", "compose.yaml", "node.yaml",
	} {
		if string(after[keep]) != string(before[keep]) {
			t.Errorf("%s changed during conversion; an update must not touch identity or state", keep)
		}
	}

	var mf bootstrap.InviteManifest
	if err := json.Unmarshal(after["invite-manifest.json"], &mf); err != nil {
		t.Fatal(err)
	}
	if mf.NodeID != "19e8f7b9-2ddc-4d12-8260-14dea6d39edf" {
		t.Errorf("node id changed to %q; that is a re-enrollment, not an update", mf.NodeID)
	}
	if mf.Fingerprint != "ab:cd" {
		t.Errorf("certificate fingerprint changed to %q", mf.Fingerprint)
	}
}

// TestConvertEmitsOnlyLockAndUpdateFiles.
func TestConvertEmitsOnlyLockAndUpdateFiles(t *testing.T) {
	dir, before := v1Bundle(t)
	if err := convert(t, dir); err != nil {
		t.Fatal(err)
	}
	after := snapshot(t, dir)

	var added []string
	for name := range after {
		if _, existed := before[name]; !existed {
			added = append(added, name)
		}
	}
	slices.Sort(added)
	want := []string{"donor-lock.json", "donor-update.sh"}
	if !slices.Equal(added, want) {
		t.Errorf("conversion added %v, want exactly %v", added, want)
	}
	for name := range before {
		if _, still := after[name]; !still {
			t.Errorf("conversion removed %s", name)
		}
	}

	info, err := os.Stat(filepath.Join(dir, "donor-update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("donor-update.sh is not executable; the volunteer is told to run it")
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, "*.partial")); len(entries) > 0 {
		t.Errorf("a partial file survived: %v", entries)
	}
}

// TestDonorLockCoversNodeNebulaAndKubo. One reviewed topology: three images,
// each with its own rollback verdict.
func TestDonorLockCoversNodeNebulaAndKubo(t *testing.T) {
	dir, _ := v1Bundle(t)
	if err := convert(t, dir); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "donor-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	dl, err := release.ParseDonorLock(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range release.ComponentNames {
		c, ok := dl.Components[name]
		if !ok {
			t.Fatalf("the donor lock does not cover %s", name)
		}
		if !strings.Contains(c.Ref, "@sha256:") {
			t.Errorf("%s ref %q is not digest-pinned", name, c.Ref)
		}
		if c.Rollback.Safe {
			t.Errorf("%s claims rollback is safe; no release declares that evidence yet, and "+
				"the default has to be no rather than probably", name)
		}
		if c.Rollback.Reason == "" {
			t.Errorf("%s states no reason", name)
		}
	}
	if dl.IntentDigest == "" {
		t.Error("the donor lock does not name the reviewed intent it realizes")
	}
	// It must NOT carry the release lock's digest: the lock records THIS
	// document's digest, and two documents cannot each be an input to the other.
	if strings.Contains(string(b), "release_lock_digest") {
		t.Error("the donor lock records the release lock's digest, which the release lock " +
			"cannot then hash without a cycle")
	}
}

// TestDonorLockIsIdenticalForEveryNode. It carries no per-node identity, which
// is what lets the release lock record ONE digest for the whole fleet and an
// operator authorize a rollout once rather than per volunteer.
func TestDonorLockIsIdenticalForEveryNode(t *testing.T) {
	first, _ := v1Bundle(t)
	second, _ := v1Bundle(t)

	// Give the second bundle a different identity in every field that has one.
	b, err := os.ReadFile(filepath.Join(second, "invite-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var mf bootstrap.InviteManifest
	if err := json.Unmarshal(b, &mf); err != nil {
		t.Fatal(err)
	}
	mf.NodeID = "7c1d0f22-0000-4d12-8260-14dea6d39edf"
	mf.DisplayName = "volunteer-two"
	mf.NebulaIP = "10.42.0.11/24"
	out, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "invite-manifest.json"), append(out, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := convert(t, first); err != nil {
		t.Fatal(err)
	}
	if err := convert(t, second); err != nil {
		t.Fatal(err)
	}

	a, err := os.ReadFile(filepath.Join(first, "donor-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := os.ReadFile(filepath.Join(second, "donor-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(c) {
		t.Errorf("two nodes produced different donor locks:\n--- one ---\n%s\n--- two ---\n%s", a, c)
	}
}

// TestV1BundleKeepsWorkingUnconverted. A volunteer who never converts is not
// broken by this milestone; they simply cannot be updated in place, which is
// the state they were already in.
func TestV1BundleKeepsWorkingUnconverted(t *testing.T) {
	dir, before := v1Bundle(t)
	after := snapshot(t, dir)
	if len(after) != len(before) {
		t.Fatalf("the fixture is not a faithful v1 bundle")
	}

	var mf bootstrap.InviteManifest
	if err := json.Unmarshal(after["invite-manifest.json"], &mf); err != nil {
		t.Fatal(err)
	}
	if mf.Version != bootstrap.BundleSchemaV1 {
		t.Fatalf("fixture version = %d", mf.Version)
	}
	if _, err := os.Stat(filepath.Join(dir, "donor-lock.json")); err == nil {
		t.Error("an unconverted bundle must not carry a donor lock")
	}
}

// TestConvertRefusesSomethingThatIsNotABundle. Writing new files into a
// directory that is not a donor bundle is how an operator ends up with a
// half-converted something.
func TestConvertRefusesSomethingThatIsNotABundle(t *testing.T) {
	empty := t.TempDir()
	if err := convert(t, empty); err == nil {
		t.Fatal("an empty directory must be refused")
	}

	// A manifest but no identity material.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "invite-manifest.json"),
		[]byte(`{"version":1,"node_id":"19e8f7b9-2ddc-4d12-8260-14dea6d39edf"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := convert(t, dir)
	if err == nil || !strings.Contains(err.Error(), "cannot rebuild") {
		t.Fatalf("err = %v, want a refusal that says conversion preserves rather than rebuilds", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "donor-lock.json")); statErr == nil {
		t.Error("the refusal still wrote a donor lock")
	}
}

// TestConvertRequiresAVerifiedReleaseLock. The refs a volunteer is told to run
// come from a document the operator authenticated, not from a path.
func TestConvertRequiresAVerifiedReleaseLock(t *testing.T) {
	dir, _ := v1Bundle(t)
	intentPath, lockPath, _ := rolloutFixture(t)

	err := cmdNodeConvertBundle([]string{
		"--bundle", dir, "--lock", lockPath, "--intent", intentPath,
		"--expect-lock-digest", "sha256:" + strings.Repeat("f", 64),
	})
	if err == nil {
		t.Fatal("an unverified release lock must be refused")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "donor-lock.json")); statErr == nil {
		t.Error("the refusal still wrote a donor lock")
	}

	for _, args := range [][]string{
		{"--lock", lockPath, "--intent", intentPath, "--expect-lock-digest", "x"},
		{"--bundle", dir, "--intent", intentPath, "--expect-lock-digest", "x"},
		{"--bundle", dir, "--lock", lockPath, "--expect-lock-digest", "x"},
		{"--bundle", dir, "--lock", lockPath, "--intent", intentPath},
	} {
		if err := cmdNodeConvertBundle(args); err == nil {
			t.Errorf("%v was accepted with an input missing", args)
		}
	}
}
