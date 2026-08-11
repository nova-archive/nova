// Package release is the release lifecycle's data model: the checked-in
// INTENT, the signed LOCK, the compatibility SCENARIOS bound to executed
// evidence, the support WINDOW, and the compiled-in CATALOG (P2-M7.3).
//
// # Why intent and lock are two documents
//
// A release decision has to be reviewable before a build exists, and a release
// fact cannot exist until one does. Stamping a version and OCI labels changes
// the image config digest, and therefore the manifest digest, so a document
// that commits to digests and is then built from cannot reproduce them — the
// digest depends on the document that names it.
//
// So: INTENT declares what is being asked for and which gate must prove each
// claim. It carries no digests of Nova's own artifacts and no evidence. LOCK is
// generated after the candidate is built once, records the exact descriptors
// that were produced, binds each claim to signed evidence, and is signed.
// Declared and proven are distinct Go types, so a claim cannot acquire evidence
// by being assigned to the wrong field.
package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/distribution/reference"
	"golang.org/x/mod/semver"
)

// IntentSchema is the intent document's own format version. It is not the Nova
// release version; it changes when this struct changes shape.
const IntentSchema = 1

// ArtifactNames are the three released images, in a fixed order so every
// derived document (lock, bundle, compose env) lists them identically.
//
// nova-admin is included because it uniquely mounts the federation PKI volume:
// publishing a third artifact while production builds that one locally would
// leave issuance authority running unsigned, unpinned bytes.
var ArtifactNames = []string{"nova-coordinator", "nova-node", "nova-admin"}

// Predecessor identifies what a release is an upgrade FROM.
//
// It accepts a COMMIT because Nova's first contract-bearing release upgrades
// from a baseline that has no product version: the remote tag namespace holds
// only two lightweight pre-contract refs, and the deployment in the field is a
// local build of a commit. A scenario that can only name versions cannot
// describe the one transition that actually has to work.
type Predecessor struct {
	// Kind is "commit" or "version". Exactly one of Commit/Version is set.
	Kind    string `json:"kind"`
	Commit  string `json:"commit,omitempty"`
	Version string `json:"version,omitempty"`

	// Schema is the goose schema version that predecessor runs.
	Schema int64 `json:"schema"`

	// SupportEpoch is when this artifact's support window starts. The baseline
	// has no release date, so its epoch is DECLARED here rather than inferred
	// from a tag that does not exist.
	SupportEpoch time.Time `json:"support_epoch"`
}

// ID renders the predecessor as the stable string scenarios and the census use.
func (p Predecessor) ID() string {
	if p.Kind == "commit" {
		return "commit:" + p.Commit
	}
	return p.Version
}

// Validate checks that exactly one identity is present and well-formed.
func (p Predecessor) Validate() error {
	switch p.Kind {
	case "commit":
		if p.Version != "" {
			return errors.New("predecessor: kind=commit must not also carry a version")
		}
		if len(p.Commit) < 7 || strings.TrimLeft(p.Commit, "0123456789abcdef") != "" {
			return fmt.Errorf("predecessor: %q is not a hex commit of at least 7 characters", p.Commit)
		}
	case "version":
		if p.Commit != "" {
			return errors.New("predecessor: kind=version must not also carry a commit")
		}
		if err := validateProductVersion(p.Version); err != nil {
			return fmt.Errorf("predecessor: %w", err)
		}
	default:
		return fmt.Errorf("predecessor: kind %q must be \"commit\" or \"version\"", p.Kind)
	}
	if p.Schema < 0 {
		return fmt.Errorf("predecessor: schema %d is negative", p.Schema)
	}
	if p.SupportEpoch.IsZero() {
		return errors.New("predecessor: support_epoch must be declared; the baseline has no " +
			"release date to infer one from")
	}
	return nil
}

