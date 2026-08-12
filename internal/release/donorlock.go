package release

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// The donor lock (P2-M7.3, D-M7.3-15, D-M7.3-16).
//
// # It is a projection, not a second signed document
//
// A separately signed donor lock would be a second signing ceremony, a second
// key policy and a second thing that can disagree with the release. Instead the
// donor lock is a DETERMINISTIC PROJECTION of the release lock — the three
// component refs plus their rollback evidence — and the release lock records
// that projection's digest. Verifying it is re-deriving it: the volunteer hashes
// the file, the operator compares against what the release lock says, and there
// is nothing to sign separately.
//
// # It joins to the INTENT digest, not to the lock's
//
// The release lock records the projection's digest, so the projection cannot
// record the lock's: each would then be an input to the other and neither could
// be computed. This is the same circularity the whole milestone exists to
// break, at a smaller scale.
//
// The intent digest is the join instead. It is fixed before any build happens,
// the lock already records it, and it identifies the reviewed decision this
// release realizes — which is the thing an operator actually wants to check a
// donor bundle against.
//
// # It carries no per-node identity
//
// Identity lives in invite-manifest.json, where it already lived. That keeps the
// donor lock BYTE-IDENTICAL for every node in the fleet, which is what lets the
// release lock carry one digest for all of them and what lets an operator
// authorize a rollout once rather than per volunteer.
//
// # Rollback is per component and conditional
//
// The donor topology holds node state, a persistent Kubo repo and Nebula
// config. Reverting image refs is only safe when nothing has already changed
// persistent state in a way the older software cannot read — and that is a
// different answer for nova-node than it is for Kubo. So each component carries
// its own evidence, and the update script rolls back exactly the components
// whose evidence says it may.

// DonorLockSchema is the projection's own format version. Distinct from the
// invite manifest's version: one describes the artifact set, the other the
// bundle layout, and they will not move together.
const DonorLockSchema = 1

// ComponentNames are the three images a donor runs, in a fixed order so the
// projection is byte-stable.
var ComponentNames = []string{"nova-node", "nebula", "kubo"}

// RollbackEvidence states whether one component may be reverted, and on what
// grounds.
type RollbackEvidence struct {
	// Safe reports whether reverting THIS component to Predecessor is
	// supported. False is the conservative answer and the default.
	Safe bool `json:"safe"`
	// Predecessor is the artifact ref rollback would return to. Empty when
	// there is no predecessor to return to, which is itself a reason Safe is
	// false.
	Predecessor string `json:"predecessor,omitempty"`
	// Reason explains the verdict in one sentence, because a volunteer reading
	// `"safe": false` at 3am needs to know whether they are stuck or merely
	// need to ask.
	Reason string `json:"reason"`
}

// DonorComponent is one image and what is known about going back from it.
type DonorComponent struct {
	Ref      string           `json:"ref"`
	Rollback RollbackEvidence `json:"rollback"`
}

// DonorLock is the projection itself.
type DonorLock struct {
	Schema  int    `json:"schema"`
	Release string `json:"release"`
	// IntentDigest ties this projection to the reviewed decision the release
	// realizes. NOT the release lock's digest: the lock records THIS
	// document's digest, and two documents cannot each be an input to the
	// other.
	IntentDigest string `json:"intent_digest"`
	// Components is keyed by ComponentNames.
	Components map[string]DonorComponent `json:"components"`
}

