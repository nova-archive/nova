package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
)

// The donor's Nebula certificate fingerprint (P2-M7.3, found by the mixed-fleet
// gate).
//
// # The bug this fixes
//
// `registerReq` never populated `NebulaCertFingerprint`, so every donor sent the
// empty string. `nodes.nebula_cert_fingerprint` is `text UNIQUE NOT NULL`.
//
// The FIRST donor to register therefore inserted "", and every donor after it
// collided on the unique index and got a 500 from /fed/v1/register — with the
// coordinator logging an internal error and the volunteer's agent exiting.
//
// A federation with one donor never sees this. A federation with two cannot
// form. No single-donor test could find it, which is the whole argument for
// standing six up at once.
//
// # Why the fingerprint and not something easier
//
// A UUID would satisfy the constraint and mean nothing. The column exists so an
// operator can tie a registration to the overlay identity they issued, and the
// certificate's fingerprint is that. It is derived from the DER, not from the
// PEM bytes, so re-encoding or a trailing newline does not change it.

// nebulaFingerprint reads the donor's Nebula certificate and returns a stable
// fingerprint of it.
//
// Nova mints X.509 for its Nebula identity material, so the PEM block is parsed
// and the DER hashed. A file that does not parse as PEM is hashed whole rather
// than rejected: the value's job is to be unique and stable per donor, and
// refusing to register over an unparseable overlay certificate would take a
// donor offline for something the coordinator never inspects.
func nebulaFingerprint(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("no nebula_cert_path configured")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(b) == 0 {
		return "", fmt.Errorf("%s is empty", path)
	}
	if block, _ := pem.Decode(b); block != nil {
		sum := sha256.Sum256(block.Bytes)
		return "sha256:" + hex.EncodeToString(sum[:]), nil
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
