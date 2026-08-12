package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// LockSchema is the lock document's own format version.
const LockSchema = 1

// Bundle member paths the lock's payload map covers. Fixed names so every
// consumer — the bootstrap script, the target admin, the donor updater — looks
// in the same place.
const (
	MemberIntent     = "intent.json"
	MemberPolicy     = "verification-policy.txt"
	MemberEvidence   = "evidence/"
	MemberComposeEnv = "release.env"
	MemberUpgrading  = "UPGRADING.md"
	MemberBootstrap  = "scripts/nova-release"
	MemberHashes     = "hashes.txt"
)

// requiredMembers must appear in every lock's payload map. Evidence statements
// are additional entries under evidence/.
var requiredMembers = []string{
	MemberIntent, MemberPolicy, MemberComposeEnv, MemberUpgrading, MemberBootstrap,
}

// excludedFromPayload can never appear in the payload map, because hashing them
// would make the authentication graph cyclic (D-M7.3-2d).
//
//   - the lock cannot hash itself;
//   - it cannot hash the Sigstore bundle, which CONTAINS the signature over it;
//   - it cannot hash the signing certificate carried in that bundle's chain.
//
// hashes.txt may be hashed, because it is DERIVED from the payload map and
// covers none of these.
var excludedFromPayload = []string{
	"lock.json",
	"lock.sigstore.json",
	"lock.pem",
	"lock.crt",
}

// EvidenceRef binds a claim to a signed statement by CONTENT DIGEST.
//
// Not by CI run ID: logs expire, runs are deleted, and a run ID proves nothing
// about what the run concluded. The statement itself carries artifact digests,
// source commit, scenario and capability IDs, test revision, outcome,
// timestamps and runner class, and is attached to the release.
type EvidenceRef struct {
	// StatementDigest is the sha256 of the signed evidence statement's bytes.
	StatementDigest string `json:"statement_digest"`
	// Gate names which gate produced it.
	Gate string `json:"gate"`
	// Outcome is "passed" or "skipped". A claim proven by a skipped gate is
	// rejected; requirement and outcome are separate axes everywhere else, and
	// a claim is by definition required.
	Outcome string `json:"outcome"`
	// RunnerClass records the tier the gate ran in (pr-static, rc-docker,
	// release-tun), so a static gate cannot silently satisfy an executed claim.
	RunnerClass string `json:"runner_class"`
	// At is when the gate ran.
	At time.Time `json:"at"`
}

// ProvenClaim is a claim WITH evidence. It exists only on the lock; the
// intent-side DeclaredClaim has no evidence field at all, so a declared claim
// cannot acquire proof by being assigned to the wrong variable.
type ProvenClaim struct {
	ID        string        `json:"id"`
	Statement string        `json:"statement"`
	Gate      string        `json:"gate"`
	Evidence  []EvidenceRef `json:"evidence"`
}

// LockedArtifact is one released image, recorded as an OCI DESCRIPTOR.
//
// A bare digest string cannot say whether it names an index or a single
// manifest, and promotion behaves differently for the two: `docker buildx
// imagetools create` builds a NEW index around a single-platform manifest
// unless told otherwise, which changes the digest the lock just committed to.
// The mediaType is what lets the read-back assert the right thing.
type LockedArtifact struct {
	Repository string             `json:"repository"`
	Descriptor ocispec.Descriptor `json:"descriptor"`
	// Manifests are the child descriptors when Descriptor is an index.
	Manifests []ocispec.Descriptor `json:"manifests,omitempty"`
}

// Ref renders the pinned reference an operator or Compose file uses.
func (a LockedArtifact) Ref() string { return a.Repository + "@" + a.Descriptor.Digest.String() }

// Lock is the post-build, signed record of what a release IS.
type Lock struct {
	Schema  int    `json:"schema"`
	Version string `json:"version"`

	// IntentDigest is the sha256 of the exact intent bytes this lock realizes.
	// It is what ties the reviewed decision to the built artifact.
	IntentDigest string `json:"intent_digest"`

	SourceCommit string    `json:"source_commit"`
	BuiltAt      time.Time `json:"built_at"`
	TargetSchema int       `json:"target_schema"`

	Artifacts map[string]LockedArtifact `json:"artifacts"`
	Sidecars  map[string]string         `json:"sidecars"`

	// DonorRollback is the release's per-component verdict on reverting a
	// donor, keyed by ComponentNames. Absent means NO for that component: the
	// donor topology holds replicas and a registration, and an unevidenced
	// rollback of software that has already written to them is a guess made
	// with somebody else's data.
	//
	// It lives here rather than being chosen at bundle-conversion time so the
	// donor-lock projection stays a deterministic function of this document —
	// which is what lets this document carry the projection's digest.
	DonorRollback map[string]RollbackEvidence `json:"donor_rollback,omitempty"`

	ProvenClaims []ProvenClaim `json:"proven_claims"`

	// Payload maps each bundle member's path to its sha256. This is the ONLY
	// authenticated path to every deployable byte in the bundle.
	Payload map[string]string `json:"payload"`

	// DonorLockDigest is the sha256 of the deterministic donor-lock projection
	// (node, Nebula and Kubo refs). The donor lock is not separately signed: it
	// is a projection of this document, and this digest is what binds it.
	DonorLockDigest string `json:"donor_lock_digest"`
}

