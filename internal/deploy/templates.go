// Package deploy owns the CANONICAL Nova donor deployment definition.
//
// It is the single source for both the invite bundle `novactl node invite`
// hands a volunteer and the checked-in artifacts under deploy/donor/ (generated
// by `make gen-deploy`). Before P2-M7.2 these were parallel implementations
// that had already diverged on ports, topology, paths, secrets and
// milestone-era comments. See the P2-M7.2 design spec under
// docs/superpowers/specs/phase2/ for the full evidence table.
//
// novactl is a CONSUMER of this package, not its owner.
package deploy

import (
	"bytes"
	"embed"
	"fmt"
	"path"
	"strings"
	"text/template"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// Canonical port vocabulary. 8443 deliberately does not appear here: it is the
// operator's public nginx HTTPS port and nothing else (§22.3).
const (
	PortNebulaLighthouse = 4242 // udp, operator publishes, donors dial out
	PortFederationMTLS   = 9443 // tcp, coordinator federation, overlay only
	PortReadSourceMTLS   = 9555 // tcp, donor read-source, overlay only
)

// Reviewed, digest-pinned sidecar images for the canonical donor topology.
//
// Pinned as tag@digest to match docker/docker-compose.yml, so
// scripts/refresh-docker-digests.sh and the Dependabot docker ecosystem both
// track them. A donor must never be handed a mutable tag: the whole point of
// digest pinning is that the operator and the volunteer run the same bytes.
//
// resolved 2026-08-08
const (
	DefaultNebulaImage = "nebulaoss/nebula:1.11.0@sha256:1bee6515faf687e590ab42e14a769d406bf1dc59cb03e21520ae50669adc0581"
	DefaultKuboImage   = "ipfs/kubo:v0.38.1@sha256:ab04133d2851eb9e761cfee77348195c0b88d58f8a759ee4d8aaf916935fe356"
)

// Canonical filesystem layout. Public/configuration material lives under
// /etc/nova; private keys under /run/secrets. Chosen over the competing
// convention because it survives a read_only rootfs cleanly.
const (
	DirNovaEtc    = "/etc/nova"
	DirNebulaEtc  = "/etc/nebula"
	DirSecrets    = "/run/secrets"
	DirStorage    = "/var/lib/nova-node/data"
	KuboAPIListen = "http://127.0.0.1:5001"
)

// DonorParams drives the canonical donor deployment definition.
type DonorParams struct {
	Name                 string
	NebulaIP             string // CIDR form, e.g. 10.42.0.10/24
	LighthouseOverlayIP  string
	LighthousePublicIP   string
	CoordinatorOverlayIP string

	// Image references. All three MUST be digest-pinned in production; Validate
	// enforces it so a generated artifact can never carry a mutable tag (§22).
	NodeImage   string
	NebulaImage string
	KuboImage   string

	// PathPrefix is empty in production. Tests set it to a temp root so the
	// rendered node.yaml can be loaded through the production nodeconfig loader,
	// which stats every *_path and probe-writes storage_dir (D-M7.2-10a).
	PathPrefix string

	StorageMaxBytes            int64
	BandwidthBudgetBytesPerDay int64

	// AllowMutableTags is a generator-only escape hatch for deploy/donor
	// examples. `node invite` never sets it.
	AllowMutableTags bool
}

// Validate rejects params that would render an unusable or unsafe artifact.
func (p DonorParams) Validate() error {
	if p.Name == "" {
		return fmt.Errorf("deploy: Name is required")
	}
	if p.NebulaIP == "" || !strings.Contains(p.NebulaIP, "/") {
		return fmt.Errorf("deploy: NebulaIP must be in CIDR form, got %q", p.NebulaIP)
	}
	if p.CoordinatorOverlayIP == "" {
		return fmt.Errorf("deploy: CoordinatorOverlayIP is required")
	}
	if p.BandwidthBudgetBytesPerDay <= 0 {
		return fmt.Errorf("deploy: BandwidthBudgetBytesPerDay must be positive (nodeconfig refuses <= 0)")
	}
	if !p.AllowMutableTags {
		for label, ref := range map[string]string{
			"NodeImage": p.NodeImage, "NebulaImage": p.NebulaImage, "KuboImage": p.KuboImage,
		} {
			if !strings.Contains(ref, "@sha256:") {
				return fmt.Errorf("deploy: %s %q is not digest-pinned; generated artifacts must not carry mutable tags", label, ref)
			}
		}
	}
	return nil
}

// NebulaAddr returns the bare overlay address (CIDR mask stripped).
func (p DonorParams) NebulaAddr() string {
	if i := strings.Index(p.NebulaIP, "/"); i >= 0 {
		return p.NebulaIP[:i]
	}
	return p.NebulaIP
}

// tmplData is the view handed to templates.
type tmplData struct {
	DonorParams
	FederationPort int
	ReadSourcePort int
	LighthousePort int
	KuboAPIAddr    string
	SourceAddr     string // overlay addr:9555 advertised to the coordinator
	ReadListenAddr string // local bind addr:9555 (DISTINCT from SourceAddr by design)
}

// RenderDonorBundle renders every file of the canonical donor deployment.
// Keys are bundle-relative filenames.
func RenderDonorBundle(p DonorParams) (map[string][]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	data := tmplData{
		DonorParams:    p,
		FederationPort: PortFederationMTLS,
		ReadSourcePort: PortReadSourceMTLS,
		LighthousePort: PortNebulaLighthouse,
		KuboAPIAddr:    KuboAPIListen,
		SourceAddr:     fmt.Sprintf("%s:%d", p.NebulaAddr(), PortReadSourceMTLS),
		ReadListenAddr: fmt.Sprintf("0.0.0.0:%d", PortReadSourceMTLS),
	}

	funcs := template.FuncMap{
		// path prefixes an absolute container path with PathPrefix. EVERY
		// absolute path in a template must go through this so the fixture-tree
		// gate in D-M7.2-10a can relocate the whole tree.
		"path": func(abs string) string {
			if p.PathPrefix == "" {
				return abs
			}
			return path.Join(p.PathPrefix, abs)
		},
	}

	out := map[string][]byte{}
	for name, tmpl := range map[string]string{
		"compose.yaml":      "templates/donor-compose.yaml.tmpl",
		"node.yaml":         "templates/node.yaml.tmpl",
		"nebula-config.yml": "templates/nebula-config.yml.tmpl",
		"kubo-init.sh":      "templates/kubo-init.sh.tmpl",
		"README.md":         "templates/operator-README.txt.tmpl",
	} {
		t, err := template.New(path.Base(tmpl)).Funcs(funcs).ParseFS(templateFS, tmpl)
		if err != nil {
			return nil, fmt.Errorf("deploy: parse %s: %w", tmpl, err)
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, data); err != nil {
			return nil, fmt.Errorf("deploy: render %s: %w", tmpl, err)
		}
		out[name] = buf.Bytes()
	}
	return out, nil
}
