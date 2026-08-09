package bootstrap

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// DoctorLocal is Plane A of `federation doctor` (D-M7.2-3): everything
// observable inside nova-admin's own mounts.
//
// Checks REPORT rather than erroring. An operator debugging a broken bootstrap
// needs the whole picture — stopping at the first problem hides the other four.
func DoctorLocal(root, operatorYAMLPath string) []Check {
	const plane = "A"
	var out []Check

	active := ActiveDir(root)
	read := func(name string) ([]byte, error) { return os.ReadFile(filepath.Join(active, name)) }

	// pki.parse — every certificate parses and pairs with its key.
	var parseProblems []string
	for _, pair := range [][2]string{
		{FileFederationCACert, FileFederationCAKey},
		{FileCoordinatorCert, FileCoordinatorKey},
		{FileClientCert, FileClientKey},
		{FileNebulaCACert, FileNebulaCAKey},
	} {
		certB, errC := read(pair[0])
		keyB, errK := read(pair[1])
		if errC != nil || errK != nil {
			parseProblems = append(parseProblems, fmt.Sprintf("%s/%s missing", pair[0], pair[1]))
			continue
		}
		if err := certKeyPairs(certB, keyB); err != nil {
			parseProblems = append(parseProblems, fmt.Sprintf("%s does not pair with %s: %v", pair[0], pair[1], err))
		}
	}
	if len(parseProblems) > 0 {
		out = append(out, Fail(plane, "pki.parse", strings.Join(parseProblems, "; ")))
	} else {
		out = append(out, Pass(plane, "pki.parse", "all certificate/key pairs parse and match"))
	}

	// pki.sans — the coordinator certificate must name what the manifest says,
	// or donors fail the server-cert check with a confusing error.
	mf, mfErr := readManifest(active)
	switch {
	case mfErr != nil:
		out = append(out, Fail(plane, "pki.sans", fmt.Sprintf("cannot read manifest: %v", mfErr)))
	default:
		certB, err := read(FileCoordinatorCert)
		if err != nil {
			out = append(out, Fail(plane, "pki.sans", fmt.Sprintf("cannot read %s: %v", FileCoordinatorCert, err)))
			break
		}
		c, err := parseCertPEM(certB)
		if err != nil {
			out = append(out, Fail(plane, "pki.sans", fmt.Sprintf("%s does not parse: %v", FileCoordinatorCert, err)))
			break
		}
		var problems []string
		if !hasDNSName(c, mf.Hostname) {
			problems = append(problems, fmt.Sprintf("hostname %q not in SANs (%v)", mf.Hostname, c.DNSNames))
		}
		if !hasIP(c, mf.OperatorOverlayIP) {
			problems = append(problems, fmt.Sprintf("overlay IP %s not in SANs (%s)", mf.OperatorOverlayIP, joinIPs(c.IPAddresses)))
		}
		if len(problems) > 0 {
			out = append(out, Fail(plane, "pki.sans", strings.Join(problems, "; ")))
		} else {
			out = append(out, Pass(plane, "pki.sans", "coordinator certificate names the configured hostname and overlay IP"))
		}
	}

	// pki.seed — without a usable repair seed the source endpoint degrades to
	// 503 and no grants are minted, which looks like a donor problem.
	if b, err := read(FileRepairSigningKey); err != nil {
		out = append(out, Fail(plane, "pki.seed", fmt.Sprintf("cannot read %s: %v", FileRepairSigningKey, err)))
	} else {
		raw, decErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		switch {
		case decErr != nil:
			out = append(out, Fail(plane, "pki.seed", fmt.Sprintf("not valid base64: %v", decErr)))
		case len(raw) != ed25519.PrivateKeySize:
			out = append(out, Fail(plane, "pki.seed",
				fmt.Sprintf("%d bytes, want %d for an Ed25519 private key", len(raw), ed25519.PrivateKeySize)))
		default:
			out = append(out, Pass(plane, "pki.seed", "repair signing key parses"))
		}
	}

	// pki.perms — key material must not be group- or world-readable.
	var loose []string
	for _, name := range []string{
		FileFederationCAKey, FileCoordinatorKey, FileClientKey,
		FileRepairSigningKey, FileNebulaCAKey, FileLighthouseKey, FileSwarmKey,
	} {
		info, err := os.Stat(filepath.Join(active, name))
		if err != nil {
			continue
		}
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			loose = append(loose, fmt.Sprintf("%s is %o", name, mode))
		}
	}
	if len(loose) > 0 {
		out = append(out, Fail(plane, "pki.perms", strings.Join(loose, "; ")))
	} else {
		out = append(out, Pass(plane, "pki.perms", "no key material is group- or world-readable"))
	}

	// cfg.manifest — operator.yaml must agree with the manifest, or the
	// coordinator binds something other than what was bootstrapped.
	out = append(out, checkOperatorYAML(plane, operatorYAMLPath, mf, mfErr == nil))

	// swarm.key — the fingerprint must match the manifest. A changed swarm key
	// silently partitions every donor holding the old one.
	if b, err := read(FileSwarmKey); err != nil {
		out = append(out, Fail(plane, "swarm.key", fmt.Sprintf("cannot read %s: %v", FileSwarmKey, err)))
	} else if mfErr == nil && mf.SwarmKeyFP != "" && fingerprintBytes(b) != mf.SwarmKeyFP {
		out = append(out, Fail(plane, "swarm.key",
			"active swarm key does not match the manifest; donors holding the recorded key are partitioned from the swarm"))
	} else if !strings.HasPrefix(string(b), "/key/swarm/psk/1.0.0/") {
		out = append(out, Fail(plane, "swarm.key", "not in Kubo PSK format; Kubo will refuse to start"))
	} else {
		out = append(out, Pass(plane, "swarm.key", "swarm key present and matches the manifest"))
	}

	return out
}

