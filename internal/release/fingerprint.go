package release

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The configuration fingerprint (P2-M7.3, D-M7.3-10).
//
// # The gap this closes
//
// `upgrade_runs` has `config_fingerprint_before` and `config_fingerprint_after`
// columns, and nothing computed a fingerprint. The apply plumbed a field that
// every caller left empty, and the "after" value was written as the empty
// string. So the one question the columns exist to answer — *did my
// configuration change across this upgrade?* — had the same answer for every
// run, which is no answer.
//
// # It covers the EFFECTIVE configuration
//
// Not the file on disk. Two deployments with byte-identical operator.yaml can
// differ in what they actually run, because environment overrides win — and the
// override is exactly the thing somebody changes during an upgrade and then
// forgets. The caller supplies the effective view.
//
// # It excludes secrets and PRESERVES unknown keys
//
// Excluding secrets is obvious: a fingerprint is written to a table an operator
// pastes into a support thread, and hashing a secret alongside its key name
// would leak nothing but hashing is not the risk — carrying the VALUE into a
// structure that gets logged is, so secret-shaped keys are dropped before the
// hash rather than hashed.
//
// Preserving unknown keys is less obvious and matters more. The fingerprint is
// computed by the TARGET binary, which may not recognize a key a newer release
// added, and by the PREDECESSOR on the way back. If unknown keys were dropped,
// a rollback would report an unchanged fingerprint while the configuration had
// in fact changed — the exact case the column exists for.

// FingerprintFinding reports something notable about the configuration that
// went into a fingerprint.
type FingerprintFinding struct {
	Path   string
	Reason string
}

// Fingerprint hashes an effective configuration document.
//
// The input is YAML because that is what operator.yaml is; the hash is over a
// CANONICAL rendering — sorted keys, one `path = value` line each — rather than
// over the bytes, so reformatting the file does not read as a configuration
// change and reordering two keys does not either.
func Fingerprint(effectiveYAML []byte) (string, []FingerprintFinding, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(effectiveYAML, &doc); err != nil {
		return "", nil, fmt.Errorf("release: effective configuration does not parse: %w", err)
	}
	if len(doc.Content) == 0 {
		// An empty configuration is a legitimate state and has a stable
		// fingerprint, distinct from "we could not compute one".
		return canonicalDigest(nil), nil, nil
	}

	var lines []string
	var findings []FingerprintFinding
	flattenNode(doc.Content[0], "", &lines, &findings)
	sort.Strings(lines)
	return canonicalDigest(lines), findings, nil
}

func canonicalDigest(lines []string) string {
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func flattenNode(n *yaml.Node, prefix string, lines *[]string, findings *[]FingerprintFinding) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			p := key.Value
			if prefix != "" {
				p = prefix + "." + key.Value
			}
			// Secret-shaped keys contribute their PRESENCE and not their value.
			// Whether a secret is configured is a real configuration fact and a
			// change worth noticing; what it is, is not this table's business.
			if secretish.MatchStringConfig(p) {
				*lines = append(*lines, p+" = [secret present]")
				*findings = append(*findings, FingerprintFinding{
					Path:   p,
					Reason: "secret-shaped key: presence is fingerprinted, the value is not",
				})
				continue
			}
			flattenNode(val, p, lines, findings)
		}
	case yaml.SequenceNode:
		// Indexed, because order is meaningful in every sequence Nova's config
		// has — webhook destinations, scopes, trusted proxies — and a set-like
		// hash would call a reordering identical.
		for i, c := range n.Content {
			flattenNode(c, fmt.Sprintf("%s[%d]", prefix, i), lines, findings)
		}
	default:
		*lines = append(*lines, prefix+" = "+n.Value)
	}
}

// secretPattern is the denylist. It is deliberately the same shape as the
// journal's redaction list; the two are separate because one protects a file an
// operator pastes into a bug report and the other protects a database column,
// and merging them would make a change to either affect the other.
type secretPattern struct{}

var secretish2 = secretPattern{}

// MatchStringConfig reports whether a dotted config path names a secret.
func (secretPattern) MatchStringConfig(p string) bool {
	lower := strings.ToLower(p)
	for _, needle := range []string{
		"password", "passwd", "secret", "token", "api_key", "apikey",
		"private_key", "credential", "dsn", "database_url", "signing_key",
		"master_key", "swarm_key",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// secretish is the package-local handle used above. Named so the call site
// reads as a question rather than as a regexp match.
var secretish = secretish2
