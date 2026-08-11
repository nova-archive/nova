package release

import (
	"fmt"
	"slices"
	"time"
)

// The executable support window (P2-M7.3, D-M7.3-21a).
//
// T1.22 forbids auto-update, so each volunteer decides when their machine
// changes. Long-lived backward compatibility is therefore not a courtesy but a
// requirement, and the window has to be something the coordinator can evaluate
// OFFLINE, from the compiled-in catalog, with no network.
//
// A donor artifact R is supported by target T while EITHER branch holds:
//
//   - PREDECESSOR — R is one of T's two immediate donor predecessors (N-1 or
//     N-2); or
//   - AGE — fewer than six months have elapsed since R's declared support_epoch.
//
// Support expires only when BOTH are false.
//
// # What that actually means
//
// For a FIXED target T, an N-1 or N-2 artifact has NO EFFECTIVE EXPIRY.
// `support_epoch + six months` is the AGE-BRANCH DEADLINE, not the artifact's
// expiry date, and it only becomes decisive once the predecessor branch stops
// holding — that is, once two further releases have shipped. Documentation says
// it that way deliberately: describing the six-month date as "when support
// ends" would be wrong for exactly the artifacts most likely still deployed.

// AgeBranchWindow is how long the age branch holds after an artifact's epoch.
const AgeBranchWindow = 6 * 30 * 24 * time.Hour // six months

// PredecessorDepth is how many immediate predecessors the predecessor branch
// covers: N-1 and N-2.
const PredecessorDepth = 2

// SupportBranch names why an artifact is (or is not) supported. Both branches
// are reported, because "supported, but only by age" and "supported as N-1" are
// operationally different — the first has a date, the second does not.
type SupportStatus struct {
	// Supported is the disjunction: either branch is enough.
	Supported bool
	// ByPredecessor is true when the artifact is one of the target's two
	// immediate predecessors. While this holds there is no effective expiry.
	ByPredecessor bool
	// ByAge is true when fewer than six months have elapsed since the
	// artifact's declared epoch.
	ByAge bool
	// AgeBranchDeadline is when the AGE branch stops holding. It is not the
	// artifact's expiry unless ByPredecessor is already false.
	AgeBranchDeadline time.Time
	// Known is false when the target's catalog says nothing about this
	// artifact. Unknown is NOT unsupported (D-M7.3-13).
	Known bool
}

// Explain renders the status the way the census and deprecation_message do.
func (s SupportStatus) Explain() string {
	switch {
	case !s.Known:
		return "unknown to this release's catalog; judged by protocol and capabilities, not by version"
	case s.ByPredecessor && s.ByAge:
		return "supported (an immediate predecessor, and within the age window)"
	case s.ByPredecessor:
		return fmt.Sprintf("supported as an immediate predecessor; the age branch lapsed on %s, "+
			"which does not end support while this branch holds",
			s.AgeBranchDeadline.Format(time.DateOnly))
	case s.ByAge:
		return fmt.Sprintf("supported by age until %s; no longer an immediate predecessor",
			s.AgeBranchDeadline.Format(time.DateOnly))
	default:
		return fmt.Sprintf("unsupported since %s and no longer an immediate predecessor; "+
			"untested, not dead — existing replicas are kept and nothing is evicted",
			s.AgeBranchDeadline.Format(time.DateOnly))
	}
}

// SupportedBy evaluates the window for a donor artifact against a target's
// catalog. It needs no network and no database: everything it reads is
// compiled in.
//
// artifactID is a Predecessor.ID() — a product version, or "commit:<sha>" for
// the precontract baseline, which has no product version at all.
func SupportedBy(target Catalog, artifactID string, now time.Time) SupportStatus {
	idx := slices.IndexFunc(target.SupportedPredecessors, func(p Predecessor) bool {
		return p.ID() == artifactID
	})
	if idx < 0 {
		// Not in the catalog. Unknown is not incompatible: the interop contract
		// is protocol plus capabilities, and a version string was never it.
		return SupportStatus{Known: false}
	}
	p := target.SupportedPredecessors[idx]

	s := SupportStatus{
		Known:             true,
		ByPredecessor:     idx < PredecessorDepth,
		AgeBranchDeadline: p.SupportEpoch.Add(AgeBranchWindow),
	}
	s.ByAge = now.Before(s.AgeBranchDeadline)
	s.Supported = s.ByPredecessor || s.ByAge
	return s
}

// AgeBranchDeadlineFor is the date the age branch lapses for an artifact.
//
// Named for what it is. Calling it "expiry" would be wrong for an N-1 artifact,
// whose support does not end on that date and has no end date at all while it
// stays an immediate predecessor.
func AgeBranchDeadlineFor(p Predecessor) time.Time {
	return p.SupportEpoch.Add(AgeBranchWindow)
}
