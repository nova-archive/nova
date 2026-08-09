package deploy_test

import (
	"strings"
	"testing"

	"github.com/nova-archive/nova/internal/deploy"
	"gopkg.in/yaml.v3"
)

// composeFile is the minimal shape bundle-topology needs. Compose has many more
// fields; parsing only these keeps the gate robust against unrelated additions.
type composeFile struct {
	Services map[string]struct {
		Image       string   `yaml:"image"`
		NetworkMode string   `yaml:"network_mode"`
		Volumes     []string `yaml:"volumes"`
		DependsOn   []string `yaml:"depends_on"`
	} `yaml:"services"`
	Volumes map[string]any `yaml:"volumes"`
}

func parseCompose(t *testing.T, b []byte) composeFile {
	t.Helper()
	var c composeFile
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatalf("generated compose.yaml is not valid YAML: %v", err)
	}
	return c
}

// mountTargets returns the in-container paths a service mounts.
func mountTargets(vols []string) []string {
	out := make([]string, 0, len(vols))
	for _, v := range vols {
		parts := strings.Split(v, ":")
		if len(parts) >= 2 {
			out = append(out, parts[1])
		}
	}
	return out
}

func TestBundleTopology_GeneratedComposeIsValidYAMLWithAllThreeServices(t *testing.T) {
	c := parseCompose(t, render(t, testParams())["compose.yaml"])
	for _, svc := range []string{"nebula", "kubo", "nova-node"} {
		if _, ok := c.Services[svc]; !ok {
			t.Errorf("canonical topology is missing service %q", svc)
		}
	}
}

// TestBundleTopology_EveryNodeYAMLPathHasABackingMount is the gate that would
// have caught the generated bundle referencing files nothing mounted.
func TestBundleTopology_EveryNodeYAMLPathHasABackingMount(t *testing.T) {
	out := render(t, testParams())
	c := parseCompose(t, out["compose.yaml"])

	node, ok := c.Services["nova-node"]
	if !ok {
		t.Fatal("no nova-node service")
	}
	targets := mountTargets(node.Volumes)

	for _, line := range strings.Split(string(out["node.yaml"]), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, found := strings.Cut(line, ":")
		if !found || !strings.HasSuffix(strings.TrimSpace(key), "_path") {
			continue
		}
		p := strings.Trim(strings.TrimSpace(val), `"`)
		if p == "" || !strings.HasPrefix(p, "/") {
			continue
		}
		covered := false
		for _, tgt := range targets {
			if p == tgt || strings.HasPrefix(p, strings.TrimSuffix(tgt, "/")+"/") {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("node.yaml %s = %s has no backing mount in nova-node (mounts: %v)",
				strings.TrimSpace(key), p, targets)
		}
	}
}

// TestBundleTopology_StorageDirHasAMount catches the /var/lib/nova-node vs
// /var/lib/nova-node/data divergence between the two old definitions.
func TestBundleTopology_StorageDirHasAMount(t *testing.T) {
	out := render(t, testParams())
	c := parseCompose(t, out["compose.yaml"])
	targets := mountTargets(c.Services["nova-node"].Volumes)

	found := false
	for _, tgt := range targets {
		if tgt == deploy.DirStorage {
			found = true
		}
	}
	if !found {
		t.Fatalf("storage_dir %s has no matching mount (mounts: %v)", deploy.DirStorage, targets)
	}
}

// TestBundleTopology_LoopbackAddressesStayInsideOneNamespace is the D-M7.2-6
// guarantee: node.yaml's kubo_api_addr is a loopback address, which only
// resolves across containers when both share a network namespace. In separate
// namespaces it is silently unreachable and nothing can be pinned.
func TestBundleTopology_LoopbackAddressesStayInsideOneNamespace(t *testing.T) {
	out := render(t, testParams())
	c := parseCompose(t, out["compose.yaml"])

	if !strings.Contains(string(out["node.yaml"]), "127.0.0.1:5001") {
		t.Skip("kubo_api_addr is not loopback; namespace sharing is not required")
	}
	nodeNS := c.Services["nova-node"].NetworkMode
	kuboNS := c.Services["kubo"].NetworkMode

	if nodeNS == "" || kuboNS == "" {
		t.Fatalf("both services need an explicit network_mode; got node=%q kubo=%q", nodeNS, kuboNS)
	}
	if nodeNS != kuboNS {
		t.Fatalf("kubo_api_addr is loopback but namespaces differ: node=%q kubo=%q", nodeNS, kuboNS)
	}
}

func TestBundleTopology_NamedVolumesAreDeclared(t *testing.T) {
	c := parseCompose(t, render(t, testParams())["compose.yaml"])
	for svcName, svc := range c.Services {
		for _, v := range svc.Volumes {
			src, _, found := strings.Cut(v, ":")
			if !found || strings.HasPrefix(src, ".") || strings.HasPrefix(src, "/") {
				continue // bind mount
			}
			if _, ok := c.Volumes[src]; !ok {
				t.Errorf("service %s mounts named volume %q which is not declared", svcName, src)
			}
		}
	}
}

// TestBundleTopology_NoPublishedPorts: all federation traffic is mTLS over the
// overlay, so a donor publishes nothing to its host.
func TestBundleTopology_NoPublishedPorts(t *testing.T) {
	if strings.Contains(string(render(t, testParams())["compose.yaml"]), "\n    ports:") {
		t.Fatal("a donor must publish no ports; all traffic is mTLS over the overlay")
	}
}
