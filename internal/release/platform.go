package release

import (
	"fmt"
	"sort"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// The platforms a release actually publishes (P2-M7.3, amendment).
//
// # What was missing
//
// A descriptor was validated for digest, mediaType and size, and said nothing
// about what it RUNS ON. `linux/amd64` was a string in the intent and a
// `platforms:` line in the workflow, and nothing compared them — so the release
// would have been single-platform because one YAML line said so, not because
// anybody decided it.
//
// That matters more for Nova than for most projects. Donors are volunteer
// hardware. A donor on arm64 that pulls a manifest with no matching platform
// gets `no matching manifest for linux/arm64`, and the operator has no way to
// tell "we deliberately support amd64 only" from "the release forgot".
//
// So the intent DECLARES the platform set, and the lock's descriptors have to
// realize exactly that set — no more, no less. Publishing arm64 without
// declaring it is as much a defect as declaring it and not publishing: the
// declared set is what the compatibility matrix prints and what a donor reads.
//
// # Attestation manifests are not platforms
//
// A BuildKit index carries provenance and SBOM manifests alongside the image,
// distinguished by `platform.architecture == "unknown"`. They are attestations
// about the image, not another architecture it runs on, and counting them would
// make every index fail the comparison.

// unknownArchitecture is how BuildKit marks a manifest that is an attestation
// rather than an image.
const unknownArchitecture = "unknown"

// PlatformString renders a platform the way the intent writes it.
func PlatformString(p *ocispec.Platform) string {
	if p == nil {
		return ""
	}
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// IsAttestationManifest reports whether a child descriptor is an attestation
// rather than an image for some architecture.
func IsAttestationManifest(d ocispec.Descriptor) bool {
	return d.Platform != nil && d.Platform.Architecture == unknownArchitecture
}

// ArtifactPlatforms returns the platform set a locked artifact actually offers.
//
// An index is asked its children; a single manifest must carry its own
// platform. A single manifest with no platform is refused rather than assumed
// to be the one the intent declares — assuming is how a descriptor comes to say
// something nobody verified.
func ArtifactPlatforms(name string, a LockedArtifact) ([]string, error) {
	switch a.Descriptor.MediaType {
	case ocispec.MediaTypeImageIndex, "application/vnd.docker.distribution.manifest.list.v2+json":
		if len(a.Manifests) == 0 {
			return nil, fmt.Errorf("lock: %s is an index and lists no child descriptors; an "+
				"index is a list of manifests, and one recorded without them cannot say which "+
				"platforms it serves", name)
		}
		var out []string
		for i, child := range a.Manifests {
			if IsAttestationManifest(child) {
				continue
			}
			if child.Platform == nil {
				return nil, fmt.Errorf("lock: %s.manifests[%d] carries no platform; a child of "+
					"an index is selected BY platform, so one without a platform can never be "+
					"selected", name, i)
			}
			out = append(out, PlatformString(child.Platform))
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("lock: %s is an index whose only children are attestations; "+
				"there is no image in it to run", name)
		}
		return dedupeSorted(out), nil
	default:
		if a.Descriptor.Platform == nil {
			return nil, fmt.Errorf("lock: %s is a single manifest with no platform. A donor "+
				"pulling it on the wrong architecture fails at run time with an exec-format "+
				"error rather than at pull time with a clear one", name)
		}
		if len(a.Manifests) > 0 {
			return nil, fmt.Errorf("lock: %s is a single manifest (%s) but records %d child "+
				"descriptor(s); only an index has children", name, a.Descriptor.MediaType,
				len(a.Manifests))
		}
		return []string{PlatformString(a.Descriptor.Platform)}, nil
	}
}

// ValidateArtifactPlatforms refuses an artifact whose platform set is not
// exactly the declared one.
//
// Exactly, in both directions. A missing platform is a donor that cannot run
// the release; an extra one is a platform the compatibility matrix does not
// mention, no gate exercised, and nobody committed to supporting.
func ValidateArtifactPlatforms(declared []string, name string, a LockedArtifact) error {
	got, err := ArtifactPlatforms(name, a)
	if err != nil {
		return err
	}
	want := dedupeSorted(declared)
	if len(want) == 0 {
		return fmt.Errorf("lock: %s cannot be checked: the intent declares no platforms", name)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		return fmt.Errorf("lock: %s publishes %s but the intent declares %s. The two must be "+
			"exactly equal: a missing platform is a donor that cannot run the release, and an "+
			"extra one is a platform the compatibility matrix never mentions and no gate ever "+
			"exercised", name, joinOrNone(got), joinOrNone(want))
	}
	return nil
}

func dedupeSorted(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func joinOrNone(in []string) string {
	if len(in) == 0 {
		return "nothing"
	}
	return strings.Join(in, ", ")
}
