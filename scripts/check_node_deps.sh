#!/usr/bin/env bash
# Fails if the cmd/node build graph imports anything outside the donor-safe
# allowlist. DENY-BY-DEFAULT over ALL non-stdlib deps (first-party AND
# third-party): a heavy/risky transitive dep is a violation just like an
# operator-only package. Stdlib is filtered out via go list's .Standard flag.
# Test deps (testify, etc.) do NOT appear in `go list -deps ./cmd/node`, so they
# need no allowlisting. This is the load-bearing P2-M1 boundary gate.
set -euo pipefail

MOD="github.com/nova-archive/nova"
# Donor-safe runtime roots. Adding an entry is a deliberate, reviewed act.
ALLOWED=(
  "$MOD/cmd/node"
  "$MOD/internal/secret"
  "$MOD/internal/node"
  "$MOD/internal/federation/wire"
  "$MOD/internal/federation/transport"
  "gopkg.in/yaml.v3"   # donor config parsing — the only third-party runtime dep
)

deps="$(go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./cmd/node)"

violations=()
while IFS= read -r p; do
  [ -z "$p" ] && continue
  ok=0
  for a in "${ALLOWED[@]}"; do
    case "$p" in "$a"|"$a"/*) ok=1; break ;; esac
  done
  [ "$ok" -eq 0 ] && violations+=("$p")
done <<< "$deps"

if [ "${#violations[@]}" -ne 0 ]; then
  echo "FAIL: cmd/node imports non-allowlisted package(s):" >&2
  printf '  %s\n' "${violations[@]}" >&2
  exit 1
fi
echo "OK: cmd/node dependency boundary clean"
