#!/usr/bin/env bash
# scripts/check-gen-deploy.sh — deploy/donor/ must match what the canonical
# templates in internal/deploy generate (P2-M7.2, D-M7.2-5).
#
# §22.5 of the upstream findings: the donor topology was maintained in six
# places that had already diverged on ports, topology, paths, secrets and
# milestone-era comments. deploy/donor/ is now GENERATED; this gate is what
# stops it drifting back.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

go run ./cmd/gen-deploy >/dev/null

if ! git diff --quiet -- deploy/donor/; then
  echo "FAIL: deploy/donor/ is out of date with internal/deploy/templates/." >&2
  echo >&2
  git --no-pager diff --stat -- deploy/donor/ >&2
  echo >&2
  echo "deploy/donor/ is generated. Edit internal/deploy/templates/ instead," >&2
  echo "then run:  make gen-deploy  and commit the result." >&2
  exit 1
fi

# Untracked generated files also count as drift (a renamed target left behind).
if [ -n "$(git ls-files --others --exclude-standard -- deploy/donor/)" ]; then
  echo "FAIL: untracked generated files under deploy/donor/:" >&2
  git ls-files --others --exclude-standard -- deploy/donor/ >&2
  echo "Run 'make gen-deploy' and commit the result." >&2
  exit 1
fi

echo "OK: deploy/donor/ matches internal/deploy/templates/"
