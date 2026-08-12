#!/usr/bin/env bash
# scripts/check-release-docs.sh — the compatibility matrix in UPGRADING.md is
# GENERATED, and has not been hand-edited (P2-M7.3, D-M7.3-19).
#
# UPGRADING.md's matrix is the first thing an operator reads and the last thing
# anyone updates. Prose that restates the intent, the gate coverage and the
# support window is prose that will eventually disagree with them — and the
# disagreement gets discovered by an operator mid-upgrade rather than by a
# reviewer.
#
# Everything OUTSIDE the markers is written by a person, because the sequences
# and the reasoning are not derivable from a data structure. Everything inside
# is rendered, and this is what stops the two from drifting apart.
#
# The comparison is over BYTES, like the catalog gate. Comparing meaning would
# let a hand edit that happens to say the same thing pass, and the property is
# that the block is generated rather than that it is currently accurate.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

go run ./internal/release/cmd/novarel matrix --check

# The documented bootstrap command is the one thing an operator copies BEFORE
# anything from the release has executed, so it is the copy of the signing
# policy that matters most. It must agree with the script and the policy file.
IDENTITY="https://github.com/nova-archive/nova/.github/workflows/release.yml@refs/heads/main"
ISSUER="https://token.actions.githubusercontent.com"

fail=0
for f in docs/UPGRADING.md docs/quickstart/donor.md; do
  if ! grep -qF -- "$IDENTITY" "$f"; then
    echo "FAIL: $f does not carry the release signing identity" >&2
    fail=1
  fi
  if ! grep -qF -- "$ISSUER" "$f"; then
    echo "FAIL: $f does not carry the OIDC issuer" >&2
    fail=1
  fi
  # A regexp identity is one typo away from admitting a workflow nobody meant
  # to trust, and cosign will happily accept the wider pattern.
  if grep -qF -- "--certificate-identity-regexp" "$f"; then
    echo "FAIL: $f uses --certificate-identity-regexp; the identity is an EXACT string" >&2
    fail=1
  fi
done

# UPGRADING.md is normative, so the three sentences it turns on have to be in
# it. Each of these was a decision; a document that loses one silently loses the
# decision with it.
declare -a required=(
  "A backup that has never passed restore verification does not count"
  "migrate down"
  "Unknown is not unsupported"
  "Acknowledgement waives a SKIP"
)
for phrase in "${required[@]}"; do
  if ! grep -qF -- "$phrase" docs/UPGRADING.md; then
    echo "FAIL: docs/UPGRADING.md no longer says: $phrase" >&2
    fail=1
  fi
done

if [ "$fail" -ne 0 ]; then
  echo >&2
  echo "Release documentation is out of sync. Run 'make release-docs' for the matrix;" >&2
  echo "the rest are hand-written sentences that a change removed." >&2
  exit 1
fi

echo "OK: release documentation matches the intent, the coverage and the signing policy"