// ParseLock decodes and validates lock bytes against the intent it claims to
// realize. Unknown and trailing JSON are rejected for the same reason as the
// intent: this is release metadata, not the wire path.
func ParseLock(b []byte, in Intent, intentBytes []byte) (Lock, error) {
	var l Lock
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return Lock{}, fmt.Errorf("lock: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return Lock{}, errors.New("lock: trailing content after the JSON document")
	}
	if err := l.Validate(in, intentBytes); err != nil {
		return Lock{}, err
	}
	return l, nil
}

// Validate enforces every rule that makes a lock usable as a trust anchor's
// payload description.
func (l Lock) Validate(in Intent, intentBytes []byte) error {
	if l.Schema != LockSchema {
		return fmt.Errorf("lock: schema %d, want %d", l.Schema, LockSchema)
	}
	if l.Version != in.Version {
		return fmt.Errorf("lock: version %q does not match the intent's %q", l.Version, in.Version)
	}
	if want := IntentDigest(intentBytes); l.IntentDigest != want {
		return fmt.Errorf("lock: intent_digest %s does not match the intent bytes (%s); the "+
			"reviewed decision and the built artifact are not the same document",
			l.IntentDigest, want)
	}
	if l.TargetSchema != in.TargetSchema {
		return fmt.Errorf("lock: target_schema %d does not match the intent's %d",
			l.TargetSchema, in.TargetSchema)
	}
	if len(l.SourceCommit) < 7 {
		return errors.New("lock: source_commit is required")
	}
	if l.BuiltAt.IsZero() {
		return errors.New("lock: built_at is required")
	}

	// All three artifacts. nova-admin uniquely mounts the federation PKI, so a
	// release that omits it leaves issuance authority unsigned.
	for _, name := range ArtifactNames {
		a, ok := l.Artifacts[name]
		if !ok {
			return fmt.Errorf("lock: no descriptor for %s; nova-admin is the image that mounts "+
				"nova-fedpki, so all three are released together", name)
		}
		if a.Repository != in.Repositories[name] {
			return fmt.Errorf("lock: %s was published to %q but the intent declared %q",
				name, a.Repository, in.Repositories[name])
		}
		if _, err := reference.ParseNormalizedNamed(a.Repository); err != nil {
			return fmt.Errorf("lock: %s repository: %w", name, err)
		}
		if err := validateDescriptor(name, a.Descriptor); err != nil {
			return err
		}
		for i, child := range a.Manifests {
			if err := validateDescriptor(fmt.Sprintf("%s.manifests[%d]", name, i), child); err != nil {
				return err
			}
		}
		// The declared platform set and the published one must be the same set.
		// Nova is single-platform by DECISION or multi-platform by decision;
		// what it must not be is either one by accident, because donors are
		// volunteer hardware and the matrix is what they read.
		if err := ValidateArtifactPlatforms(in.Platforms, name, a); err != nil {
			return err
		}
	}
	for name := range l.Artifacts {
		if !slices.Contains(ArtifactNames, name) {
			return fmt.Errorf("lock: unknown artifact %q", name)
		}
	}

	// Sidecars carry forward unchanged; a lock that renamed one would describe
	// a topology nobody reviewed.
	for name, ref := range in.Sidecars {
		if l.Sidecars[name] != ref {
			return fmt.Errorf("lock: sidecar %s is %q but the intent declared %q",
				name, l.Sidecars[name], ref)
		}
	}

	// Every declared claim must be proven, and every proof must be evidence.
	declared := map[string]DeclaredClaim{}
	for _, c := range in.Claims {
		declared[c.ID] = c
	}
	proven := map[string]bool{}
	for _, c := range l.ProvenClaims {
		d, ok := declared[c.ID]
		if !ok {
			return fmt.Errorf("lock: claim %q is proven but was never declared in the intent; "+
				"a claim nobody reviewed is not part of the release", c.ID)
		}
		if c.Gate != d.ProvenByGate {
			return fmt.Errorf("lock: claim %q was proven by gate %q but the intent named %q",
				c.ID, c.Gate, d.ProvenByGate)
		}
		if len(c.Evidence) == 0 {
			return fmt.Errorf("lock: claim %q carries no evidence; that is the difference "+
				"between a declared claim and a proven one", c.ID)
		}
		for _, e := range c.Evidence {
			if err := validateSHA256(e.StatementDigest); err != nil {
				return fmt.Errorf("lock: claim %q evidence: %w (evidence is bound by content "+
					"digest, never by a run id — logs expire)", c.ID, err)
			}
			if e.Outcome != "passed" {
				return fmt.Errorf("lock: claim %q is bound to evidence with outcome %q; a claim "+
					"is required by definition, so a skipped gate cannot prove one", c.ID, e.Outcome)
			}
			if e.RunnerClass == "" {
				return fmt.Errorf("lock: claim %q evidence names no runner class; a static gate "+
					"must not be able to satisfy a claim that needs an executed one", c.ID)
			}
		}
		proven[c.ID] = true
	}
	for id := range declared {
		if !proven[id] {
			return fmt.Errorf("lock: claim %q was declared but never proven", id)
		}
	}

	// The payload map is the only authenticated path to a deployable byte.
	for _, m := range requiredMembers {
		h, ok := l.Payload[m]
		if !ok {
			return fmt.Errorf("lock: payload does not cover %s; an uncovered member can be "+
				"substituted between signing and use", m)
		}
		if err := validateSHA256(h); err != nil {
			return fmt.Errorf("lock: payload[%s]: %w", m, err)
		}
	}
	for path, h := range l.Payload {
		if slices.Contains(excludedFromPayload, path) {
			return fmt.Errorf("lock: payload covers %s, which would make the authentication "+
				"graph cyclic — a lock cannot hash itself or the envelope carrying the "+
				"signature over it", path)
		}
		if err := validateSHA256(h); err != nil {
			return fmt.Errorf("lock: payload[%s]: %w", path, err)
		}
	}

	if err := validateSHA256(l.DonorLockDigest); err != nil {
		return fmt.Errorf("lock: donor_lock_digest: %w", err)
	}
	return nil
}

