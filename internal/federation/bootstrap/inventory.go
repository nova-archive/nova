package bootstrap

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Action is what Init decided to do with the state it found.
type Action int

const (
	ActionCreate  Action = iota // nothing present: greenfield
	ActionAdopt                 // present and consistent: carry it forward
	ActionRefuse                // present and inconsistent: stop, show the diff
	ActionReplace               // operator explicitly asked to destroy authority
)

func (a Action) String() string {
	switch a {
	case ActionCreate:
		return "create"
	case ActionAdopt:
		return "adopt"
	case ActionRefuse:
		return "refuse"
	case ActionReplace:
		return "replace"
	}
	return "unknown"
}

// Origin records where an inventoried artifact came from.
type Origin string

const (
	OriginActive Origin = "active" // already in the PKI volume
	OriginImport Origin = "import" // presented via --adopt-from
)

// Found is one inventoried artifact.
type Found struct {
	Name   string
	Origin Origin
	Path   string
	Data   []byte
}

// State is the result of inventorying existing federation material.
//
// It covers the D-M7.2-2b preservation inventory: federation CA cert and key,
// coordinator server identity, coordinator client identity, repair seed,
// Nebula CA cert and key, lighthouse identity, and the Kubo swarm key. Donor
// identity and registration state live in Postgres and are NEVER touched here.
type State struct {
	Root    string
	Items   map[string]Found
	Missing []string
}

// preservedArtifacts is the exact set adoption must carry forward unchanged.
var preservedArtifacts = []string{
	FileFederationCACert,
	FileFederationCAKey,
	FileCoordinatorCert,
	FileCoordinatorKey,
	FileClientCert,
	FileClientKey,
	FileRepairSigningKey,
	FileNebulaCACert,
	FileNebulaCAKey,
	FileLighthouseCert,
	FileLighthouseKey,
	FileSwarmKey,
}

// Inventory reads existing material from <root>/active and, when importDir is
// set, from that read-only directory. Active always wins: adoption never
// overwrites what is already live.
func Inventory(root, importDir string) (State, error) {
	st := State{Root: root, Items: map[string]Found{}}
	active := ActiveDir(root)

	for _, name := range preservedArtifacts {
		if b, err := os.ReadFile(filepath.Join(active, name)); err == nil {
			st.Items[name] = Found{Name: name, Origin: OriginActive, Path: filepath.Join(active, name), Data: b}
			continue
		}
		if importDir != "" {
			p := filepath.Join(importDir, name)
			if b, err := os.ReadFile(p); err == nil {
				st.Items[name] = Found{Name: name, Origin: OriginImport, Path: p, Data: b}
				continue
			}
		}
		st.Missing = append(st.Missing, name)
	}
	sort.Strings(st.Missing)
	return st, nil
}

// Has reports whether an artifact was found.
func (s State) Has(name string) bool { _, ok := s.Items[name]; return ok }

// Get returns an inventoried artifact's bytes.
func (s State) Get(name string) ([]byte, bool) {
	f, ok := s.Items[name]
	if !ok {
		return nil, false
	}
	return f.Data, true
}

// HasAnyAuthority reports whether either CA is already present. This is what
// distinguishes greenfield from adoption.
func (s State) HasAnyAuthority() bool {
	return s.Has(FileFederationCACert) || s.Has(FileNebulaCACert)
}

// Diff is one reason adoption was refused.
type Diff struct {
	Artifact string
	Want     string
	Got      string
	Why      string
}

// Classify decides create / adopt / refuse / replace, and on refusal explains
// exactly what disagrees.
//
// Refuse-to-clobber and adopt are the SAME code path seen from two sides: both
// begin by inventorying, and the only question is whether what is there agrees
// with what was asked for.
func (s State) Classify(p Params) (Action, []Diff) {
	if p.ReplaceAuthority {
		return ActionReplace, nil
	}
	if !s.HasAnyAuthority() {
		return ActionCreate, nil
	}

	var diffs []Diff

	// The coordinator server cert must already name this hostname and overlay
	// IP, or the adopted federation would not match the requested parameters.
	if certPEM, ok := s.Get(FileCoordinatorCert); ok {
		c, err := parseCertPEM(certPEM)
		if err != nil {
			diffs = append(diffs, Diff{
				Artifact: FileCoordinatorCert, Why: fmt.Sprintf("unparseable: %v", err),
			})
		} else {
			if p.Hostname != "" && !hasDNSName(c, p.Hostname) {
				diffs = append(diffs, Diff{
					Artifact: FileCoordinatorCert,
					Want:     p.Hostname,
					Got:      strings.Join(c.DNSNames, ","),
					Why:      "existing coordinator certificate does not name the requested hostname",
				})
			}
			if p.OperatorOverlayIP != "" && !hasIP(c, p.OperatorOverlayIP) {
				diffs = append(diffs, Diff{
					Artifact: FileCoordinatorCert,
					Want:     p.OperatorOverlayIP,
					Got:      joinIPs(c.IPAddresses),
					Why:      "existing coordinator certificate does not name the requested overlay IP",
				})
			}
		}
	}

	// A CA present without its key cannot issue, and re-creating the key is not
	// possible — that is a refusal, not something to paper over.
	if s.Has(FileFederationCACert) && !s.Has(FileFederationCAKey) {
		diffs = append(diffs, Diff{
			Artifact: FileFederationCAKey,
			Why:      "federation CA certificate is present but its private key is not; issuance is impossible",
		})
	}
	if s.Has(FileNebulaCACert) && !s.Has(FileNebulaCAKey) {
		diffs = append(diffs, Diff{
			Artifact: FileNebulaCAKey,
			Why:      "Nebula CA certificate is present but its private key is not",
		})
	}

	if len(diffs) > 0 {
		return ActionRefuse, diffs
	}
	return ActionAdopt, nil
}

