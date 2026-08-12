package coordinator

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/release"
)

// The deprecation channel (P2-M7.3, D-M7.3-21c).
//
// FEDERATION_PROTOCOL.md specified `config_updates.deprecation_message` as how
// the coordinator warns an outdated donor. Nothing carried it, and nothing
// consumed it, so the specified mechanism did not exist.
//
// # Nonconformance is ADVISORY
//
// A donor outside the mandatory profile stays connected and keeps its replicas.
// Placement excludes it from work requiring the missing capability — that
// happens in the capability predicates, not here. This message tells the
// VOLUNTEER, and the census tells the operator. The operator, not the
// heartbeat, invokes drain: `draining_at` is operator-controlled and no
// automatic lifecycle transition may set it.
//
// # Why the wording matters
//
// "Unsupported" means untested, not dead. A message that reads like an
// ultimatum invites a volunteer to shut a donor down, which is the outcome the
// whole fleet policy exists to avoid.

// deprecationFor builds the warning for one donor, or "" when there is nothing
// to say. It reads the compiled-in catalog and a clock; T1.22 leaves it no
// other option.
func deprecationFor(c *wire.RuntimeContract, now time.Time) string {
	return deprecationForWith(release.Compiled(), c, now)
}

// deprecationForWith is the testable form: the catalog is a parameter so a test
// can exercise a fleet the shipped catalog does not describe.
func deprecationForWith(cat release.Catalog, c *wire.RuntimeContract, now time.Time) string {
	if !cat.Stamped() {
		return "" // an unstamped build makes no support claims
	}

	var parts []string

	// 1. Core-profile compliance. A donor missing a core capability is not sent
	//    work that needs it, and it should know why.
	if c != nil && len(c.Capabilities) > 0 {
		var missing []string
		for _, want := range cat.Profiles.Core {
			if !slices.Contains(c.Capabilities, want) {
				missing = append(missing, want)
			}
		}
		if len(missing) > 0 {
			parts = append(parts, fmt.Sprintf(
				"this donor does not advertise %s, which release %s lists in its core profile. "+
					"It stays registered and keeps everything it holds; it is simply not sent "+
					"work that needs those capabilities.",
				strings.Join(missing, ", "), cat.Version))
		}
	}

	// 2. The support window, evaluated offline.
	if c != nil && c.ClientVersion != "" && c.ClientVersion != cat.Version {
		s := release.SupportedBy(cat, c.ClientVersion, now)
		switch {
		case !s.Known:
			// Unknown is NOT incompatible: the interop contract is protocol plus
			// capabilities, and a version string was never it. Say nothing.
		case !s.Supported:
			parts = append(parts, fmt.Sprintf(
				"this donor reports %s, which release %s no longer tests against (the support "+
					"window lapsed on %s). Nothing is evicted and your replicas are kept — "+
					"untested is not unsupported-and-removed — but an update when convenient "+
					"is worth doing.",
				c.ClientVersion, cat.Version, s.AgeBranchDeadline.Format(time.DateOnly)))
		case s.ByAge && !s.ByPredecessor:
			parts = append(parts, fmt.Sprintf(
				"this donor reports %s. It is supported by age until %s; after that it is no "+
					"longer one of release %s's tested predecessors.",
				c.ClientVersion, s.AgeBranchDeadline.Format(time.DateOnly), cat.Version))
		}
	}

	return strings.Join(parts, " ")
}
