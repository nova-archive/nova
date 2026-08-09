package deploy_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nova-archive/nova/internal/deploy"
	nodeconfig "github.com/nova-archive/nova/internal/node/config"
)

func testParams() deploy.DonorParams {
	return deploy.DonorParams{
		Name:                 "alice",
		NebulaIP:             "10.42.0.10/24",
		LighthouseOverlayIP:  "10.42.0.1",
		LighthousePublicIP:   "203.0.113.7",
		CoordinatorOverlayIP: "10.42.0.1",
		NodeImage:            "ghcr.io/nova-archive/nova-node@sha256:" + strings.Repeat("a", 64),
		NebulaImage:          "nebulaoss/nebula@sha256:" + strings.Repeat("b", 64),
		KuboImage:            "ipfs/kubo@sha256:" + strings.Repeat("c", 64),
		StorageMaxBytes:      536870912000,

		BandwidthBudgetBytesPerDay: 53687091200,
	}
}

func render(t *testing.T, p deploy.DonorParams) map[string][]byte {
	t.Helper()
	out, err := deploy.RenderDonorBundle(p)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return out
}

func TestRenderDonorBundle_ProducesEveryCanonicalFile(t *testing.T) {
	out := render(t, testParams())
	for _, want := range []string{"compose.yaml", "node.yaml", "nebula-config.yml", "kubo-init.sh", "README.md"} {
		if _, ok := out[want]; !ok {
			t.Errorf("bundle is missing %s", want)
		}
	}
}

func TestValidate_RefusesMutableTags(t *testing.T) {
	p := testParams()
	p.NodeImage = "ghcr.io/nova-archive/nova-node:latest"
	if _, err := deploy.RenderDonorBundle(p); err == nil {
		t.Fatal("a mutable tag must be refused in a generated artifact")
	} else if !strings.Contains(err.Error(), "digest") {
		t.Fatalf("error should name the digest requirement, got %v", err)
	}
}

func TestValidate_RefusesNonPositiveBandwidthBudget(t *testing.T) {
	p := testParams()
	p.BandwidthBudgetBytesPerDay = 0
	if _, err := deploy.RenderDonorBundle(p); err == nil {
		t.Fatal("nodeconfig refuses <= 0, so rendering it would produce an unloadable artifact")
	}
}

// --- D-M7.2-5: canonical topology -----------------------------------------

func TestCompose_HasKuboSharingTheNebulaNamespace(t *testing.T) {
	compose := string(render(t, testParams())["compose.yaml"])
	if !strings.Contains(compose, "kubo:") {
		t.Fatal("canonical topology is nebula + hardened kubo + nova-node; kubo is missing")
	}
	if n := strings.Count(compose, `network_mode: "service:nebula"`); n < 2 {
		t.Fatalf("kubo and nova-node must both share the nebula namespace, found %d", n)
	}
}

func TestNodeYAML_CarriesEveryRuntimeRequiredField(t *testing.T) {
	node := string(render(t, testParams())["node.yaml"])
	for _, f := range []string{
		"coordinator_url:", "federation_ca_path:", "federation_cert_path:", "federation_key_path:",
		"nebula_cert_path:", "nebula_key_path:", "swarm_key_path:", "storage_dir:",
		"bandwidth_budget_bytes_per_day:", "storage_max_bytes:", "kubo_api_addr:",
		"source_nebula_addr:", "source_read_listen_addr:",
	} {
		if !strings.Contains(node, "\n"+f) {
			t.Errorf("node.yaml missing required field %s", f)
		}
	}
}

func TestNodeYAML_EgressBudgetStaysCommentedSoInheritanceSurvives(t *testing.T) {
	node := string(render(t, testParams())["node.yaml"])
	if strings.Contains(node, "\negress_budget_bytes_per_day:") {
		t.Fatal("pinning a literal breaks the loader's inherit-from-bandwidth behaviour")
	}
	if !strings.Contains(node, "# egress_budget_bytes_per_day:") {
		t.Fatal("it should still appear as documented, commented guidance")
	}
}

func TestPortVocabularyIsFixed(t *testing.T) {
	out := render(t, testParams())
	node, neb := string(out["node.yaml"]), string(out["nebula-config.yml"])

	if !strings.Contains(node, ":9443") {
		t.Error("coordinator_url must use 9443")
	}
	if strings.Contains(node, ":8443") || strings.Contains(neb, ":8443") {
		t.Error("8443 is the public nginx port and must never appear in donor artifacts")
	}
	if !strings.Contains(node, ":9555") {
		t.Error("read-source must use 9555")
	}
	if !strings.Contains(neb, "4242") {
		t.Error("nebula lighthouse must use 4242")
	}
}

func TestNodeYAML_AdvertisedAndBindAddressesAreDistinct(t *testing.T) {
	node := string(render(t, testParams())["node.yaml"])
	if !strings.Contains(node, `source_nebula_addr:     "10.42.0.10:9555"`) {
		t.Error("advertised source address should be the overlay address")
	}
	if !strings.Contains(node, `source_read_listen_addr: "0.0.0.0:9555"`) {
		t.Error("bind address is distinct from the advertised address by design")
	}
}