func refusalError(diffs []Diff) error {
	var b strings.Builder
	b.WriteString("federation init: refusing to modify the existing federation.\n\n")
	for _, d := range diffs {
		fmt.Fprintf(&b, "  %s: %s\n", d.Artifact, d.Why)
		if d.Want != "" || d.Got != "" {
			fmt.Fprintf(&b, "      requested: %s\n      existing:  %s\n", d.Want, d.Got)
		}
	}
	b.WriteString("\nNothing was changed. Either re-run with parameters matching the existing\n")
	b.WriteString("federation, or, to destroy and recreate it, see --replace-authority in\n")
	b.WriteString("docs/reference/operator-configuration.md.\n")
	return fmt.Errorf("%s", b.String())
}

// authorizeReplacement enforces D-M7.2-2d. Overwriting generated config and
// destroying a federation CA are not the same act, so there is no generic
// --force: replacement refuses outright while donors exist unless the operator
// also acknowledges exactly how many they are about to orphan.
func authorizeReplacement(p Params) error {
	if p.RegisteredDonorCount == nil {
		return nil
	}
	n, err := p.RegisteredDonorCount()
	if err != nil {
		return fmt.Errorf("federation init: could not count registered donors: %w", err)
	}
	if n == 0 || p.DestroyExistingFederation {
		return nil
	}
	return fmt.Errorf(
		"federation init: --replace-authority would orphan %d registered donor(s).\n"+
			"Every issued certificate becomes untrusted and every donor must re-enroll.\n"+
			"If that is genuinely what you want, add --destroy-existing-federation.", n)
}

// ensurePair stages a cert/key pair, preserving an existing one unless the
// operator explicitly asked to replace authority.
func ensurePair(stage *Stage, st State, res *Result, replace bool, certName, keyName string,
	gen func() ([]byte, []byte, error)) (certPEM, keyPEM []byte, err error) {

	haveCert, okC := st.Get(certName)
	haveKey, okK := st.Get(keyName)

	if !replace && okC && okK {
		// Preserve. Import-origin material is COPIED into the PKI volume;
		// already-active material stays where it is.
		if st.Items[certName].Origin == OriginImport {
			if err := stage.Put(certName, haveCert, perm0644); err != nil {
				return nil, nil, err
			}
			if err := stage.Put(keyName, haveKey, perm0600); err != nil {
				return nil, nil, err
			}
			res.Adopted += 2
			res.Notes = append(res.Notes, "adopted-from-import: "+certName+", "+keyName)
		} else {
			res.Existing += 2
		}
		return haveCert, haveKey, nil
	}

	certPEM, keyPEM, err = gen()
	if err != nil {
		return nil, nil, fmt.Errorf("federation init: generate %s: %w", certName, err)
	}
	if err := stage.Put(certName, certPEM, perm0644); err != nil {
		return nil, nil, err
	}
	if err := stage.Put(keyName, keyPEM, perm0600); err != nil {
		return nil, nil, err
	}
	res.Created += 2
	return certPEM, keyPEM, nil
}

// ensureSecret stages a single secret, preserving an existing one.
//
// This is the path that enforces the swarm-key invariant: an existing
// swarm.key is NEVER regenerated, because a donor already holding it is
// silently partitioned from the private swarm if it changes.
func ensureSecret(stage *Stage, st State, res *Result, replace bool, name string,
	gen func() ([]byte, error)) error {

	if have, ok := st.Get(name); ok {
		if replace && name == FileSwarmKey {
			return fmt.Errorf(
				"federation init: refusing to rotate an existing %s even with --replace-authority.\n"+
					"Every donor holding the current key would be silently partitioned from the\n"+
					"private swarm: still up, still registered, unable to exchange blocks.\n"+
					"Remove it deliberately if that is truly intended.", FileSwarmKey)
		}
		if !replace {
			if st.Items[name].Origin == OriginImport {
				if err := stage.Put(name, have, perm0600); err != nil {
					return err
				}
				res.Adopted++
				res.Notes = append(res.Notes, "adopted-from-import: "+name)
			} else {
				res.Existing++
			}
			return nil
		}
	}

	data, err := gen()
	if err != nil {
		return fmt.Errorf("federation init: generate %s: %w", name, err)
	}
	if err := stage.Put(name, data, perm0600); err != nil {
		return err
	}
	res.Created++
	return nil
}

// stagedOrActive reads an artifact from the staging generation if it was
// written there, else from the inventory.
func stagedOrActive(stage *Stage, st State, name string) ([]byte, bool) {
	if stage.Has(name) {
		if b, err := os.ReadFile(filepath.Join(stage.Dir(), name)); err == nil {
			return b, true
		}
	}
	return st.Get(name)
}

// --- helpers ---------------------------------------------------------------

func parseCertPEM(b []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	return x509.ParseCertificate(blk.Bytes)
}

func hasDNSName(c *x509.Certificate, want string) bool {
	for _, n := range c.DNSNames {
		if strings.EqualFold(n, want) {
			return true
		}
	}
	return false
}

func hasIP(c *x509.Certificate, want string) bool {
	w := net.ParseIP(want)
	for _, ip := range c.IPAddresses {
		if ip.Equal(w) {
			return true
		}
	}
	return false
}

func joinIPs(ips []net.IP) string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return strings.Join(out, ",")
}

func fingerprintPEM(b []byte) string {
	c, err := parseCertPEM(b)
	if err != nil {
		return fingerprintBytes(b)
	}
	sum := sha256.Sum256(c.Raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func fingerprintBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
