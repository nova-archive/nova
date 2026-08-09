package bootstrap

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/federation/ca"
)

// Artifact names inside the active PKI directory. These are the canonical
// spellings; adoption (D-M7.2-2c) expects the same names under --adopt-from.
const (
	FileFederationCACert              = "federation-ca.crt"
	FileFederationCAKey               = "federation-ca.key"
	FileCoordinatorCert               = "coordinator-federation.crt"
	FileCoordinatorKey                = "coordinator-federation.key"
	FileClientCert                    = "federation-client.crt"
	FileClientKey                     = "federation-client.key"
	FileRepairSigningKey              = "repair-signing.key"
	FileNebulaCACert                  = "nebula-ca.crt"
	FileNebulaCAKey                   = "nebula-ca.key"
	FileLighthouseCert                = "lighthouse.crt"
	FileLighthouseKey                 = "lighthouse.key"
	FileSwarmKey                      = "swarm.key"
	FileManifest                      = "federation-manifest.json"
	activeDirName                     = "active"
	defaultFederationPort             = 9443
	defaultLighthousePort             = 4242
	perm0600              os.FileMode = 0o600
	perm0644              os.FileMode = 0o644
)

// Params drives federation init.
type Params struct {
	Root string // PKI volume root; artifacts land in <Root>/active

	OverlayCIDR       string // e.g. 10.42.0.0/24
	OperatorOverlayIP string // e.g. 10.42.0.1
	LighthousePublic  string // e.g. 203.0.113.7:4242
	Hostname          string // e.g. nova.example.org

	// AdoptFrom is a read-only directory holding a pre-existing hand-built PKI
	// (D-M7.2-2c). Empty means greenfield-or-adopt-in-place.
	AdoptFrom string

	// Destructive, and deliberately awkward. See D-M7.2-2d.
	ReplaceAuthority          bool
	DestroyExistingFederation bool
	RegisteredDonorCount      func() (int, error)

	// SkipPreflight bypasses /dev/net/tun and route-conflict checks (tests).
	SkipPreflight bool

	// FailAfter injects a failure after a named stage, for crash-recovery
	// tests: "stage", "fsync", "validate", "activate".
	FailAfter string
}

// Result reports what init did, so an operator can see adoption vs creation
// rather than having to infer it.
type Result struct {
	Created  int      `json:"created"`
	Adopted  int      `json:"adopted"`
	Existing int      `json:"existing"`
	Notes    []string `json:"notes,omitempty"`
	Manifest Manifest `json:"manifest"`
}

// Manifest is the NON-SECRET record of addresses and fingerprints. doctor,
// invite and support read this rather than re-deriving from key material.
type Manifest struct {
	Version              int       `json:"version"`
	CreatedAt            time.Time `json:"created_at"`
	Hostname             string    `json:"hostname"`
	OverlayCIDR          string    `json:"overlay_cidr"`
	OperatorOverlayIP    string    `json:"operator_overlay_ip"`
	LighthousePublic     string    `json:"lighthouse_public"`
	FederationListenAddr string    `json:"federation_listen_addr"`
	CAFingerprint        string    `json:"ca_fingerprint"`
	CoordinatorFP        string    `json:"coordinator_fingerprint"`
	ClientFP             string    `json:"client_fingerprint"`
	SwarmKeyFP           string    `json:"swarm_key_fingerprint"`
	NebulaCAFingerprint  string    `json:"nebula_ca_fingerprint"`
}

// ActiveDir is where activated artifacts live.
func ActiveDir(root string) string { return filepath.Join(root, activeDirName) }

// Validate checks the parameters that must hold regardless of greenfield or
// adoption.
func (p Params) Validate() error {
	if p.Root == "" {
		return errors.New("federation init: --root is required")
	}
	if p.OverlayCIDR == "" {
		return errors.New("federation init: --overlay-cidr is required")
	}
	_, netw, err := net.ParseCIDR(p.OverlayCIDR)
	if err != nil {
		return fmt.Errorf("federation init: --overlay-cidr %q: %w", p.OverlayCIDR, err)
	}
	if p.OperatorOverlayIP == "" {
		return errors.New("federation init: --operator-overlay-ip is required")
	}
	ip := net.ParseIP(p.OperatorOverlayIP)
	if ip == nil {
		return fmt.Errorf("federation init: --operator-overlay-ip %q is not an IP", p.OperatorOverlayIP)
	}
	if !netw.Contains(ip) {
		return fmt.Errorf("federation init: --operator-overlay-ip %s is outside --overlay-cidr %s",
			p.OperatorOverlayIP, p.OverlayCIDR)
	}
	if p.Hostname == "" {
		return errors.New("federation init: --hostname is required")
	}
	if p.LighthousePublic == "" {
		return errors.New("federation init: --lighthouse-public is required")
	}
	if _, _, err := net.SplitHostPort(p.LighthousePublic); err != nil {
		return fmt.Errorf("federation init: --lighthouse-public %q is not host:port: %w", p.LighthousePublic, err)
	}
	if p.DestroyExistingFederation && !p.ReplaceAuthority {
		return errors.New("federation init: --destroy-existing-federation requires --replace-authority")
	}
	return nil
}

