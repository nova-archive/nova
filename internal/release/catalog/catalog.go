// Package catalog is the compiled-in release identity (P2-M7.3, D-M7.3-2c).
//
// It is a SEPARATE, DEPENDENCY-FREE package on purpose. T1.22 means every
// binary — including the donor — has to carry what it claims about itself,
// and cmd/node's dependency boundary is deny-by-default over all non-stdlib
// imports. Keeping the catalog with intent and lock parsing would have dragged
// distribution/reference, image-spec, go-digest and x/mod/semver into the donor
// binary to print one version string.
//
// So: this package holds the data and the accessor and imports nothing outside
// the standard library. internal/release owns generation and validation and
// re-exports these types by alias, so callers see one namespace.
package catalog

import (
	"fmt"
	"time"
)

// Predecessor identifies what a release is an upgrade FROM.
//
// It accepts a COMMIT because Nova's first contract-bearing release upgrades
// from a baseline with no product version: the remote tag namespace holds only
// two lightweight pre-contract refs, and the deployment in the field is a local
// build of a commit. A model that can only name versions cannot describe the
// one transition that actually has to work.
type Predecessor struct {
	// Kind is "commit" or "version"; exactly one of Commit/Version is set.
	Kind    string `json:"kind"`
	Commit  string `json:"commit,omitempty"`
	Version string `json:"version,omitempty"`

	// Schema is the goose schema version that predecessor runs.
	Schema int64 `json:"schema"`

	// SupportEpoch is when this artifact's support window starts. The baseline
	// has no release date, so its epoch is DECLARED rather than inferred from a
	// tag that does not exist.
	SupportEpoch time.Time `json:"support_epoch"`
}

// ID renders the predecessor as the stable string scenarios and the census use.
func (p Predecessor) ID() string {
	if p.Kind == "commit" {
		return "commit:" + p.Commit
	}
	return p.Version
}

// CapabilityProfiles is the core-plus-per-role capability model the census and
// the deprecation channel consult offline (D-M7.3-21a).
type CapabilityProfiles struct {
	// Core is required at registration. Everything else is route-gated.
	Core []string `json:"core"`
	// Roles maps a role to the capabilities it needs. Missing one excludes a
	// donor from that role and from nothing else.
	Roles map[string][]string `json:"roles"`
}

// Catalog is what this build claims about the release it belongs to.
//
// Deliberately small: identity, the support window, the capability profiles and
// the repositories. Anything that only exists after a build — digests,
// evidence — belongs on the lock, which arrives as an operator-supplied
// verified file.
type Catalog struct {
	// Version is the product version this build is part of.
	Version string
	// IntentDigest is the sha256 of the exact intent bytes it was generated
	// from: the join between a running binary and a reviewed decision.
	IntentDigest string
	// TargetSchema is the goose schema version this release expects.
	TargetSchema int
	// Platforms is the declared platform policy.
	Platforms []string

	// SupportEpoch and SupportUntil drive the offline support window.
	SupportEpoch time.Time
	SupportUntil time.Time
	// SupportedPredecessors are the artifacts this release interoperates with.
	SupportedPredecessors []Predecessor

	// Repositories is where each artifact publishes. No digests: those are
	// lock-only, and a self-referential digest constant in the source tree is
	// exactly the circularity this design exists to break.
	Repositories map[string]string

	// Profiles is the capability model, consulted without a network.
	Profiles CapabilityProfiles
}

// Compiled returns the catalog linked into this binary. A binary built without
// generating one reports an empty Version, which every consumer treats as
// "unknown" rather than as a claim.
func Compiled() Catalog { return compiled }

// Stamped reports whether this binary carries a real catalog.
func (c Catalog) Stamped() bool { return c.Version != "" && c.IntentDigest != "" }

// String is the one-line form --version prints. The intent digest is included
// because it, not the version string, is what a target admin cross-checks.
func (c Catalog) String() string {
	if !c.Stamped() {
		return "release catalog: not generated"
	}
	return fmt.Sprintf("release %s (intent %s, schema %d)", c.Version, c.IntentDigest, c.TargetSchema)
}
