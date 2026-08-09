#!/usr/bin/env bash
# scripts/check-no-mutable-tags.sh — operational templates and deployment
# artifacts must pin images by digest (P2-M7.2, D-M7.2-5).
#
# The donor quickstart correctly tells volunteers to verify and pin a signed
# digest, while the generated artifacts still shipped `:latest`. Digest pinning
# is the only way the operator and the volunteer are running the same bytes,
# and it is what makes the cosign/provenance verification meaningful.
#
# P2-M7.1 pinned the operator-side images; the donor-side artifacts were missed.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

failures=0

# Files scanned for unpinned image references.
mapfile -t targets < <(
  git ls-files -- \
    'internal/deploy/templates/*' \
    'deploy/**/*.yaml' 'deploy/**/*.yml' \
    'docker/docker-compose*.yml' \
    'docker/*.Dockerfile' 2>/dev/null | sort
)

# ALLOWLIST, each entry justified:
#
#   REPLACE-WITH-DIGEST  deploy/donor is a reference copy; the real digest is
#                        chosen per release by the operator issuing the invite,
#                        and node invite refuses a mutable tag at runtime.
#   nova-node:dev        a locally built image, never pulled.
#   nova-admin:dev       likewise — built from docker/admin.Dockerfile, whose
#                        own FROM lines ARE digest-pinned and are checked here.
#   {{.                  a template placeholder resolved at render time, which
#                        DonorParams.Validate already digest-checks.
allow_re='REPLACE-WITH-DIGEST|nova-node:dev|nova-admin:dev|\{\{\.'

for f in "${targets[@]}"; do
  [ -f "$f" ] || continue
  while IFS= read -r hit; do
    lineno="${hit%%:*}"
    body="${hit#*:}"

    # Skip comments.
    trimmed="${body#"${body%%[![:space:]]*}"}"
    case "$trimmed" in \#*) continue ;; esac

    # Already digest-pinned?
    case "$body" in *@sha256:*) continue ;; esac

    # Allowlisted?
    if printf '%s' "$body" | grep -Eq "$allow_re"; then
      continue
    fi

    echo "FAIL: $f:$lineno unpinned image reference" >&2
    echo "      ${trimmed}" >&2
    failures=$((failures + 1))
  done < <(grep -nE '^[[:space:]]*(image:|FROM )' "$f" 2>/dev/null || true)
done

if [ "$failures" -gt 0 ]; then
  echo >&2
  echo "$failures unpinned image reference(s). Pin as image:tag@sha256:<digest>." >&2
  echo "Run scripts/refresh-docker-digests.sh to re-resolve existing pins." >&2
  exit 1
fi

echo "OK: every operational image reference is digest-pinned"