// FederationListenAddr is the overlay address the coordinator binds.
func (p Params) FederationListenAddr() string {
	return fmt.Sprintf("%s:%d", p.OperatorOverlayIP, defaultFederationPort)
}

// Init bootstraps or adopts the federation.
//
// It is failure-atomic with respect to activation (D-M7.2-2a) and idempotent:
// a repeat run with identical parameters is a no-op that never regenerates an
// existing CA.
//
// It NEVER touches the nodes table. This package deliberately does not import
// internal/db — adoption is a filesystem and PKI operation, and a donor's
// registration must survive it untouched (preservation inventory row 9).
func Init(p Params) (Result, error) {
	var res Result
	if err := p.Validate(); err != nil {
		return res, err
	}
	if !p.SkipPreflight {
		if err := preflight(p); err != nil {
			return res, err
		}
	}

	active := ActiveDir(p.Root)
	if err := os.MkdirAll(active, 0o700); err != nil {
		return res, fmt.Errorf("federation init: create %s: %w", active, err)
	}

	state, err := Inventory(p.Root, p.AdoptFrom)
	if err != nil {
		return res, err
	}
	action, diffs := state.Classify(p)
	switch action {
	case ActionRefuse:
		return res, refusalError(diffs)
	case ActionReplace:
		if err := authorizeReplacement(p); err != nil {
			return res, err
		}
	}

	stage, err := NewStage(p.Root)
	if err != nil {
		return res, err
	}
	defer func() { _ = DiscardOrphans(p.Root, "") }()

	if err := buildFederation(stage, p, state, &res, action); err != nil {
		return res, err
	}
	if p.FailAfter == "stage" {
		return res, errors.New("federation init: injected failure after stage")
	}
	if p.FailAfter == "fsync" {
		return res, errors.New("federation init: injected failure after fsync")
	}

	if err := stage.Validate(func(fsys fs.FS) error { return validateStaged(fsys, p, stage) }); err != nil {
		return res, err
	}
	if p.FailAfter == "validate" {
		return res, errors.New("federation init: injected failure after validate")
	}

	targets := map[string]string{}
	for _, rel := range stage.Files() {
		targets[rel] = filepath.Join(active, rel)
	}
	if err := stage.Activate(targets); err != nil {
		return res, err
	}
	if p.FailAfter == "activate" {
		return res, errors.New("federation init: injected failure after activate")
	}

	_ = stage.Discard()
	if err := DiscardOrphans(p.Root, ""); err != nil {
		return res, err
	}

	mf, err := readManifest(active)
	if err != nil {
		return res, err
	}
	res.Manifest = mf
	return res, nil
}

