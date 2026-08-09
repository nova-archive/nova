package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/nova-archive/nova/internal/federation/bootstrap"
	"gopkg.in/yaml.v3"
)

// Plane C of `federation doctor` (P2-M7.2, D-M7.2-3): a STATIC custody check
// over the effective deployment definition.
//
// The property is that issuance authority — the Nova federation CA key and the
// Nebula CA key — never enters a long-running container. Verifying that by
// inspecting a running container would require handing the admin container
// /var/run/docker.sock, which turns a deliberately narrow administrative tool
// into a root-equivalent Docker control plane and defeats the whole point.
//
// Proving the deployment definition makes the mount IMPOSSIBLE is both cheaper
// and strictly stronger than proving one container happened not to have it at
// one moment. It reads `docker compose config` output as plain YAML: no
// privilege, no socket, no daemon.

// pkiVolume is the admin-only volume holding CA private keys.
const pkiVolume = "nova-fedpki"

// adminService is the only service permitted to mount pkiVolume.
const adminService = "nova-admin"

// importMount is the read-only adoption path (D-M7.2-2c).
const importMount = "/import"

// caKeyMarkers identify a CA private key by path. A service mounting one of
// these directly is the same violation as mounting the volume.
var caKeyMarkers = []string{
	"federation-ca.key",
	"nebula-ca.key",
	"repair-signing.key",
}

type composeDoc struct {
	Services map[string]composeService `yaml:"services"`
}

type composeService struct {
	Profiles []string    `yaml:"profiles"`
	Volumes  []yaml.Node `yaml:"volumes"`
}

// mount is the normalized form of both compose volume syntaxes.
type mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

// parseMount handles both the short string form ("src:dst:ro") and the long
// mapping form ({type, source, target, read_only}).
func parseMount(n yaml.Node) (mount, error) {
	if n.Kind == yaml.ScalarNode {
		var s string
		if err := n.Decode(&s); err != nil {
			return mount{}, err
		}
		parts := strings.Split(s, ":")
		m := mount{Source: parts[0]}
		if len(parts) > 1 {
			m.Target = parts[1]
		}
		for _, opt := range parts[2:] {
			if opt == "ro" {
				m.ReadOnly = true
			}
		}
		return m, nil
	}
	var long struct {
		Source   string `yaml:"source"`
		Target   string `yaml:"target"`
		ReadOnly bool   `yaml:"read_only"`
	}
	if err := n.Decode(&long); err != nil {
		return mount{}, err
	}
	return mount{Source: long.Source, Target: long.Target, ReadOnly: long.ReadOnly}, nil
}

// policyResult collects violations. Checks report rather than short-circuit, so
// one problem does not hide the others.
type policyResult struct {
	violations []bootstrap.Check
	checked    int
}

func (r *policyResult) add(id, detail string) {
	r.violations = append(r.violations, bootstrap.Fail("C", id, detail))
}

func (r *policyResult) OK() bool { return len(r.violations) == 0 }

func (r *policyResult) Has(id string) bool {
	for _, v := range r.violations {
		if v.ID == id {
			return true
		}
	}
	return false
}

func (r *policyResult) Violations() []bootstrap.Check { return r.violations }

// checkComposePolicy runs the three custody checks over rendered compose YAML.
func checkComposePolicy(in []byte) *policyResult {
	res := &policyResult{}

	var doc composeDoc
	if err := yaml.Unmarshal(in, &doc); err != nil {
		res.add("custody.parse", fmt.Sprintf("compose config is not valid YAML: %v", err))
		return res
	}
	if len(doc.Services) == 0 {
		res.add("custody.parse", "compose config declares no services")
		return res
	}

	names := make([]string, 0, len(doc.Services))
	for n := range doc.Services {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		svc := doc.Services[name]
		res.checked++
		for _, raw := range svc.Volumes {
			m, err := parseMount(raw)
			if err != nil {
				res.add("custody.parse", fmt.Sprintf("service %s: unreadable volume entry: %v", name, err))
				continue
			}

			// custody.fedpki — only nova-admin may mount the PKI volume.
			if m.Source == pkiVolume && name != adminService {
				res.add("custody.fedpki", fmt.Sprintf(
					"service %q mounts %s at %s; issuance authority must stay in %s only",
					name, pkiVolume, m.Target, adminService))
			}

			// custody.import — the adoption path is read-only and admin-only.
			if m.Target == importMount {
				if name != adminService {
					res.add("custody.import", fmt.Sprintf(
						"service %q mounts %s; only %s may read the adoption import",
						name, importMount, adminService))
				}
				if !m.ReadOnly {
					res.add("custody.import", fmt.Sprintf(
						"service %q mounts %s writable; adoption must never write to the operator's existing PKI",
						name, importMount))
				}
			}

			// custody.secrets — no service mounts a CA private key by path.
			for _, marker := range caKeyMarkers {
				if strings.Contains(m.Source, marker) || strings.Contains(m.Target, marker) {
					res.add("custody.secrets", fmt.Sprintf(
						"service %q mounts %q, which is CA/authority key material", name, m.Source))
				}
			}
		}
	}
	return res
}

func cmdFederationComposePolicy(args []string) error {
	fs := flag.NewFlagSet("federation compose-policy", flag.ContinueOnError)
	file := fs.String("file", "-", `rendered "docker compose config" output ("-" for stdin)`)
	asJSON := fs.Bool("json", false, "emit machine-readable results")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	var (
		in  []byte
		err error
	)
	if *file == "-" {
		in, err = io.ReadAll(os.Stdin)
	} else {
		in, err = os.ReadFile(*file)
	}
	if err != nil {
		return fmt.Errorf("compose-policy: read input: %w", err)
	}

	res := checkComposePolicy(in)

	if *asJSON {
		return bootstrap.EmitJSON(os.Stdout, res.Violations())
	}
	if res.OK() {
		fmt.Printf("OK: custody policy satisfied across %d service(s)\n", res.checked)
		return nil
	}
	for _, v := range res.Violations() {
		fmt.Fprintf(os.Stderr, "FAIL [%s] %s\n", v.ID, v.Detail)
	}
	return fmt.Errorf("compose-policy: %d custody violation(s)", len(res.Violations()))
}
