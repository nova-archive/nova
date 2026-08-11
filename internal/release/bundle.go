package release

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strings"
)

// The release bundle (P2-M7.3, D-M7.3-2d).
//
// # What is in it
//
// The intent bytes; the signed lock; the Sigstore bundle and certificate; an
// auditable copy of the verification policy; the signed evidence statements;
// the release Compose environment; a release-specific UPGRADING.md;
// scripts/nova-release; and a hash manifest.
//
// # The authentication graph, and why it is acyclic
//
//	lock.sigstore.json  ──authenticates──▶  lock (exact bytes)
//	lock                ──hashes──────────▶  every payload member
//	                                          (intent, policy copy, evidence,
//	                                           compose env, UPGRADING.md,
//	                                           scripts/nova-release)
//	lock                ──hashes──────────▶  hashes.txt   (optional, derived)
//	hashes.txt          ──derived from────▶  the lock's payload map
//	                        (covers no lock, no signature envelope, not itself)
//
// Root: lock.sigstore.json. Excluded from hashing: the lock itself and its
// authentication envelope — that envelope CONTAINS the signature over the lock,
// so a lock hashing it would have to be written after it was signed.
//
// hashes.txt is a human-readable convenience DERIVED from the payload map,
// never an authority. Every deployable byte has exactly one authenticated path:
// not zero, and not two.
//
// # Why the Compose environment is covered
//
// Signing the description while the deployment reads something else is the
// whole attack. The environment file the operator's Compose actually consumes
// is a payload member like any other.

// AuthRoot is the only document that authenticates rather than being
// authenticated. Everything else in the bundle is reachable from it.
const AuthRoot = "lock.sigstore.json"

// BundleFile is one member and its bytes.
type BundleFile struct {
	Path  string
	Bytes []byte
}

// Sha256 renders the member's content digest in the payload map's form.
func (f BundleFile) Sha256() string {
	sum := sha256.Sum256(f.Bytes)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// BuildPayload computes the payload map for a set of members.
//
// It REFUSES to hash anything in the authentication envelope, so a caller
// cannot create a cycle by handing it the wrong file list — the rule is
// enforced where the map is built, not only where it is validated.
func BuildPayload(files []BundleFile) (map[string]string, error) {
	out := make(map[string]string, len(files))
	for _, f := range files {
		clean := path.Clean(f.Path)
		if clean != f.Path || strings.HasPrefix(clean, "..") || path.IsAbs(clean) {
			return nil, fmt.Errorf("bundle: %q is not a clean relative path", f.Path)
		}
		if slices.Contains(excludedFromPayload, clean) {
			return nil, fmt.Errorf("bundle: refusing to hash %s — it is the lock or its "+
				"authentication envelope, and hashing it would make the graph cyclic", clean)
		}
		if _, dup := out[clean]; dup {
			return nil, fmt.Errorf("bundle: %s appears twice", clean)
		}
		out[clean] = f.Sha256()
	}
	for _, m := range requiredMembers {
		if _, ok := out[m]; !ok {
			return nil, fmt.Errorf("bundle: no %s; an uncovered member can be substituted "+
				"between signing and use", m)
		}
	}
	return out, nil
}

// RenderHashes writes the human-readable manifest, DERIVED from the payload
// map in sorted order. Sorted so it is byte-stable, derived so it can never
// disagree with the authority.
func RenderHashes(payload map[string]string) []byte {
	paths := make([]string, 0, len(payload))
	for p := range payload {
		if p == MemberHashes {
			continue // it cannot list its own digest
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var b strings.Builder
	b.WriteString("# Derived from the signed lock's payload map. NOT an authority:\n")
	b.WriteString("# verify the lock, then compare against the lock, never against this file.\n")
	for _, p := range paths {
		fmt.Fprintf(&b, "%s  %s\n", strings.TrimPrefix(payload[p], "sha256:"), p)
	}
	return []byte(b.String())
}

// VerifyPayload checks a set of files against an authenticated lock's payload
// map. It reports the FIRST mismatch with the member named, and refuses a file
// set that is missing or has gained a member.
//
// It takes the lock as a value rather than a path because the caller is
// expected to have re-hashed the lock bytes against a digest that arrived
// through the authenticated chain.
func VerifyPayload(l Lock, files []BundleFile) error {
	seen := map[string]bool{}
	for _, f := range files {
		want, ok := l.Payload[f.Path]
		if !ok {
			return fmt.Errorf("bundle: %s is present but the lock does not cover it; a member "+
				"with no authenticated path is an unsigned file riding along", f.Path)
		}
		if got := f.Sha256(); got != want {
			return fmt.Errorf("bundle: %s has been modified (lock says %s, file is %s)",
				f.Path, want, got)
		}
		seen[f.Path] = true
	}
	for p := range l.Payload {
		if !seen[p] {
			return fmt.Errorf("bundle: the lock covers %s but it is missing", p)
		}
	}
	return nil
}

// AuthPaths reports, for each member, how many authenticated paths reach it.
//
// The graph must be acyclic AND total: exactly one path per deployable byte,
// neither zero (an unsigned file) nor two (a second authority that can
// disagree). This is the assertion the design's graph diagram makes executable.
func AuthPaths(l Lock, members []string) map[string]int {
	out := map[string]int{}
	for _, m := range members {
		switch {
		case m == AuthRoot:
			// The root authenticates; nothing authenticates it in-bundle. That is
			// what "out-of-band trust anchor" means.
			out[m] = 0
		case slices.Contains(excludedFromPayload, m):
			// The lock is authenticated by the root, not by the payload map.
			if m == "lock.json" {
				out[m] = 1
			} else {
				out[m] = 0
			}
		default:
			if _, ok := l.Payload[m]; ok {
				out[m] = 1
			} else {
				out[m] = 0
			}
		}
	}
	return out
}

// ReadBundle loads every regular file under fsys as a BundleFile, in sorted
// path order.
func ReadBundle(fsys fs.FS) ([]BundleFile, error) {
	var files []BundleFile
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, rerr := fs.ReadFile(fsys, p)
		if rerr != nil {
			return rerr
		}
		files = append(files, BundleFile{Path: p, Bytes: b})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errors.New("bundle: empty")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}