// validateDescriptor rejects the shapes that would leave a promotion read-back
// unable to assert anything.
func validateDescriptor(name string, d ocispec.Descriptor) error {
	if d.Digest == "" {
		return fmt.Errorf("lock: %s has no digest", name)
	}
	if err := d.Digest.Validate(); err != nil {
		return fmt.Errorf("lock: %s digest %q: %w", name, d.Digest, err)
	}
	if d.Digest.Algorithm() != digest.SHA256 {
		return fmt.Errorf("lock: %s digest algorithm is %s, want sha256", name, d.Digest.Algorithm())
	}
	if d.MediaType == "" {
		return fmt.Errorf("lock: %s has a bare digest and no mediaType; a digest alone cannot "+
			"say whether it names an index or a single manifest, and promotion treats the "+
			"two differently", name)
	}
	switch d.MediaType {
	case ocispec.MediaTypeImageIndex, ocispec.MediaTypeImageManifest,
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json":
	default:
		return fmt.Errorf("lock: %s mediaType %q is not an image manifest or index",
			name, d.MediaType)
	}
	if d.Size <= 0 {
		return fmt.Errorf("lock: %s has size %d; a descriptor without a size cannot be "+
			"verified against the registry", name, d.Size)
	}
	return nil
}

func validateSHA256(s string) error {
	if !strings.HasPrefix(s, "sha256:") {
		return fmt.Errorf("%q is not a sha256: digest", s)
	}
	hexPart := strings.TrimPrefix(s, "sha256:")
	if len(hexPart) != 64 {
		return fmt.Errorf("%q is not 64 hex characters", s)
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return fmt.Errorf("%q is not hex: %w", s, err)
	}
	return nil
}

// LockDigest is the sha256 of the EXACT lock bytes, for the same reason
// IntentDigest is: every consumer re-hashes the bytes it was handed and
// compares against a digest that arrived through the authenticated chain.
//
// `--lock <file>` means nothing across a process boundary — a path is not an
// identity. The digest is.
func LockDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// DonorProjection is the subset of a lock a donor needs: the three images of
// the canonical donor topology. It is a DETERMINISTIC projection, not a
// separately signed document, and it is identical for every node except the
// per-node identity fields the bundle already carries.
type DonorProjection struct {
	Schema      int    `json:"schema"`
	Version     string `json:"version"`
	NodeImage   string `json:"node_image"`
	NebulaImage string `json:"nebula_image"`
	KuboImage   string `json:"kubo_image"`
}

// DonorProjectionOf builds the projection. Marshalling is deterministic because
// the struct has fixed field order and no maps.
func DonorProjectionOf(l Lock) (DonorProjection, error) {
	node, ok := l.Artifacts["nova-node"]
	if !ok {
		return DonorProjection{}, errors.New("lock: no nova-node artifact to project")
	}
	neb, ok := l.Sidecars["nebula"]
	if !ok {
		return DonorProjection{}, errors.New("lock: no nebula sidecar to project")
	}
	kubo, ok := l.Sidecars["kubo"]
	if !ok {
		return DonorProjection{}, errors.New("lock: no kubo sidecar to project")
	}
	return DonorProjection{
		Schema: LockSchema, Version: l.Version,
		NodeImage: node.Ref(), NebulaImage: neb, KuboImage: kubo,
	}, nil
}

// DonorProjectionBytes renders the projection canonically, so its digest is a
// function of the lock alone.
func DonorProjectionBytes(p DonorProjection) ([]byte, error) {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
