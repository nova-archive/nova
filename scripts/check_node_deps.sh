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
  "$MOD/internal/federation/replay"   # P2-M4.1: donor read-source single-use jti replay cache + boot-floor (pure stdlib sync+time)
  "$MOD/internal/ipfs/importspec"   # P2-M4: shared deterministic-import params (no Kubo, no go-cid)
  "$MOD/internal/buildinfo"         # P2-M7.3 P0-c: three link-time strings, zero imports. A donor
                                    # that cannot say what it is makes the fleet census unanswerable.
  "$MOD/internal/release/catalog"   # P2-M7.3 D-M7.3-2c: the compiled-in release identity. A LEAF
                                    # package (fmt + time only) precisely so the donor is not dragged
                                    # into distribution/reference, image-spec, go-digest and x/mod to
                                    # print one version line — internal/release itself stays out.
  "gopkg.in/yaml.v3"   # donor config parsing — the only third-party runtime dep
)

# P2-M7 (D-M7-1): metrics are coordinator-only. HARD DENY — even a future
# allowlist broadening must not admit a metrics stack into the donor graph.
DENIED_PREFIXES=("github.com/prometheus")

deps="$(go list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./cmd/node)"

violations=()
while IFS= read -r p; do
  [ -z "$p" ] && continue
  for d in "${DENIED_PREFIXES[@]}"; do
    case "$p" in "$d"|"$d"/*)
      echo "FAIL: cmd/node imports HARD-DENIED package: $p (metrics are coordinator-only, D-M7-1)" >&2
      exit 1 ;;
    esac
  done
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
