package release

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
)

// Assembling a bundle (P2-M7.3, D-M7.3-2d).
//
// # The gap this closes
//
// The package could verify a bundle and could not build one. `BuildPayload`
// computed a map from members a caller had already collected; nothing collected
// them, wrote them out, or produced the directory an operator downloads. The
// release workflow's publication step therefore had nothing to publish.
//
// # The order is forced, and it is the acyclic graph
//
//	1. gather the payload members
//	2. compute the payload map over them
//	3. render hashes.txt FROM that map, then re-compute the map to include it
//	4. build the lock, which carries the map
//	5. write the lock
//	6. sign the lock — the signature lands OUTSIDE the payload, because a lock
//	   cannot hash the file that contains the signature over it
//
// Step 3 looks redundant and is not: hashes.txt is derived from the map, so the
// map must exist before the file does, and the file must then be covered like
// any other member. The alternative is a bundle member with no authenticated
// path, which is an unsigned file riding along.

// BundleInputs is everything that goes into a release bundle.
type BundleInputs struct {
	// IntentBytes are the exact reviewed bytes.
	IntentBytes []byte
	// Policy is the auditable copy of the signer policy.
	Policy []byte
	// ComposeEnv is the release env the deployment actually consumes.
	ComposeEnv []byte
	// Upgrading is the release-specific UPGRADING.md.
	Upgrading []byte
	// Bootstrap is scripts/nova-release.
	Bootstrap []byte
	// Evidence maps a bundle-relative path under evidence/ to its bytes.
	Evidence map[string][]byte
}

// AssembleBundle writes the bundle directory and returns the payload map.
//
// The LOCK is not written here. It cannot be: the lock carries the payload map,
// so it does not exist until this returns, and writing a placeholder would put
// a file in the directory that the map does not cover.
func AssembleBundle(dir string, in BundleInputs) (map[string]string, error) {
	files := []BundleFile{
		{Path: MemberIntent, Bytes: in.IntentBytes},
		{Path: MemberPolicy, Bytes: in.Policy},
		{Path: MemberComposeEnv, Bytes: in.ComposeEnv},
		{Path: MemberUpgrading, Bytes: in.Upgrading},
		{Path: MemberBootstrap, Bytes: in.Bootstrap},
	}
	for _, f := range files {
		if len(f.Bytes) == 0 {
			return nil, fmt.Errorf("bundle: %s is empty; an empty required member is a member "+
				"nobody notices is missing", f.Path)
		}
	}

	evPaths := make([]string, 0, len(in.Evidence))
	for p := range in.Evidence {
		if !isUnder(p, "evidence/") {
			return nil, fmt.Errorf("bundle: evidence member %q is not under evidence/", p)
		}
		evPaths = append(evPaths, p)
	}
	sort.Strings(evPaths)
	for _, p := range evPaths {
		files = append(files, BundleFile{Path: p, Bytes: in.Evidence[p]})
	}
	if len(evPaths) == 0 {
		return nil, fmt.Errorf("bundle: no evidence statements. Every claim in the lock cites " +
			"one, and a bundle that omits them ships citations to documents the operator " +
			"cannot read")
	}

	// Steps 2 and 3.
	payload, err := BuildPayload(files)
	if err != nil {
		return nil, err
	}
	hashes := RenderHashes(payload)
	files = append(files, BundleFile{Path: MemberHashes, Bytes: hashes})
	payload, err = BuildPayload(files)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	for _, f := range files {
		full := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return nil, err
		}
		mode := os.FileMode(0o644)
		if f.Path == MemberBootstrap {
			// The operator is told to run it. A bundle whose bootstrap is not
			// executable fails at step three for a reason that has nothing to
			// do with trust.
			mode = 0o755
		}
		if err := os.WriteFile(full, f.Bytes, mode); err != nil {
			return nil, err
		}
	}
	return payload, nil
}

// VerifyAssembled re-reads the directory and checks it against the lock, the
// same way the operator's bootstrap will.
//
// Run immediately after writing, because the useful time to discover that the
// bundle does not verify is before it is signed and published, not when the
// first operator runs `nova-release verify`.
func VerifyAssembled(dir string, l Lock) error {
	fsys := os.DirFS(dir)
	files, err := ReadBundle(fsys)
	if err != nil {
		return err
	}
	// The lock and its signature envelope are excluded from the payload by
	// construction, so they are excluded from the comparison too.
	kept := files[:0]
	for _, f := range files {
		if isExcludedFromPayload(f.Path) {
			continue
		}
		kept = append(kept, f)
	}
	return VerifyPayload(l, kept)
}

func isExcludedFromPayload(p string) bool {
	for _, e := range excludedFromPayload {
		if p == e {
			return true
		}
	}
	return false
}

func isUnder(p, prefix string) bool {
	clean := path.Clean(p)
	return len(clean) > len(prefix) && clean[:len(prefix)] == prefix
}
