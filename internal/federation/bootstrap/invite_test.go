package bootstrap_test

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nova-archive/nova/internal/federation/bootstrap"
)

const pinnedImage = "ghcr.io/nova-archive/nova-node@sha256:" +
	"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func inviteParams(root, out string) bootstrap.InviteParams {
	return bootstrap.InviteParams{
		Root: root, OutDir: out, Name: "alice-desktop",
		NebulaIP: "10.42.0.10/24", NodeImage: pinnedImage,
	}
}

func bootstrappedRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if _, err := bootstrap.Init(baseParams(root)); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestInvite_NeverContainsOperatorSecrets is the assertion that matters most.
// An operator cannot un-send a leaked CA key.
func TestInvite_NeverContainsOperatorSecrets(t *testing.T) {
	root, out := bootstrappedRoot(t), t.TempDir()
	if _, err := bootstrap.Invite(inviteParams(root, out)); err != nil {
		t.Fatal(err)
	}

	active := bootstrap.ActiveDir(root)
	forbidden := map[string][]byte{
		"federation CA key":      mustRead(t, filepath.Join(active, bootstrap.FileFederationCAKey)),
		"nebula CA key":          mustRead(t, filepath.Join(active, bootstrap.FileNebulaCAKey)),
		"repair signing key":     mustRead(t, filepath.Join(active, bootstrap.FileRepairSigningKey)),
		"coordinator server key": mustRead(t, filepath.Join(active, bootstrap.FileCoordinatorKey)),
		"coordinator client key": mustRead(t, filepath.Join(active, bootstrap.FileClientKey)),
	}

	err := filepath.WalkDir(out, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body := mustRead(t, p)
		for label, secret := range forbidden {
			if len(secret) > 0 && bytes.Contains(body, bytes.TrimSpace(secret)) {
				rel, _ := filepath.Rel(out, p)
				t.Errorf("%s leaked into the invite at %s", label, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestInvite_RefusesToWriteWhenASecretWouldLeak proves the guard is a RUNTIME
// refusal, not merely a test assertion: nothing is written at all.
func TestInvite_RefusesToWriteWhenASecretWouldLeak(t *testing.T) {
	root, out := bootstrappedRoot(t), t.TempDir()
	active := bootstrap.ActiveDir(root)

	// Simulate the catastrophic mistake: the CA key content ends up where the
	// donor's own key belongs.
	caKey := mustRead(t, filepath.Join(active, bootstrap.FileFederationCAKey))
	if err := os.WriteFile(filepath.Join(active, bootstrap.FileSwarmKey), caKey, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := bootstrap.Invite(inviteParams(root, out))
	if err == nil {
		t.Fatal("a bundle carrying the CA key must be refused")
	}
	if !strings.Contains(err.Error(), "REFUSING") {
		t.Fatalf("error should be an explicit refusal, got %v", err)
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 0 {
		t.Fatalf("nothing may be written when the assertion fails, found %d entries", len(entries))
	}
}

func TestInvite_RequiresADigestPinnedImage(t *testing.T) {
	root, out := bootstrappedRoot(t), t.TempDir()
	p := inviteParams(root, out)
	p.NodeImage = "ghcr.io/nova-archive/nova-node:latest"

	_, err := bootstrap.Invite(p)
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("a mutable tag must be refused, got %v", err)
	}
}

func TestInvite_ProducesACompleteRunnableBundle(t *testing.T) {
	root, out := bootstrappedRoot(t), t.TempDir()
	res, err := bootstrap.Invite(inviteParams(root, out))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"compose.yaml", "node.yaml", "nebula-config.yml", "kubo-init.sh", "README.md",
		"invite-manifest.json",
		"federation/federation-ca.crt", "federation/federation.crt",
		"nebula/nebula-ca.crt",
		"secrets/ipfs_swarm_key", "secrets/nova_node_federation_key",
	} {
		if _, err := os.Stat(filepath.Join(out, want)); err != nil {
			t.Errorf("bundle is missing %s", want)
		}
	}
	if res.NodeID == "" || res.Fingerprint == "" {
		t.Fatalf("result not populated: %+v", res)
	}
}

func TestInvite_SecretsAreNotWorldReadable(t *testing.T) {
	root, out := bootstrappedRoot(t), t.TempDir()
	if _, err := bootstrap.Invite(inviteParams(root, out)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(out, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		info, _ := e.Info()
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("secrets/%s mode = %o, want no group/other access", e.Name(), mode)
		}
	}
}

func TestInvite_ManifestRecordsPinnedDigests(t *testing.T) {
	root, out := bootstrappedRoot(t), t.TempDir()
	if _, err := bootstrap.Invite(inviteParams(root, out)); err != nil {
		t.Fatal(err)
	}
	var mf bootstrap.InviteManifest
	if err := json.Unmarshal(mustRead(t, filepath.Join(out, "invite-manifest.json")), &mf); err != nil {
		t.Fatal(err)
	}
	for label, ref := range map[string]string{
		"node": mf.NodeImage, "nebula": mf.NebulaImage, "kubo": mf.KuboImage,
	} {
		if !strings.Contains(ref, "@sha256:") {
			t.Errorf("%s image in the manifest is not digest-pinned: %q", label, ref)
		}
	}
	if mf.NodeID == "" || mf.CAFingerprint == "" || mf.SwarmKeyFP == "" {
		t.Fatalf("manifest under-populated: %+v", mf)
	}
}

// TestInvite_NebulaPublicKeyKeepsTheOverlayPrivateKeyWithTheDonor covers the
// stronger handoff: the operator signs a donor-generated public key and never
// holds the donor's overlay private key.
func TestInvite_NebulaPublicKeyKeepsTheOverlayPrivateKeyWithTheDonor(t *testing.T) {
	root, out := bootstrappedRoot(t), t.TempDir()
	pub := filepath.Join(t.TempDir(), "donor.pub")
	if err := os.WriteFile(pub, []byte("DONOR-PUBLIC-KEY\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := inviteParams(root, out)
	p.NebulaPublicKey = pub
	if _, err := bootstrap.Invite(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "nebula", "nebula.pub")); err != nil {
		t.Fatal("the donor-supplied public key should be recorded in the bundle")
	}
	if _, err := os.Stat(filepath.Join(out, "secrets", "nebula_key")); !os.IsNotExist(err) {
		t.Fatal("the operator must never ship an overlay private key in this mode")
	}
}

func TestInvite_RefusesWithoutABootstrappedFederation(t *testing.T) {
	_, err := bootstrap.Invite(inviteParams(t.TempDir(), t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "federation init") {
		t.Fatalf("should point the operator at federation init, got %v", err)
	}
}

func TestAssertBundleClean_CatchesAPlantedSecret(t *testing.T) {
	root, out := bootstrappedRoot(t), t.TempDir()
	if _, err := bootstrap.Invite(inviteParams(root, out)); err != nil {
		t.Fatal(err)
	}
	active := bootstrap.ActiveDir(root)

	if err := bootstrap.AssertBundleClean(active, out); err != nil {
		t.Fatalf("a freshly issued bundle must be clean, got %v", err)
	}

	// Plant the CA key after the fact — the E2E checks the artifact, not just
	// the generator that produced it.
	caKey := mustRead(t, filepath.Join(active, bootstrap.FileFederationCAKey))
	if err := os.WriteFile(filepath.Join(out, "oops.key"), caKey, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := bootstrap.AssertBundleClean(active, out); err == nil {
		t.Fatal("a planted CA key must be detected in an on-disk bundle")
	}
}