// ProjectDonorLock derives the donor lock from a release lock.
//
// It takes NO digest argument. The projection has to be a pure function of the
// lock, because the lock records the projection's digest: anything else the
// caller could vary would make two operators produce two donor locks for one
// release, and "the release lock carries the projection's digest" would be
// false for at least one of them.
func ProjectDonorLock(l Lock) (DonorLock, error) {
	if l.IntentDigest == "" {
		return DonorLock{}, errors.New("release: the lock records no intent digest, so the " +
			"projection has nothing to join to the reviewed decision")
	}
	node, ok := l.Artifacts["nova-node"]
	if !ok {
		return DonorLock{}, errors.New("release: the lock names no nova-node artifact")
	}

	out := DonorLock{
		Schema: DonorLockSchema, Release: l.Version,
		IntentDigest: l.IntentDigest,
		Components:   map[string]DonorComponent{},
	}

	// nova-node's rollback evidence comes from the release itself: it is Nova
	// software, and whether the previous one runs against whatever this one
	// wrote is a claim the release either makes or does not.
	out.Components["nova-node"] = DonorComponent{
		Ref:      node.Ref(),
		Rollback: declaredRollback(l, "nova-node"),
	}

	for _, name := range []string{"nebula", "kubo"} {
		ref, ok := l.Sidecars[name]
		if !ok || ref == "" {
			return DonorLock{}, fmt.Errorf("release: the lock names no %s sidecar; a donor "+
				"topology is three images and a projection missing one is not one", name)
		}
		out.Components[name] = DonorComponent{Ref: ref, Rollback: declaredRollback(l, name)}
	}
	return out, nil
}

// declaredRollback reads the release's own verdict for one component.
//
// The evidence lives on the LOCK rather than being supplied at conversion time,
// and that is the whole point: the projection has to be a deterministic
// function of the lock, or its digest cannot be something the lock records. An
// operator flag would mean every operator produced a different donor lock for
// the same release, and "the release lock carries the projection's digest"
// would be false.
//
// Absent a declaration the answer is NO. An unevidenced rollback of software
// holding persistent state is a guess, and the thing being guessed with is the
// volunteer's replicas.
func declaredRollback(l Lock, component string) RollbackEvidence {
	if ev, ok := l.DonorRollback[component]; ok {
		if ev.Reason == "" {
			ev.Reason = "declared by the release"
		}
		return ev
	}
	return RollbackEvidence{
		Safe: false,
		Reason: "this release declares no rollback evidence for " + component + ". Going back " +
			"needs proof that the predecessor runs against state this version wrote, and " +
			"until that exists the answer is no rather than probably",
	}
}

// Render writes the projection in the canonical byte form its digest is taken
// over: indented JSON, sorted keys, trailing newline.
//
// Byte-stable, because the release lock records a digest of exactly these
// bytes. encoding/json already sorts map keys; the indentation and the newline
// are pinned here so a future caller cannot change them by passing different
// options.
func (d DonorLock) Render() ([]byte, error) {
	for _, name := range ComponentNames {
		if _, ok := d.Components[name]; !ok {
			return nil, fmt.Errorf("release: the donor lock is missing %s", name)
		}
	}
	if len(d.Components) != len(ComponentNames) {
		names := make([]string, 0, len(d.Components))
		for k := range d.Components {
			names = append(names, k)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("release: the donor lock carries %v; a donor topology is exactly "+
			"%v, and an extra component is one nothing verifies", names, ComponentNames)
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ParseDonorLock reads a projection and checks its shape.
func ParseDonorLock(b []byte) (DonorLock, error) {
	var d DonorLock
	if err := json.Unmarshal(b, &d); err != nil {
		return DonorLock{}, fmt.Errorf("release: donor lock does not parse: %w", err)
	}
	if d.Schema != DonorLockSchema {
		return DonorLock{}, fmt.Errorf("release: donor lock schema %d, this binary understands %d",
			d.Schema, DonorLockSchema)
	}
	if d.IntentDigest == "" {
		return DonorLock{}, errors.New("release: the donor lock names no intent; a projection " +
			"that cannot say what reviewed decision it realizes verifies nothing")
	}
	for _, name := range ComponentNames {
		c, ok := d.Components[name]
		if !ok {
			return DonorLock{}, fmt.Errorf("release: the donor lock is missing %s", name)
		}
		if c.Ref == "" {
			return DonorLock{}, fmt.Errorf("release: %s has no ref", name)
		}
		if c.Rollback.Reason == "" {
			return DonorLock{}, fmt.Errorf("release: %s states no rollback reason; a bare "+
				"true/false leaves a volunteer with no way to tell stuck from ask-your-operator",
				name)
		}
	}
	return d, nil
}

// DonorLockDigestOf is the digest the release lock records for a projection.
// The same sha256-over-exact-bytes rule as everything else in this package.
func DonorLockDigestOf(b []byte) string { return IntentDigest(b) }