// DeclaredClaim is a compatibility claim as REQUESTED, before any build exists.
//
// It deliberately has no evidence field. A reviewer reading the intent is
// approving what will be attempted and which gate must demonstrate it; evidence
// exists only on the lock, as ProvenClaim, so the two cannot be confused by
// assignment.
type DeclaredClaim struct {
	// ID is stable across releases: scenarios and the coverage table key on it.
	ID string `json:"id"`
	// Statement is what an operator would read.
	Statement string `json:"statement"`
	// RequiresCapabilities are the capabilities the claim is conditional on.
	RequiresCapabilities []string `json:"requires_capabilities,omitempty"`
	// ProvenByGate names the gate that must demonstrate it. A claim whose gate
	// does not cover it is rejected — see scenario.go.
	ProvenByGate string `json:"proven_by_gate"`
}

// CapabilityProfiles is the core-plus-per-role capability model the census and
// the deprecation channel consult offline (D-M7.3-21a).
type CapabilityProfiles struct {
	// Core is required at registration. Everything else is route-gated.
	Core []string `json:"core"`
	// Roles maps a role name to the capabilities it needs. Missing one excludes
	// a donor from that role and from nothing else.
	Roles map[string][]string `json:"roles"`
}

// Intent is the checked-in, reviewed-before-build release decision.
type Intent struct {
	Schema  int    `json:"schema"`
	Version string `json:"version"`

	// SupportEpoch and SupportUntil drive the executable support window.
	SupportEpoch time.Time `json:"support_epoch"`
	SupportUntil time.Time `json:"support_until"`

	// SupportedPredecessors are the artifacts this release commits to
	// interoperating with, newest first.
	SupportedPredecessors []Predecessor `json:"supported_predecessors"`

	TargetSchema int      `json:"target_schema"`
	Platforms    []string `json:"platforms"`

	// Repositories maps each artifact name to the repository it publishes to.
	// NO DIGESTS: they do not exist yet, and a digest here could not survive
	// the build that produces it.
	Repositories map[string]string `json:"repositories"`

	// Sidecars are third-party images that are ALREADY published, so an exact
	// digest-pinned reference is both possible and required.
	Sidecars map[string]string `json:"sidecars"`

	FedProtocols    []string `json:"fed_protocols"`
	EnvelopeFormats []int    `json:"envelope_formats"`

	Profiles CapabilityProfiles `json:"capability_profiles"`
	Claims   []DeclaredClaim    `json:"claims"`
}

// validateProductVersion enforces strict SemVer with no build metadata.
//
// `+` is not a legal Docker tag character, and SemVer ignores build metadata
// for precedence — so a version carrying it would be un-taggable AND
// indistinguishable from its base for ordering.
func validateProductVersion(v string) error {
	if !semver.IsValid(v) {
		return fmt.Errorf("version %q is not valid SemVer with a leading v", v)
	}
	if semver.Build(v) != "" {
		return fmt.Errorf("version %q carries build metadata; `+` is not a legal Docker tag "+
			"character and SemVer ignores build metadata for precedence", v)
	}
	if semver.Canonical(v) != strings.TrimSuffix(v, semver.Prerelease(v)) &&
		semver.Canonical(v)+semver.Prerelease(v) != v {
		return fmt.Errorf("version %q is not canonical (want vMAJOR.MINOR.PATCH[-prerelease])", v)
	}
	return nil
}

// ParseIntent decodes and validates intent bytes. Unknown and trailing JSON are
// rejected: this is a release-metadata loader, not the wire path, and silently
// ignoring a misspelled field is how a reviewed decision stops matching what is
// executed.
func ParseIntent(b []byte) (Intent, error) {
	var in Intent
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return Intent{}, fmt.Errorf("intent: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return Intent{}, errors.New("intent: trailing content after the JSON document")
	}
	if err := in.Validate(); err != nil {
		return Intent{}, err
	}
	return in, nil
}

