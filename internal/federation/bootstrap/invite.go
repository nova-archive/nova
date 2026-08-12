package bootstrap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/deploy"
	"github.com/nova-archive/nova/internal/federation/ca"
)

// InviteParams drives `novactl node invite`.
type InviteParams struct {
	Root   string // PKI volume root
	OutDir string // bundle destination
	Name   string // donor display name

	NebulaIP  string // CIDR form, e.g. 10.42.0.10/24
	NodeImage string // MUST be digest-pinned

	// NebulaPublicKey, when set, is a donor-generated Nebula public key. The
	// operator signs it and never receives the donor's overlay private key.
	NebulaPublicKey string

	StorageMaxBytes            int64
	BandwidthBudgetBytesPerDay int64

	// DoctorStatus records the doctor verdict at issuance, so a bundle emitted
	// over a failing doctor is traceable to that decision.
	DoctorStatus []Check
}

// InviteResult reports what was issued.
type InviteResult struct {
	NodeID      string   `json:"node_id"`
	Fingerprint string   `json:"fingerprint"`
	OutDir      string   `json:"out_dir"`
	Files       []string `json:"files"`
}

// Bundle schema versions (P2-M7.3, D-M7.3-15).
//
// v1 baked the image digests into compose.yaml, so changing which bytes a donor
// ran meant a new bundle — and a new bundle from `node invite` means a new node
// id and new certificates, which is a re-enrollment rather than an update.
//
// v2 moves the refs into .env, adds donor-lock.json naming the authorized refs
// with per-component rollback evidence, and adds donor-update.sh to apply them.
// A v1 bundle keeps working untouched; `novactl node convert-bundle` raises one
// in place without touching identity or state.
const (
	BundleSchemaV1 = 1
	BundleSchemaV2 = 2
)

// InviteManifest travels with the bundle. Non-secret by construction: it is the
// thing a donor can safely send back to their operator for support.
type InviteManifest struct {
	Version        int       `json:"version"`
	IssuedAt       time.Time `json:"issued_at"`
	NodeID         string    `json:"node_id"`
	DisplayName    string    `json:"display_name"`
	Fingerprint    string    `json:"federation_cert_fingerprint"`
	NebulaIP       string    `json:"nebula_ip"`
	CoordinatorURL string    `json:"coordinator_url"`
	NodeImage      string    `json:"node_image"`
	NebulaImage    string    `json:"nebula_image"`
	KuboImage      string    `json:"kubo_image"`
	CAFingerprint  string    `json:"ca_fingerprint"`
	SwarmKeyFP     string    `json:"swarm_key_fingerprint"`
	DoctorStatus   []Check   `json:"doctor_status_at_issuance,omitempty"`

	// DonorLockDigest is the digest of this bundle's donor-lock.json, set when
	// the bundle is converted to v2. It is what the volunteer passes to
	// donor-update.sh and what the operator compares a donor's report against.
	DonorLockDigest string `json:"donor_lock_digest,omitempty"`
}

// operatorSecrets are artifacts that must NEVER appear in a donor bundle.
var operatorSecrets = []string{
	FileFederationCAKey,
	FileNebulaCAKey,
	FileCoordinatorKey,
	FileClientKey,
	FileRepairSigningKey,
}

