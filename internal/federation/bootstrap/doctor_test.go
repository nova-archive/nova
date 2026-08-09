package bootstrap_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nova-archive/nova/internal/federation/bootstrap"
)

func findCheck(t *testing.T, checks []bootstrap.Check, id string) bootstrap.Check {
	t.Helper()
	for _, c := range checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no check with id %q in %+v", id, checks)
	return bootstrap.Check{}
}

// healthyFederation builds a bootstrapped federation plus a matching
// operator.yaml, the state doctor should call clean.
func healthyFederation(t *testing.T) (root, operatorYAML string) {
	t.Helper()
	root = t.TempDir()
	res, err := bootstrap.Init(baseParams(root))
	if err != nil {
		t.Fatal(err)
	}
	active := bootstrap.ActiveDir(root)
	operatorYAML = filepath.Join(t.TempDir(), "operator.yaml")
	body := "federation:\n" +
		"  listen_addr: \"" + res.Manifest.FederationListenAddr + "\"\n" +
		"  nebula_interface: nebula1\n" +
		"  federation_ca_path: " + filepath.Join(active, bootstrap.FileFederationCACert) + "\n" +
		"  federation_cert_path: " + filepath.Join(active, bootstrap.FileCoordinatorCert) + "\n" +
		"  federation_key_path: " + filepath.Join(active, bootstrap.FileCoordinatorKey) + "\n"
	if err := os.WriteFile(operatorYAML, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, operatorYAML
}

// --- Plane A ---------------------------------------------------------------

func TestDoctorLocal_HealthyFederationPassesEveryCheck(t *testing.T) {
	root, opYAML := healthyFederation(t)
	checks := bootstrap.DoctorLocal(root, opYAML)

	if !bootstrap.OK(checks) {
		t.Fatalf("healthy federation should pass; failures: %+v", bootstrap.Failures(checks))
	}
	for _, id := range []string{"pki.parse", "pki.sans", "pki.seed", "pki.perms", "cfg.manifest", "swarm.key"} {
		if got := findCheck(t, checks, id); got.Status != "pass" {
			t.Errorf("%s = %s (%s)", id, got.Status, got.Detail)
		}
	}
}

func TestDoctorLocal_ReportsEveryFailureNotJustTheFirst(t *testing.T) {
	root, opYAML := healthyFederation(t)
	active := bootstrap.ActiveDir(root)

	// Break two independent things at once.
	if err := os.Chmod(filepath.Join(active, bootstrap.FileFederationCAKey), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active, bootstrap.FileRepairSigningKey), []byte("not base64!!"), 0o600); err != nil {
		t.Fatal(err)
	}

	checks := bootstrap.DoctorLocal(root, opYAML)
	if got := findCheck(t, checks, "pki.perms"); got.Status != "fail" {
		t.Error("a world-readable CA key must fail pki.perms")
	}
	if got := findCheck(t, checks, "pki.seed"); got.Status != "fail" {
		t.Error("an unparseable repair seed must fail pki.seed")
	}
	// The point of reporting rather than erroring: both are visible at once.
	if n := len(bootstrap.Failures(checks)); n < 2 {
		t.Fatalf("want both failures reported, got %d", n)
	}
}

func TestDoctorLocal_DetectsMismatchedKeyPair(t *testing.T) {
	root, opYAML := healthyFederation(t)
	other := t.TempDir()
	if _, err := bootstrap.Init(baseParams(other)); err != nil {
		t.Fatal(err)
	}
	// Swap in a key from a different federation: same format, wrong key.
	foreign := mustRead(t, filepath.Join(bootstrap.ActiveDir(other), bootstrap.FileCoordinatorKey))
	if err := os.WriteFile(filepath.Join(bootstrap.ActiveDir(root), bootstrap.FileCoordinatorKey), foreign, 0o600); err != nil {
		t.Fatal(err)
	}

	checks := bootstrap.DoctorLocal(root, opYAML)
	if got := findCheck(t, checks, "pki.parse"); got.Status != "fail" {
		t.Fatal("a cert/key mismatch must fail pki.parse — the handshake would silently never succeed")
	}
}

