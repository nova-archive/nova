package release

import (
	"slices"
	"strings"
	"testing"
)

func payloadFiles() []BundleFile {
	return []BundleFile{
		{Path: MemberIntent, Bytes: []byte(`{"schema":1}`)},
		{Path: MemberPolicy, Bytes: []byte("issuer=...\nidentity=...\n")},
		{Path: MemberComposeEnv, Bytes: []byte("NOVA_COORDINATOR_IMAGE=ghcr.io/...@sha256:...\n")},
		{Path: MemberUpgrading, Bytes: []byte("# Upgrading to v0.3.0\n")},
		{Path: MemberBootstrap, Bytes: []byte("#!/usr/bin/env bash\n")},
		{Path: "evidence/upgrade-wire-e2e.json", Bytes: []byte(`{"outcome":"passed"}`)},
	}
}

// TestBundleContainsEveryRequiredMember.
func TestBundleContainsEveryRequiredMember(t *testing.T) {
	files := payloadFiles()
	p, err := BuildPayload(files)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range requiredMembers {
		if _, ok := p[m]; !ok {
			t.Errorf("payload does not cover %s", m)
		}
	}

	for i := range files {
		short := slices.Delete(slices.Clone(files), i, i+1)
		if _, err := BuildPayload(short); err == nil && slices.Contains(requiredMembers, files[i].Path) {
			t.Errorf("omitting %s must be refused", files[i].Path)
		}
	}
}

// TestComposeEnvIsCoveredByTheLock. Signing the description while the
// deployment reads something else is the whole attack.
func TestComposeEnvIsCoveredByTheLock(t *testing.T) {
	if !slices.Contains(requiredMembers, MemberComposeEnv) {
		t.Fatal("the environment file the operator's Compose actually reads must be a payload member")
	}
	p, err := BuildPayload(payloadFiles())
	if err != nil {
		t.Fatal(err)
	}
	if p[MemberComposeEnv] == "" {
		t.Fatal("release.env has no authenticated path")
	}
}

// TestHashManifestIsDerivedAndItselfCoveredByTheLock. An attacker must not be
// able to alter a file and its own hash entry together — so hashes.txt is
// derived from the payload map rather than being a second source, and the
// payload map covers it.
func TestHashManifestIsDerivedAndCoveredByTheLock(t *testing.T) {
	p, err := BuildPayload(payloadFiles())
	if err != nil {
		t.Fatal(err)
	}
	hashes := BundleFile{Path: MemberHashes, Bytes: RenderHashes(p)}

	full, err := BuildPayload(append(payloadFiles(), hashes))
	if err != nil {
		t.Fatal(err)
	}
	if full[MemberHashes] == "" {
		t.Error("hashes.txt must itself be covered by the lock")
	}

	// It is derived: every entry restates the payload map, and it never lists
	// itself (which it could not, without a fixed point).
	text := string(hashes.Bytes)
	if strings.Contains(text, MemberHashes) {
		t.Error("hashes.txt lists itself")
	}
	for path, h := range p {
		if !strings.Contains(text, strings.TrimPrefix(h, "sha256:")) {
			t.Errorf("hashes.txt omits %s", path)
		}
	}
	if !strings.Contains(text, "NOT an authority") {
		t.Error("hashes.txt must say plainly that it is derived; a reader who verifies against " +
			"it instead of against the lock has verified nothing")
	}
}

func TestRenderHashesIsByteStable(t *testing.T) {
	p, err := BuildPayload(payloadFiles())
	if err != nil {
		t.Fatal(err)
	}
	first := string(RenderHashes(p))
	for range 20 {
		if string(RenderHashes(p)) != first {
			t.Fatal("RenderHashes is not deterministic")
		}
	}
}

// TestBuildPayloadRefusesToCreateACycle: the rule is enforced where the map is
// BUILT, not only where it is validated, so a caller cannot hand in the wrong
// file list and get a cyclic bundle.
func TestBuildPayloadRefusesToCreateACycle(t *testing.T) {
	for _, bad := range excludedFromPayload {
		files := append(payloadFiles(), BundleFile{Path: bad, Bytes: []byte("x")})
		_, err := BuildPayload(files)
		if err == nil || !strings.Contains(err.Error(), "cyclic") {
			t.Errorf("%s: err = %v, want a cyclic-graph refusal", bad, err)
		}
	}
}

// TestTamperedMemberIsDetected.
func TestTamperedMemberIsDetected(t *testing.T) {
	files := payloadFiles()
	p, err := BuildPayload(files)
	if err != nil {
		t.Fatal(err)
	}
	l := Lock{Payload: p}

	if err := VerifyPayload(l, files); err != nil {
		t.Fatalf("the untampered bundle must verify: %v", err)
	}

	for i := range files {
		tampered := slices.Clone(files)
		tampered[i] = BundleFile{Path: files[i].Path, Bytes: append(slices.Clone(files[i].Bytes), '!')}
		err := VerifyPayload(l, tampered)
		if err == nil || !strings.Contains(err.Error(), files[i].Path) {
			t.Errorf("tampering with %s: err = %v, want it named", files[i].Path, err)
		}
	}
}

func TestUncoveredAndMissingMembersAreBothRefused(t *testing.T) {
	files := payloadFiles()
	p, err := BuildPayload(files)
	if err != nil {
		t.Fatal(err)
	}
	l := Lock{Payload: p}

	extra := append(slices.Clone(files), BundleFile{Path: "extra.sh", Bytes: []byte("rm -rf /")})
	if err := VerifyPayload(l, extra); err == nil {
		t.Error("a file with no authenticated path is an unsigned file riding along")
	}

	if err := VerifyPayload(l, files[:len(files)-1]); err == nil {
		t.Error("a covered member that is missing must be refused")
	}
}

// TestEveryDeployablePayloadByteHasExactlyOneAuthenticatedPath makes the
// design's graph diagram executable: not zero, and not two.
func TestEveryDeployablePayloadByteHasExactlyOneAuthenticatedPath(t *testing.T) {
	files := payloadFiles()
	p, err := BuildPayload(files)
	if err != nil {
		t.Fatal(err)
	}
	hashes := BundleFile{Path: MemberHashes, Bytes: RenderHashes(p)}
	p, err = BuildPayload(append(files, hashes))
	if err != nil {
		t.Fatal(err)
	}
	l := Lock{Payload: p}

	members := []string{AuthRoot, "lock.json"}
	for _, f := range append(files, hashes) {
		members = append(members, f.Path)
	}
	paths := AuthPaths(l, members)

	for _, f := range append(files, hashes) {
		if got := paths[f.Path]; got != 1 {
			t.Errorf("%s has %d authenticated paths, want exactly 1", f.Path, got)
		}
	}
	if paths["lock.json"] != 1 {
		t.Error("the lock must be authenticated by the Sigstore bundle")
	}
	if paths[AuthRoot] != 0 {
		t.Errorf("%s is the ROOT; nothing in the bundle may authenticate it, or the anchor is "+
			"no longer out-of-band", AuthRoot)
	}
}

func TestBuildPayloadRejectsUnsafePaths(t *testing.T) {
	for _, bad := range []string{"../escape", "/etc/passwd", "./intent.json"} {
		_, err := BuildPayload(append(payloadFiles(), BundleFile{Path: bad, Bytes: []byte("x")}))
		if err == nil {
			t.Errorf("%q must be rejected as a member path", bad)
		}
	}
}