// buildFederation stages every missing artifact, preserving everything the
// inventory already found (the D-M7.2-2b preservation inventory).
func buildFederation(stage *Stage, p Params, state State, res *Result, action Action) error {
	replace := action == ActionReplace

	// 1-3. Federation CA.
	caCert, caKey, err := ensurePair(stage, state, res, replace,
		FileFederationCACert, FileFederationCAKey, ca.GenerateCA)
	if err != nil {
		return err
	}

	// 4. Coordinator SERVER identity, SANs bound to the overlay IP + hostname.
	if _, _, err := ensurePair(stage, state, res, replace,
		FileCoordinatorCert, FileCoordinatorKey, func() ([]byte, []byte, error) {
			return ca.IssueServerCert(caCert, caKey, ca.ServerCertOptions{
				DNSNames:    []string{p.Hostname},
				IPAddresses: []string{p.OperatorOverlayIP},
			})
		}); err != nil {
		return err
	}

	// 5. Coordinator CLIENT identity — required since P2-M4.1 for donor-backed
	// reads and possession audits, and absent from every prior bootstrap doc.
	if _, _, err := ensurePair(stage, state, res, replace,
		FileClientCert, FileClientKey, func() ([]byte, []byte, error) {
			return ca.IssueCoordinatorClientCert(caCert, caKey, uuid.New())
		}); err != nil {
		return err
	}

	// 6. Repair-token signing seed. Without it the coordinator degrades to a
	// 503 source endpoint; also never mentioned in any prior bootstrap doc.
	if err := ensureSecret(stage, state, res, replace, FileRepairSigningKey, func() ([]byte, error) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		return []byte(base64.StdEncoding.EncodeToString(priv) + "\n"), nil
	}); err != nil {
		return err
	}

	// 7. Nebula CA + lighthouse identity. The lighthouse side was generated
	// nowhere before this milestone.
	nebCert, nebKey, err := ensurePair(stage, state, res, replace,
		FileNebulaCACert, FileNebulaCAKey, ca.GenerateCA)
	if err != nil {
		return err
	}
	if _, _, err := ensurePair(stage, state, res, replace,
		FileLighthouseCert, FileLighthouseKey, func() ([]byte, []byte, error) {
			return ca.IssueServerCert(nebCert, nebKey, ca.ServerCertOptions{
				DNSNames:    []string{p.Hostname},
				IPAddresses: []string{p.OperatorOverlayIP},
			})
		}); err != nil {
		return err
	}

	// The Kubo swarm key is NEVER rotated during adoption — see ensureSecret's
	// preservation path. Rotating it silently partitions every existing donor
	// from the private swarm: they stay up, register, and cannot exchange
	// blocks.
	if err := ensureSecret(stage, state, res, replace, FileSwarmKey, generateSwarmKey); err != nil {
		return err
	}

	// 9. Non-secret manifest.
	mf := Manifest{
		Version:              1,
		CreatedAt:            time.Now().UTC(),
		Hostname:             p.Hostname,
		OverlayCIDR:          p.OverlayCIDR,
		OperatorOverlayIP:    p.OperatorOverlayIP,
		LighthousePublic:     p.LighthousePublic,
		FederationListenAddr: p.FederationListenAddr(),
		CAFingerprint:        fingerprintPEM(caCert),
		NebulaCAFingerprint:  fingerprintPEM(nebCert),
	}
	if b, ok := stagedOrActive(stage, state, FileCoordinatorCert); ok {
		mf.CoordinatorFP = fingerprintPEM(b)
	}
	if b, ok := stagedOrActive(stage, state, FileClientCert); ok {
		mf.ClientFP = fingerprintPEM(b)
	}
	if b, ok := stagedOrActive(stage, state, FileSwarmKey); ok {
		mf.SwarmKeyFP = fingerprintBytes(b)
	}
	body, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		return err
	}
	return stage.Put(FileManifest, append(body, '\n'), perm0644)
}

// generateSwarmKey produces a Kubo private-swarm PSK in the documented format.
func generateSwarmKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return fmt.Appendf(nil, "/key/swarm/psk/1.0.0/\n/base16/\n%x\n", key), nil
}

// preflight checks the host can actually carry an overlay before anything is
// written. Cheap to run, and a confusing failure later otherwise.
func preflight(p Params) error {
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		return fmt.Errorf("federation init: /dev/net/tun is unavailable (%v); "+
			"the Nebula sidecar needs it. On WSL2 see docs/platforms/wsl2-donor.md", err)
	}
	_, overlay, err := net.ParseCIDR(p.OverlayCIDR)
	if err != nil {
		return err
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil // not fatal; the bind guard catches a real conflict later
	}
	for _, ifi := range ifaces {
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			if overlay.Contains(ipnet.IP) && ifi.Name != "nebula1" {
				return fmt.Errorf("federation init: overlay %s conflicts with %s on interface %s; "+
					"choose a different --overlay-cidr", p.OverlayCIDR, ipnet.String(), ifi.Name)
			}
		}
	}
	return nil
}

func readManifest(activeDir string) (Manifest, error) {
	var mf Manifest
	b, err := os.ReadFile(filepath.Join(activeDir, FileManifest))
	if err != nil {
		return mf, fmt.Errorf("federation init: read manifest: %w", err)
	}
	if err := json.Unmarshal(b, &mf); err != nil {
		return mf, fmt.Errorf("federation init: parse manifest: %w", err)
	}
	return mf, nil
}