func TestDoctorLocal_DetectsOperatorYAMLDisagreement(t *testing.T) {
	root, _ := healthyFederation(t)
	bad := filepath.Join(t.TempDir(), "operator.yaml")
	if err := os.WriteFile(bad, []byte(
		"federation:\n  listen_addr: \"10.42.0.1:8443\"\n  nebula_interface: nebula1\n"+
			"  federation_ca_path: /a\n  federation_cert_path: /b\n  federation_key_path: /c\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	checks := bootstrap.DoctorLocal(root, bad)
	got := findCheck(t, checks, "cfg.manifest")
	if got.Status != "fail" {
		t.Fatal("operator.yaml naming a different listen_addr must fail cfg.manifest")
	}
	if !strings.Contains(got.Detail, "8443") {
		t.Fatalf("the detail should show the disagreement, got %q", got.Detail)
	}
}

func TestDoctorLocal_DetectsEmptyFederationBlock(t *testing.T) {
	root, _ := healthyFederation(t)
	empty := filepath.Join(t.TempDir(), "operator.yaml")
	if err := os.WriteFile(empty, []byte("federation: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := findCheck(t, bootstrap.DoctorLocal(root, empty), "cfg.manifest"); got.Status != "fail" {
		t.Fatal("an empty federation block means federation is disabled; that must fail")
	}
}

// TestDoctorLocal_DetectsSwarmKeyDrift is the invariant that matters most: a
// swarm key differing from the manifest means donors holding the recorded key
// are already partitioned from the private swarm.
func TestDoctorLocal_DetectsSwarmKeyDrift(t *testing.T) {
	root, opYAML := healthyFederation(t)
	replacement := []byte("/key/swarm/psk/1.0.0/\n/base16/\nfeedface\n")
	if err := os.WriteFile(filepath.Join(bootstrap.ActiveDir(root), bootstrap.FileSwarmKey), replacement, 0o600); err != nil {
		t.Fatal(err)
	}

	got := findCheck(t, bootstrap.DoctorLocal(root, opYAML), "swarm.key")
	if got.Status != "fail" {
		t.Fatal("a swarm key that no longer matches the manifest must fail")
	}
	if !strings.Contains(got.Detail, "partitioned") {
		t.Fatalf("the detail should explain the consequence, got %q", got.Detail)
	}
}

// --- Plane B ---------------------------------------------------------------

func readyzServer(t *testing.T, code int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/readyz"
}

func TestDoctorLive_NotReadyIsReportedWithTheReason(t *testing.T) {
	url := readyzServer(t, http.StatusServiceUnavailable,
		`{"federation":{"ready":false,"iface":"nebula1","waiting_for":"nebula1"}}`)

	checks := bootstrap.DoctorLive(context.Background(), bootstrap.LiveOpts{ReadyzURL: url})
	got := findCheck(t, checks, "net.ready")
	if got.Status != "fail" {
		t.Fatalf("net.ready = %s, want fail", got.Status)
	}
	if !strings.Contains(got.Detail, "nebula1") {
		t.Fatalf("detail should name what it is waiting for, got %q", got.Detail)
	}
}

// TestDoctorLive_BindRejectsWildcardAddress: an overlay-only listener bound to
// 0.0.0.0 exposes the federation API off the overlay, which the trust model
// assumes cannot happen.
func TestDoctorLive_BindRejectsWildcardAddress(t *testing.T) {
	url := readyzServer(t, http.StatusOK,
		`{"federation":{"ready":true,"iface":"nebula1","bound_addr":"0.0.0.0:9443"}}`)

	got := findCheck(t, bootstrap.DoctorLive(context.Background(), bootstrap.LiveOpts{ReadyzURL: url}), "net.bind")
	if got.Status != "fail" {
		t.Fatal("0.0.0.0 must fail net.bind — the listener is overlay-only by contract")
	}
}

func TestDoctorLive_AcceptsOverlayBind(t *testing.T) {
	url := readyzServer(t, http.StatusOK,
		`{"federation":{"ready":true,"iface":"nebula1","bound_addr":"10.42.0.1:9443"}}`)

	checks := bootstrap.DoctorLive(context.Background(), bootstrap.LiveOpts{ReadyzURL: url})
	for _, id := range []string{"net.iface", "net.bind", "net.ready"} {
		if got := findCheck(t, checks, id); got.Status != "pass" {
			t.Errorf("%s = %s (%s)", id, got.Status, got.Detail)
		}
	}
}

func TestDoctorLive_UnreachableCoordinatorFailsRatherThanPanics(t *testing.T) {
	checks := bootstrap.DoctorLive(context.Background(), bootstrap.LiveOpts{
		ReadyzURL: "http://127.0.0.1:1/readyz",
	})
	if got := findCheck(t, checks, "net.iface"); got.Status != "fail" {
		t.Error("an unreachable coordinator must fail net.iface")
	}
	if got := findCheck(t, checks, "net.bind"); got.Status != "skip" {
		t.Error("net.bind cannot be judged without the coordinator; it should skip, not fail spuriously")
	}
}

func TestDoctorLive_MTLSSkipsWithoutAClientIdentity(t *testing.T) {
	url := readyzServer(t, http.StatusOK, `{"federation":{"ready":true,"iface":"nebula1","bound_addr":"10.42.0.1:9443"}}`)
	checks := bootstrap.DoctorLive(context.Background(), bootstrap.LiveOpts{
		ReadyzURL: url, FederationAddr: "10.42.0.1:9443",
	})
	if got := findCheck(t, checks, "net.mtls"); got.Status != "skip" {
		t.Fatalf("without a client identity the probe should skip, got %s", got.Status)
	}
}
