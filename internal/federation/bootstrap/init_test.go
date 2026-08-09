package bootstrap_test

import (
	"bytes"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nova-archive/nova/internal/federation/bootstrap"
	"github.com/nova-archive/nova/internal/federation/ca"
)

func baseParams(root string) bootstrap.Params {
	return bootstrap.Params{
		Root:              root,
		OverlayCIDR:       "10.42.0.0/24",
		OperatorOverlayIP: "10.42.0.1",
		LighthousePublic:  "203.0.113.7:4242",
		Hostname:          "nova.example.org",
		SkipPreflight:     true,
	}
}

func activePath(root, name string) string {
	return filepath.Join(bootstrap.ActiveDir(root), name)
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// --- Task 10: greenfield ---------------------------------------------------

func TestInit_GreenfieldProducesTheFullBootstrap(t *testing.T) {
	root := t.TempDir()
	res, err := bootstrap.Init(baseParams(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		bootstrap.FileFederationCACert, bootstrap.FileFederationCAKey,
		bootstrap.FileCoordinatorCert, bootstrap.FileCoordinatorKey,
		bootstrap.FileClientCert, bootstrap.FileClientKey,
		bootstrap.FileRepairSigningKey,
		bootstrap.FileNebulaCACert, bootstrap.FileNebulaCAKey,
		bootstrap.FileLighthouseCert, bootstrap.FileLighthouseKey,
		bootstrap.FileSwarmKey, bootstrap.FileManifest,
	} {
		if _, err := os.Stat(activePath(root, name)); err != nil {
			t.Errorf("greenfield init did not produce %s", name)
		}
	}
	if res.Created == 0 || res.Adopted != 0 {
		t.Fatalf("want all-created on greenfield, got %+v", res)
	}
	if res.Manifest.CAFingerprint == "" || res.Manifest.FederationListenAddr != "10.42.0.1:9443" {
		t.Fatalf("manifest not populated: %+v", res.Manifest)
	}
}

func TestInit_KeysAreNotWorldReadable(t *testing.T) {
	root := t.TempDir()
	if _, err := bootstrap.Init(baseParams(root)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		bootstrap.FileFederationCAKey, bootstrap.FileCoordinatorKey,
		bootstrap.FileClientKey, bootstrap.FileRepairSigningKey,
		bootstrap.FileNebulaCAKey, bootstrap.FileSwarmKey,
	} {
		info, err := os.Stat(activePath(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("%s mode = %o, want no group/other access", name, mode)
		}
	}
}

func TestInit_IsIdempotentOnASecondRun(t *testing.T) {
	root := t.TempDir()
	p := baseParams(root)
	if _, err := bootstrap.Init(p); err != nil {
		t.Fatal(err)
	}
	before := mustRead(t, activePath(root, bootstrap.FileFederationCACert))

	res, err := bootstrap.Init(p)
	if err != nil {
		t.Fatalf("second run must be a no-op, got %v", err)
	}
	after := mustRead(t, activePath(root, bootstrap.FileFederationCACert))

	if !bytes.Equal(before, after) {
		t.Fatal("the CA must not be regenerated on a repeat run")
	}
	if res.Created != 0 {
		t.Fatalf("want zero created on a repeat run, got %+v", res)
	}
}

func TestInit_RejectsOverlayIPOutsideCIDR(t *testing.T) {
	p := baseParams(t.TempDir())
	p.OperatorOverlayIP = "10.99.0.1"
	if _, err := bootstrap.Init(p); err == nil {
		t.Fatal("an overlay IP outside the CIDR must be refused")
	}
}

func TestInit_LeavesNoStagingBehindOnSuccess(t *testing.T) {
	root := t.TempDir()
	if _, err := bootstrap.Init(baseParams(root)); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Join(root, ".staging")); err == nil && len(entries) > 0 {
		t.Fatalf("staging should be empty after success, found %d generation(s)", len(entries))
	}
}

// TestInit_ResumesAfterInterruptionAtEveryStage is the crash-injection proof
// that idempotence holds under power loss, not merely after a clean success.
func TestInit_ResumesAfterInterruptionAtEveryStage(t *testing.T) {
	for _, failAfter := range []string{"stage", "fsync", "validate", "activate"} {
		t.Run(failAfter, func(t *testing.T) {
			root := t.TempDir()
			p := baseParams(root)
			p.FailAfter = failAfter

			if _, err := bootstrap.Init(p); err == nil {
				t.Fatal("injected failure should have errored")
			}

			p.FailAfter = ""
			if _, err := bootstrap.Init(p); err != nil {
				t.Fatalf("a re-run after interruption must converge, got %v", err)
			}
			for _, name := range []string{
				bootstrap.FileFederationCACert, bootstrap.FileFederationCAKey,
				bootstrap.FileSwarmKey, bootstrap.FileManifest,
			} {
				if _, err := os.Stat(activePath(root, name)); err != nil {
					t.Errorf("converged run is missing %s", name)
				}
			}
			if entries, err := os.ReadDir(filepath.Join(root, ".staging")); err == nil && len(entries) > 0 {
				t.Errorf("orphan staging generations survived: %d", len(entries))
			}
		})
	}
}

// --- Task 11: adoption -----------------------------------------------------

// seedImport writes a hand-built PKI into an import directory, the shape an
// existing operator actually has.
func seedImport(t *testing.T, dir string) (caCert []byte) {
	t.Helper()
	caCert, caKey, err := ca.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	srv, srvKey, err := ca.IssueServerCert(caCert, caKey, ca.ServerCertOptions{
		DNSNames:    []string{"nova.example.org"},
		IPAddresses: []string{"10.42.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{
		bootstrap.FileFederationCACert: caCert,
		bootstrap.FileFederationCAKey:  caKey,
		bootstrap.FileCoordinatorCert:  srv,
		bootstrap.FileCoordinatorKey:   srvKey,
		bootstrap.FileSwarmKey:         []byte("/key/swarm/psk/1.0.0/\n/base16/\ndeadbeef\n"),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return caCert
}

func TestAdopt_PreservesCAAndSwarmKeyFromImport(t *testing.T) {
	root, imp := t.TempDir(), t.TempDir()
	caCert := seedImport(t, imp)

	p := baseParams(root)
	p.AdoptFrom = imp
	res, err := bootstrap.Init(p)
	if err != nil {
		t.Fatal(err)
	}

	got := mustRead(t, activePath(root, bootstrap.FileFederationCACert))
	if !bytes.Equal(got, caCert) {
		t.Fatal("adoption must preserve the existing CA byte-for-byte")
	}
	swarm := mustRead(t, activePath(root, bootstrap.FileSwarmKey))
	if !strings.Contains(string(swarm), "deadbeef") {
		t.Fatal("adoption must preserve the existing swarm key")
	}
	if res.Adopted < 5 {
		t.Fatalf("want the imported material adopted, got %+v", res)
	}
	// The missing pieces (client identity, repair seed, Nebula CA) are created.
	if res.Created == 0 {
		t.Fatal("adoption should still create what the hand-built PKI lacked")
	}
	for _, name := range []string{bootstrap.FileClientCert, bootstrap.FileRepairSigningKey, bootstrap.FileNebulaCACert} {
		if _, err := os.Stat(activePath(root, name)); err != nil {
			t.Errorf("adoption did not fill in %s", name)
		}
	}
}

// TestAdopt_NeverWritesToTheImportDirectory: /import is the operator's real
// PKI, mounted read-only. Adoption copies out of it and never into it.
func TestAdopt_NeverWritesToTheImportDirectory(t *testing.T) {
	root, imp := t.TempDir(), t.TempDir()
	seedImport(t, imp)

	before, err := os.ReadDir(imp)
	if err != nil {
		t.Fatal(err)
	}
	p := baseParams(root)
	p.AdoptFrom = imp
	if _, err := bootstrap.Init(p); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadDir(imp)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("import dir changed: %d entries before, %d after", len(before), len(after))
	}
}

func TestAdopt_NeverRotatesAnExistingSwarmKey(t *testing.T) {
	root := t.TempDir()
	active := bootstrap.ActiveDir(root)
	if err := os.MkdirAll(active, 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("/key/swarm/psk/1.0.0/\n/base16/\ncafebabe\n")
	if err := os.WriteFile(filepath.Join(active, bootstrap.FileSwarmKey), original, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := bootstrap.Init(baseParams(root)); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, activePath(root, bootstrap.FileSwarmKey)); !bytes.Equal(got, original) {
		t.Fatal("rotating the swarm key silently partitions every existing donor from the swarm")
	}
}

func TestAdopt_RefusesOnHostnameMismatch(t *testing.T) {
	root, imp := t.TempDir(), t.TempDir()
	caCert, caKey, _ := ca.GenerateCA()
	srv, srvKey, _ := ca.IssueServerCert(caCert, caKey, ca.ServerCertOptions{
		DNSNames:    []string{"old.example.org"},
		IPAddresses: []string{"10.42.0.1"},
	})
	for name, b := range map[string][]byte{
		bootstrap.FileFederationCACert: caCert,
		bootstrap.FileFederationCAKey:  caKey,
		bootstrap.FileCoordinatorCert:  srv,
		bootstrap.FileCoordinatorKey:   srvKey,
	} {
		if err := os.WriteFile(filepath.Join(imp, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	p := baseParams(root)
	p.AdoptFrom = imp
	_, err := bootstrap.Init(p)
	if err == nil {
		t.Fatal("a certificate naming a different host must refuse, not silently replace")
	}
	if !strings.Contains(err.Error(), "old.example.org") {
		t.Fatalf("the refusal must show the diff, got %v", err)
	}
}

func TestAdopt_RefusesCACertWithoutItsKey(t *testing.T) {
	root := t.TempDir()
	active := bootstrap.ActiveDir(root)
	_ = os.MkdirAll(active, 0o700)
	caCert, _, _ := ca.GenerateCA()
	if err := os.WriteFile(filepath.Join(active, bootstrap.FileFederationCACert), caCert, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := bootstrap.Init(baseParams(root))
	if err == nil {
		t.Fatal("a CA with no private key cannot issue; that must refuse")
	}
	if !strings.Contains(err.Error(), "private key") {
		t.Fatalf("refusal should name the missing key, got %v", err)
	}
}

// --- Task 12: destructive replacement --------------------------------------

func TestReplaceAuthority_RefusesWhileDonorsExist(t *testing.T) {
	root := t.TempDir()
	if _, err := bootstrap.Init(baseParams(root)); err != nil {
		t.Fatal(err)
	}

	p := baseParams(root)
	p.ReplaceAuthority = true
	p.RegisteredDonorCount = func() (int, error) { return 3, nil }

	_, err := bootstrap.Init(p)
	if err == nil {
		t.Fatal("replacing authority with 3 registered donors must refuse")
	}
	if !strings.Contains(err.Error(), "3") {
		t.Fatalf("the refusal must name the donor count, got %v", err)
	}
	if !strings.Contains(err.Error(), "destroy-existing-federation") {
		t.Fatalf("the refusal must name the second acknowledgement flag, got %v", err)
	}
}

func TestReplaceAuthority_ProceedsWithBothAcknowledgements(t *testing.T) {
	root := t.TempDir()
	if _, err := bootstrap.Init(baseParams(root)); err != nil {
		t.Fatal(err)
	}
	before := mustRead(t, activePath(root, bootstrap.FileFederationCACert))

	p := baseParams(root)
	p.ReplaceAuthority = true
	p.DestroyExistingFederation = true
	p.RegisteredDonorCount = func() (int, error) { return 3, nil }

	// The swarm key is the one thing replacement still refuses to rotate.
	_ = os.Remove(activePath(root, bootstrap.FileSwarmKey))

	if _, err := bootstrap.Init(p); err != nil {
		t.Fatal(err)
	}
	after := mustRead(t, activePath(root, bootstrap.FileFederationCACert))
	if bytes.Equal(before, after) {
		t.Fatal("both acknowledgements should have replaced the CA")
	}
}

// TestReplaceAuthority_StillRefusesToRotateTheSwarmKey: even the most
// destructive flag combination must not silently partition existing donors.
func TestReplaceAuthority_StillRefusesToRotateTheSwarmKey(t *testing.T) {
	root := t.TempDir()
	if _, err := bootstrap.Init(baseParams(root)); err != nil {
		t.Fatal(err)
	}

	p := baseParams(root)
	p.ReplaceAuthority = true
	p.DestroyExistingFederation = true
	p.RegisteredDonorCount = func() (int, error) { return 0, nil }

	_, err := bootstrap.Init(p)
	if err == nil {
		t.Fatal("rotating an existing swarm key must be refused even here")
	}
	if !strings.Contains(err.Error(), "partitioned") {
		t.Fatalf("the refusal should explain the consequence, got %v", err)
	}
}

func TestDestroyFlagRequiresReplaceAuthority(t *testing.T) {
	p := baseParams(t.TempDir())
	p.DestroyExistingFederation = true
	if _, err := bootstrap.Init(p); err == nil {
		t.Fatal("--destroy-existing-federation alone is meaningless and must be refused")
	}
}

// TestPackageNeverTouchesTheDatabase pins preservation-inventory row 9: donor
// registration lives in Postgres and must survive adoption untouched. The
// structural guarantee is that this package cannot reach internal/db at all.
func TestPackageNeverTouchesTheDatabase(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		// Parse imports rather than grepping: the source discusses internal/db
		// in prose precisely because it must not import it.
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, imp := range af.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(path, "internal/db") {
				t.Errorf("%s imports %s; federation init must never touch the nodes table", f, path)
			}
		}
	}
}

// --- runtime custody split -------------------------------------------------

// TestInstallRuntime_CopiesIdentitiesButNeverCAKeys is the filesystem half of
// the custody model. No long-running service mounts the PKI volume, so runtime
// identities must be copied out — but the two CA private keys must not be.
func TestInstallRuntime_CopiesIdentitiesButNeverCAKeys(t *testing.T) {
	root := t.TempDir()
	cfg, secrets := t.TempDir(), t.TempDir()

	p := baseParams(root)
	p.RuntimeConfigDir = cfg
	p.RuntimeSecretsDir = secrets
	if _, err := bootstrap.Init(p); err != nil {
		t.Fatal(err)
	}

	// The coordinator must be able to read these.
	for _, rel := range []string{
		filepath.Join("federation", bootstrap.FileFederationCACert),
		filepath.Join("federation", bootstrap.FileCoordinatorCert),
		filepath.Join("federation", bootstrap.FileClientCert),
		filepath.Join("nebula", bootstrap.FileNebulaCACert),
	} {
		if _, err := os.Stat(filepath.Join(cfg, rel)); err != nil {
			t.Errorf("runtime config is missing %s", rel)
		}
	}
	for _, name := range []string{
		bootstrap.RuntimeCoordinatorKey, bootstrap.RuntimeClientKey,
		bootstrap.RuntimeRepairKey, bootstrap.RuntimeSwarmKey,
	} {
		info, err := os.Stat(filepath.Join(secrets, name))
		if err != nil {
			t.Errorf("runtime secrets is missing %s", name)
			continue
		}
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("%s mode = %o, want no group/other access", name, mode)
		}
	}

	// And the CA keys must NOT have travelled.
	for _, dir := range []string{cfg, secrets} {
		for _, name := range []string{bootstrap.FileFederationCAKey, bootstrap.FileNebulaCAKey} {
			if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
				t.Errorf("%s escaped into the runtime volume %s", name, dir)
			}
		}
	}
	if err := bootstrap.AssertNoCAKeysInRuntime(cfg, secrets); err != nil {
		t.Fatalf("custody assertion failed: %v", err)
	}
}

func TestAssertNoCAKeysInRuntime_CatchesALeakedCAKey(t *testing.T) {
	secrets := t.TempDir()
	if err := os.WriteFile(filepath.Join(secrets, bootstrap.FileFederationCAKey), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := bootstrap.AssertNoCAKeysInRuntime("", secrets); err == nil {
		t.Fatal("a CA key under a runtime volume must be a custody violation")
	}
}