// Validate enforces every rule an intent must satisfy to be reviewable.
func (in Intent) Validate() error {
	if in.Schema != IntentSchema {
		return fmt.Errorf("intent: schema %d, want %d", in.Schema, IntentSchema)
	}
	if err := validateProductVersion(in.Version); err != nil {
		return fmt.Errorf("intent: %w", err)
	}
	if in.SupportEpoch.IsZero() {
		return errors.New("intent: support_epoch is required")
	}
	if !in.SupportUntil.IsZero() && !in.SupportUntil.After(in.SupportEpoch) {
		return errors.New("intent: support_until must be after support_epoch")
	}
	if len(in.Platforms) == 0 {
		return errors.New("intent: at least one platform must be declared; the artifact set " +
			"was single-platform by accident before this was explicit")
	}
	if in.TargetSchema <= 0 {
		return errors.New("intent: target_schema is required")
	}

	if len(in.SupportedPredecessors) == 0 {
		return errors.New("intent: at least one supported predecessor is required; a release " +
			"that names none makes no compatibility commitment at all")
	}
	for i, p := range in.SupportedPredecessors {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("intent: supported_predecessors[%d]: %w", i, err)
		}
		if p.Schema > int64(in.TargetSchema) {
			return fmt.Errorf("intent: predecessor %s runs schema %d, ahead of target_schema %d; "+
				"the schema is forward-only", p.ID(), p.Schema, in.TargetSchema)
		}
	}

	// Repositories: named, parseable, and WITHOUT a digest or tag. Pinning
	// Nova's own artifacts here is the circular contract this design exists to
	// avoid.
	for _, name := range ArtifactNames {
		repo, ok := in.Repositories[name]
		if !ok || repo == "" {
			return fmt.Errorf("intent: no repository declared for %s", name)
		}
		if strings.Contains(repo, "@") {
			return fmt.Errorf("intent: repository for %s carries a digest (%q); intent is "+
				"reviewed BEFORE the build that would produce it, and stamping the version "+
				"changes the digest, so a digest here can never be reproduced", name, repo)
		}
		if _, err := reference.ParseNormalizedNamed(repo); err != nil {
			return fmt.Errorf("intent: repository for %s is not a valid image reference: %w", name, err)
		}
		if ref, _ := reference.ParseNormalizedNamed(repo); reference.IsNameOnly(ref) == false {
			return fmt.Errorf("intent: repository for %s carries a tag (%q); the release "+
				"workflow chooses tags", name, repo)
		}
	}
	for name := range in.Repositories {
		if !slices.Contains(ArtifactNames, name) {
			return fmt.Errorf("intent: unknown artifact %q", name)
		}
	}

	// Sidecars are already published, so an exact digest is required.
	for name, ref := range in.Sidecars {
		if !strings.Contains(ref, "@sha256:") {
			return fmt.Errorf("intent: sidecar %s (%q) is not digest-pinned; it is already "+
				"published, so there is no reason to accept a mutable reference", name, ref)
		}
		if _, err := reference.ParseNormalizedNamed(ref); err != nil {
			return fmt.Errorf("intent: sidecar %s: %w", name, err)
		}
	}

	if len(in.FedProtocols) == 0 {
		return errors.New("intent: fed_protocols is the interop contract and cannot be empty")
	}
	if len(in.EnvelopeFormats) == 0 {
		return errors.New("intent: envelope_formats cannot be empty")
	}
	if len(in.Profiles.Core) == 0 {
		return errors.New("intent: capability_profiles.core cannot be empty")
	}

	seen := map[string]bool{}
	for _, c := range in.Claims {
		if c.ID == "" || c.Statement == "" {
			return fmt.Errorf("intent: claim %+v is missing an id or statement", c)
		}
		if seen[c.ID] {
			return fmt.Errorf("intent: duplicate claim id %q", c.ID)
		}
		seen[c.ID] = true
		if c.ProvenByGate == "" {
			return fmt.Errorf("intent: claim %q names no gate; a claim nothing must prove is "+
				"not a claim", c.ID)
		}
	}
	return nil
}

// FilenameFor is the checked-in path an intent must live at.
func FilenameFor(version string) string { return version + ".json" }

// ValidateFilename rejects an intent whose filename disagrees with its version.
// Two files claiming one version, or one file claiming another's, is how the
// wrong decision gets built.
func ValidateFilename(path string, in Intent) error {
	if got, want := filepath.Base(path), FilenameFor(in.Version); got != want {
		return fmt.Errorf("intent: file %s declares version %s and should be named %s", got, in.Version, want)
	}
	return nil
}

// IntentDigest is the SHA-256 of the EXACT checked-in bytes.
//
// Not of a re-marshalled struct: re-marshalling depends on field order, on
// whether omitempty elided something, and on the Go version's encoder — none of
// which the reviewer looked at. The bytes on disk are what was approved.
func IntentDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