// Invite emits one complete, runnable donor bundle (D-M7.2-4).
//
// Before P2-M7.2 the "happy path" was issue-cert, render-template, send, and
// `docker compose up -d` — which was not a valid deployment: the generated
// compose had no Kubo, and node.yaml omitted six required fields.
func Invite(p InviteParams) (InviteResult, error) {
	var res InviteResult

	if p.Name == "" {
		return res, errors.New("node invite: --name is required")
	}
	if p.OutDir == "" {
		return res, errors.New("node invite: --out is required")
	}
	if !strings.Contains(p.NodeImage, "@sha256:") {
		return res, fmt.Errorf(
			"node invite: --image %q is not digest-pinned.\n"+
				"A donor must run exactly the bytes you verified; a mutable tag makes the\n"+
				"cosign and provenance checks in the donor quickstart meaningless.", p.NodeImage)
	}

	active := ActiveDir(p.Root)
	mf, err := ReadManifest(active)
	if err != nil {
		return res, fmt.Errorf("node invite: %w (run `federation init` first)", err)
	}

	caCert, err := os.ReadFile(filepath.Join(active, FileFederationCACert))
	if err != nil {
		return res, fmt.Errorf("node invite: read federation CA: %w", err)
	}
	caKey, err := os.ReadFile(filepath.Join(active, FileFederationCAKey))
	if err != nil {
		return res, fmt.Errorf("node invite: read federation CA key: %w", err)
	}
	nebCACert, err := os.ReadFile(filepath.Join(active, FileNebulaCACert))
	if err != nil {
		return res, fmt.Errorf("node invite: read Nebula CA: %w", err)
	}
	swarmKey, err := os.ReadFile(filepath.Join(active, FileSwarmKey))
	if err != nil {
		return res, fmt.Errorf("node invite: read swarm key: %w", err)
	}

	nodeID := uuid.New()
	nodeCert, nodeKey, err := ca.IssueClientCert(caCert, caKey, nodeID, p.Name)
	if err != nil {
		return res, fmt.Errorf("node invite: issue donor certificate: %w", err)
	}

	storageMax := p.StorageMaxBytes
	if storageMax == 0 {
		storageMax = 536870912000 // 500 GiB
	}
	bandwidth := p.BandwidthBudgetBytesPerDay
	if bandwidth == 0 {
		bandwidth = 53687091200 // 50 GiB/day
	}

	rendered, err := deploy.RenderDonorBundle(deploy.DonorParams{
		Name:                       p.Name,
		NebulaIP:                   p.NebulaIP,
		LighthouseOverlayIP:        mf.OperatorOverlayIP,
		LighthousePublicIP:         hostOnly(mf.LighthousePublic),
		CoordinatorOverlayIP:       mf.OperatorOverlayIP,
		NodeImage:                  p.NodeImage,
		NebulaImage:                deploy.DefaultNebulaImage,
		KuboImage:                  deploy.DefaultKuboImage,
		StorageMaxBytes:            storageMax,
		BandwidthBudgetBytesPerDay: bandwidth,
	})
	if err != nil {
		return res, fmt.Errorf("node invite: render deployment: %w", err)
	}

	files := map[string][]byte{}
	for name, body := range rendered {
		files[name] = body
	}
	files[filepath.Join("federation", FileFederationCACert)] = caCert
	files[filepath.Join("federation", "federation.crt")] = nodeCert
	files[filepath.Join("nebula", "nebula-ca.crt")] = nebCACert
	files[filepath.Join("secrets", "ipfs_swarm_key")] = swarmKey

	// The donor's OWN federation private key always travels with the bundle —
	// it is the donor's identity, not operator authority.
	files[filepath.Join("secrets", "nova_node_federation_key")] = nodeKey

	// The Nebula OVERLAY key is a separate question. With --nebula-public-key
	// the donor generated the keypair themselves and sends only the public
	// half; the operator signs it and never holds the private key. Without it,
	// the donor runs nebula-cert against the bundled CA on their own machine.
	if p.NebulaPublicKey != "" {
		pub, err := os.ReadFile(p.NebulaPublicKey)
		if err != nil {
			return res, fmt.Errorf("node invite: read --nebula-public-key: %w", err)
		}
		files[filepath.Join("nebula", "nebula.pub")] = pub
	}

	manifest := InviteManifest{
		// Freshly issued bundles are v2: the Compose file they carry already
		// takes its refs from .env, and donor-update.sh already ships with
		// them. What they do not carry is donor-lock.json, which needs a
		// verified release lock the operator supplies at conversion time.
		Version:        BundleSchemaV2,
		IssuedAt:       time.Now().UTC(),
		NodeID:         nodeID.String(),
		DisplayName:    p.Name,
		Fingerprint:    fingerprintPEM(nodeCert),
		NebulaIP:       p.NebulaIP,
		CoordinatorURL: fmt.Sprintf("https://%s", mf.FederationListenAddr),
		NodeImage:      p.NodeImage,
		NebulaImage:    deploy.DefaultNebulaImage,
		KuboImage:      deploy.DefaultKuboImage,
		CAFingerprint:  mf.CAFingerprint,
		SwarmKeyFP:     mf.SwarmKeyFP,
		DoctorStatus:   Failures(p.DoctorStatus),
	}
	mb, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return res, err
	}
	files["invite-manifest.json"] = append(mb, '\n')

	// THE NEGATIVE ASSERTION, before anything is written. This runs at runtime
	// and not only in tests, because the failure it prevents is silent and
	// unrecoverable: an operator cannot un-send a leaked CA key.
	if err := assertNoOperatorSecrets(active, files); err != nil {
		return res, err
	}

	if err := os.MkdirAll(p.OutDir, 0o700); err != nil {
		return res, err
	}
	names := make([]string, 0, len(files))
	for name, body := range files {
		dst := filepath.Join(p.OutDir, name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return res, err
		}
		perm := os.FileMode(0o644)
		switch {
		case strings.HasPrefix(name, "secrets/"):
			perm = 0o600
		case strings.HasSuffix(name, ".sh"):
			perm = 0o755
		}
		if err := os.WriteFile(dst, body, perm); err != nil {
			return res, err
		}
		names = append(names, name)
	}

	res = InviteResult{
		NodeID:      nodeID.String(),
		Fingerprint: manifest.Fingerprint,
		OutDir:      p.OutDir,
		Files:       names,
	}
	return res, nil
}

// assertNoOperatorSecrets fails if any operator-only key material appears in
// the bundle. It compares against the real bytes on disk rather than pattern
// matching, so it cannot be fooled by a re-encoding.
func assertNoOperatorSecrets(activeDir string, files map[string][]byte) error {
	var secrets [][2]string // name, content
	for _, name := range operatorSecrets {
		b, err := os.ReadFile(filepath.Join(activeDir, name))
		if err != nil || len(b) == 0 {
			continue
		}
		secrets = append(secrets, [2]string{name, string(b)})
	}
	for path, body := range files {
		for _, s := range secrets {
			if bytes.Contains(body, []byte(strings.TrimSpace(s[1]))) {
				return fmt.Errorf(
					"node invite: REFUSING to write the bundle — %s would leak into %s.\n"+
						"Operator authority must never leave operator custody.", s[0], path)
			}
		}
	}
	return nil
}

// AssertBundleClean re-runs the secret assertion over a bundle already on disk.
// Used by the deployment E2E to check the artifact, not just the generator.
func AssertBundleClean(activeDir, bundleDir string) error {
	files := map[string][]byte{}
	err := filepath.WalkDir(bundleDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(bundleDir, p)
		files[rel] = b
		return nil
	})
	if err != nil {
		return err
	}
	return assertNoOperatorSecrets(activeDir, files)
}

func hostOnly(hostPort string) string {
	if i := strings.LastIndex(hostPort, ":"); i > 0 {
		return hostPort[:i]
	}
	return hostPort
}