func TestNebulaFirewallIsNotWideOpenInbound(t *testing.T) {
	neb := string(render(t, testParams())["nebula-config.yml"])
	i := strings.Index(neb, "inbound:")
	if i < 0 {
		t.Fatal("no inbound section")
	}
	if strings.Contains(neb[i:], "port: any") {
		t.Fatal("inbound must be restricted to the read-source port, not any/any/any")
	}
	if !strings.Contains(neb[i:], "port: 9555") {
		t.Fatal("inbound should permit the read-source port")
	}
}

func TestKuboInit_DisablesPublicRouting(t *testing.T) {
	sh := string(render(t, testParams())["kubo-init.sh"])
	for _, want := range []string{"bootstrap rm --all", "Routing.Type none", "Discovery.MDNS.Enabled false", "Addresses.API"} {
		if !strings.Contains(sh, want) {
			t.Errorf("kubo hardening missing %q", want)
		}
	}
	if !strings.Contains(sh, "refusing to start") {
		t.Error("a missing swarm key must abort rather than join the public network")
	}
}

// --- D-M7.2-10 / 10a: artifact gates --------------------------------------

func TestPathPrefixRewritesEveryAbsolutePath(t *testing.T) {
	p := testParams()
	p.PathPrefix = "/tmp/fixture"
	node := string(render(t, p)["node.yaml"])

	for _, abs := range []string{" /etc/nova/", " /run/secrets/", " /var/lib/nova-node/"} {
		if strings.Contains(node, abs) {
			t.Errorf("node.yaml still contains an unprefixed path %q", strings.TrimSpace(abs))
		}
	}
	if !strings.Contains(node, "/tmp/fixture/etc/nova/") {
		t.Error("node.yaml should carry prefixed paths")
	}
}

// TestNodeYAMLCompleteness reflects over nodeconfig.Config so a field added in
// a later milestone cannot silently reopen the six-missing-fields defect.
func TestNodeYAMLCompleteness(t *testing.T) {
	optOut := map[string]string{
		"egress_budget_bytes_per_day": "D-M7.2-9a: commented so loader inheritance survives",
		"audit_budget_fraction":       "D-M7.2-9a: commented; defaults to 0.01",
	}
	node := string(render(t, testParams())["node.yaml"])

	rt := reflect.TypeOf(nodeconfig.Config{})
	for i := 0; i < rt.NumField(); i++ {
		tag := strings.Split(rt.Field(i).Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		if _, ok := optOut[tag]; ok {
			if !strings.Contains(node, "# "+tag+":") {
				t.Errorf("opt-out field %s must still appear as commented guidance", tag)
			}
			continue
		}
		if !strings.Contains(node, "\n"+tag+":") {
			t.Errorf("node.yaml is missing %s — add it to the template or justify it in optOut", tag)
		}
	}
}

// TestRenderedNodeConfigLoadsThroughProductionLoader is the D-M7.2-10a gate.
// Reflection proves presence; only the real loader proves the values parse,
// meet the validation floors, and agree with one another.
//
// The loader is NOT pure: checkReadableFile stats and opens all six *_path
// fields, and validate() MkdirAll's storage_dir then probe-writes it. So this
// materializes a fixture tree rather than pointing at the raw template.
func TestRenderedNodeConfigLoadsThroughProductionLoader(t *testing.T) {
	root := t.TempDir()
	p := testParams()
	p.PathPrefix = root

	node := render(t, p)["node.yaml"]

	for _, rel := range []string{
		"etc/nova/federation/federation-ca.crt",
		"etc/nova/federation/federation.crt",
		"run/secrets/nova_node_federation_key",
		"etc/nebula/nebula.crt",
		"run/secrets/nebula_key",
		"run/secrets/ipfs_swarm_key",
	} {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cfg, err := nodeconfig.LoadFromBytes(node)
	if err != nil {
		t.Fatalf("rendered node.yaml does not load through the production loader: %v", err)
	}

	if cfg.SourceNebulaAddr == "" {
		t.Fatal("read-source should be configured in the canonical artifact")
	}
	if cfg.EgressBudgetBytesPerDay != cfg.BandwidthBudgetBytesPerDay {
		t.Fatalf("egress budget = %d, want it inherited from bandwidth %d",
			cfg.EgressBudgetBytesPerDay, cfg.BandwidthBudgetBytesPerDay)
	}
	if cfg.AuditBudgetFraction != 0.01 {
		t.Fatalf("audit_budget_fraction = %v, want the 0.01 default", cfg.AuditBudgetFraction)
	}
	if cfg.KuboAPIAddr != deploy.KuboAPIListen {
		t.Fatalf("kubo_api_addr = %q", cfg.KuboAPIAddr)
	}
}