// operatorYAMLFederation is the subset of operator.yaml this check reads.
type operatorYAMLFederation struct {
	Federation struct {
		ListenAddr         string `yaml:"listen_addr"`
		NebulaInterface    string `yaml:"nebula_interface"`
		FederationCAPath   string `yaml:"federation_ca_path"`
		FederationCertPath string `yaml:"federation_cert_path"`
		FederationKeyPath  string `yaml:"federation_key_path"`
	} `yaml:"federation"`
}

func checkOperatorYAML(plane, path string, mf Manifest, haveManifest bool) Check {
	const id = "cfg.manifest"
	if path == "" {
		return Skip(plane, id, "no operator.yaml path given")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Fail(plane, id, fmt.Sprintf("cannot read %s: %v", path, err))
	}
	var doc operatorYAMLFederation
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return Fail(plane, id, fmt.Sprintf("%s does not parse: %v", path, err))
	}
	if doc.Federation.ListenAddr == "" {
		return Fail(plane, id, "operator.yaml has an empty federation.listen_addr; federation is disabled")
	}
	if haveManifest && doc.Federation.ListenAddr != mf.FederationListenAddr {
		return Fail(plane, id, fmt.Sprintf(
			"operator.yaml listen_addr %q disagrees with the manifest %q",
			doc.Federation.ListenAddr, mf.FederationListenAddr))
	}
	if doc.Federation.NebulaInterface == "" {
		return Fail(plane, id, "federation.nebula_interface is empty; the listener would not be overlay-bound")
	}
	for name, p := range map[string]string{
		"federation_ca_path":   doc.Federation.FederationCAPath,
		"federation_cert_path": doc.Federation.FederationCertPath,
		"federation_key_path":  doc.Federation.FederationKeyPath,
	} {
		if p == "" {
			return Fail(plane, id, fmt.Sprintf("federation.%s is empty", name))
		}
	}
	return Pass(plane, id, "operator.yaml federation block agrees with the manifest")
}
