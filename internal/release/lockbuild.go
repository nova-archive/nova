package release

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Building a lock (P2-M7.3, D-M7.3-2e step 5).
//
// # The gap this closes
//
// The package could PARSE a lock and validate one, and nothing could produce
// one. Every lock that existed was written by a test fixture. The release
// workflow's "finalize and sign the lock" step therefore had nothing to call,
// which is why it stopped there.
//
// # Everything it needs arrives from outside
//
// Digests come from the registry read-back, evidence from gates that have
// already run, the payload map from the assembled bundle. This function
// invents nothing: it assembles, validates, and refuses. A builder that could
// fill in a missing digest would be a builder that can produce a lock for a
// release nobody built.

// LockInputs is everything a lock is assembled from.
type LockInputs struct {
	// Intent and IntentBytes are the reviewed decision and its exact bytes.
	Intent      Intent
	IntentBytes []byte

	// SourceCommit is the candidate commit the artifacts were built from.
	SourceCommit string
	// BuiltAt is when. Supplied rather than taken from the clock so a rerun
	// that recovers a partial release produces the same document.
	BuiltAt time.Time

	// Artifacts are the promoted OCI descriptors, read back from the registry.
	Artifacts map[string]LockedArtifact
	// Sidecars are the pinned third-party refs, from the intent.
	Sidecars map[string]string
	// DonorRollback is the release's per-component rollback verdict. Absent
	// entries mean NO for that component.
	DonorRollback map[string]RollbackEvidence

	// Evidence maps a claim id to the statement supporting it, plus that
	// statement's content digest.
	Evidence map[string]EvidencePair

	// Payload is the bundle's member→sha256 map, from BuildPayload.
	Payload map[string]string

	// Coverage is the gate-coverage table the evidence is checked against.
	// Passed in so a candidate can be evaluated against the table AS OF that
	// commit rather than against whatever is checked in now.
	Coverage []GateCoverage
}

// EvidencePair is a statement and the digest of its rendered bytes.
type EvidencePair struct {
	Statement EvidenceStatement
	Digest    string
}

// BuildLock assembles and validates a lock.
//
// It refuses more than it accepts, on purpose. A lock is the document an
// operator's whole verification chain hangs from, and every check below is one
// an attacker or a tired release engineer would otherwise get past.
func BuildLock(in LockInputs) (Lock, error) {
	if in.SourceCommit == "" {
		return Lock{}, errors.New("lock: no source commit; a release that cannot say what it " +
			"was built from is not reproducible even in principle")
	}
	if in.BuiltAt.IsZero() {
		return Lock{}, errors.New("lock: no build time")
	}
	if len(in.Payload) == 0 {
		return Lock{}, errors.New("lock: an empty payload map covers no bundle member, so " +
			"every file in the bundle would be unauthenticated")
	}

	// Every declared artifact must be present, with a descriptor that says what
	// it is. A digest alone cannot distinguish an index from a manifest, and
	// promotion behaves differently for the two.
	for _, name := range ArtifactNames {
		a, ok := in.Artifacts[name]
		if !ok {
			return Lock{}, fmt.Errorf("lock: no descriptor for %s", name)
		}
		if a.Repository == "" {
			return Lock{}, fmt.Errorf("lock: %s has no repository", name)
		}
		if a.Descriptor.Digest == "" {
			return Lock{}, fmt.Errorf("lock: %s has no digest", name)
		}
		if a.Descriptor.MediaType == "" {
			return Lock{}, fmt.Errorf("lock: %s has no mediaType; the read-back after promotion "+
				"cannot then tell an index from a manifest", name)
		}
		if a.Descriptor.Size <= 0 {
			return Lock{}, fmt.Errorf("lock: %s has no size", name)
		}
		if want, ok := in.Intent.Repositories[name]; ok && want != a.Repository {
			return Lock{}, fmt.Errorf("lock: %s was promoted to %s but the intent declares %s",
				name, a.Repository, want)
		}
	}

	// EVERY declared claim needs evidence, and the evidence has to be able to
	// support it. A claim with no statement is the failure this whole split
	// exists to prevent.
	proven := make([]ProvenClaim, 0, len(in.Intent.Claims))
	for _, c := range in.Intent.Claims {
		pair, ok := in.Evidence[c.ID]
		if !ok {
			return Lock{}, fmt.Errorf("lock: claim %q has no evidence statement. The intent "+
				"declares it and no gate produced a conclusion about it; a signed lock must "+
				"not carry a claim with nothing behind it", c.ID)
		}
		if err := AssertEvidenceSupportsClaim(c, pair.Statement, in.Coverage); err != nil {
			return Lock{}, err
		}
		if pair.Digest == "" {
			return Lock{}, fmt.Errorf("lock: claim %q's evidence has no content digest", c.ID)
		}
		// The statement must be about THESE artifacts. Otherwise a passing run
		// against last week's build proves this week's.
		for name, a := range in.Artifacts {
			got, named := pair.Statement.ArtifactDigests[name]
			if !named {
				continue // a gate need not exercise every artifact
			}
			if got != a.Descriptor.Digest.String() {
				return Lock{}, fmt.Errorf("lock: claim %q's evidence examined %s %s, but this "+
					"release promotes %s", c.ID, name, got, a.Descriptor.Digest)
			}
		}
		proven = append(proven, ProvenClaim{
			ID: c.ID, Statement: c.Statement, Gate: c.ProvenByGate,
			Evidence: []EvidenceRef{pair.Statement.Ref(pair.Digest)},
		})
	}
	sort.Slice(proven, func(i, j int) bool { return proven[i].ID < proven[j].ID })

	l := Lock{
		Schema:        LockSchema,
		Version:       in.Intent.Version,
		IntentDigest:  IntentDigest(in.IntentBytes),
		SourceCommit:  in.SourceCommit,
		BuiltAt:       in.BuiltAt.UTC(),
		TargetSchema:  in.Intent.TargetSchema,
		Artifacts:     in.Artifacts,
		Sidecars:      in.Sidecars,
		DonorRollback: in.DonorRollback,
		ProvenClaims:  proven,
		Payload:       in.Payload,
	}

	// The donor-lock digest is DERIVED from the lock, then recorded on it. The
	// projection deliberately joins to the intent digest rather than to this
	// document's, because this document records the projection's — and two
	// documents cannot each be an input to the other.
	dl, err := ProjectDonorLock(l)
	if err != nil {
		return Lock{}, err
	}
	body, err := dl.Render()
	if err != nil {
		return Lock{}, err
	}
	l.DonorLockDigest = DonorLockDigestOf(body)

	// Validate through the same path a consumer uses. A builder with its own
	// notion of validity is a builder that emits documents nothing accepts.
	if err := l.Validate(in.Intent, in.IntentBytes); err != nil {
		return Lock{}, err
	}
	return l, nil
}

// RenderLock writes the lock in the canonical byte form it is signed over.
func RenderLock(l Lock) ([]byte, error) {
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
