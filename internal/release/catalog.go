package release

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nova-archive/nova/internal/release/catalog"
	"golang.org/x/mod/semver"
)

// The embedded release catalog (P2-M7.3, D-M7.3-2c).
//
// T1.22 forbids any binary from fetching release metadata, so what a build
// CLAIMS about itself has to be compiled in. The catalog is generated from the
// checked-in intent and is what makes `novactl upgrade status` work with no
// database, no network and no files — and what the target admin compares an
// operator-supplied lock against as a consistency check.
//
// It is deliberately small: identity, the support window, the capability
// profiles and the repositories. Anything that only exists after a build (
// digests, evidence) belongs on the lock, which arrives as a verified file.

// Catalog, Predecessor and CapabilityProfiles live in the leaf package
// internal/release/catalog and are re-exported here by ALIAS, so callers see
// one namespace while cmd/node links only the dependency-free half.
type (
	Catalog            = catalog.Catalog
	Predecessor        = catalog.Predecessor
	CapabilityProfiles = catalog.CapabilityProfiles
)

// Compiled returns the catalog linked into this binary.
func Compiled() Catalog { return catalog.Compiled() }

// CurrentIntentPath returns the checked-in intent the catalog is generated
// from: the greatest version by SemVer precedence in dir.
//
// Highest-by-precedence rather than newest-by-mtime, because mtime is a
// property of a checkout and precedence is a property of the release.
func CurrentIntentPath(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	best := ""
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		v := strings.TrimSuffix(e.Name(), ".json")
		if !semver.IsValid(v) {
			return "", fmt.Errorf("catalog: %s is not named for a SemVer version", e.Name())
		}
		if best == "" || semver.Compare(v, best) > 0 {
			best = v
		}
	}
	if best == "" {
		return "", fmt.Errorf("catalog: no intents under %s", dir)
	}
	return filepath.Join(dir, best+".json"), nil
}

// CatalogFrom builds the catalog value for an intent and its exact bytes.
func CatalogFrom(in Intent, intentBytes []byte) Catalog {
	return Catalog{
		Version:               in.Version,
		IntentDigest:          IntentDigest(intentBytes),
		TargetSchema:          in.TargetSchema,
		Platforms:             in.Platforms,
		SupportEpoch:          in.SupportEpoch,
		SupportUntil:          in.SupportUntil,
		SupportedPredecessors: in.SupportedPredecessors,
		Repositories:          in.Repositories,
		Profiles:              in.Profiles,
	}
}
