#!/usr/bin/env bash
# scripts/check-compose-custody.sh — Plane C of `federation doctor`
# (P2-M7.2, D-M7.2-3).
#
# Asserts that the effective deployment definition makes it IMPOSSIBLE for a
# long-running container to hold issuance authority. Proving that structurally
# is stronger than inspecting a running container, and needs no Docker socket —
# which is what keeps nova-admin from becoming a root-equivalent control plane.
#
# Prefers the real `docker compose config` rendering. When Compose is
# unavailable (hosted CI), it falls back to a checked-in rendered fixture so the
# gate still runs hermetically; a drift test keeps the fixture honest.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

base="docker/docker-compose.yml"
overlay="deploy/operator/compose.federation.yaml"
fixture="deploy/operator/testdata/rendered-federation-config.yaml"

if [ ! -f "$overlay" ]; then
  echo "SKIP: $overlay does not exist yet (lands with the federation overlay)"
  exit 0
fi

rendered="$(mktemp)"
trap 'rm -f "$rendered"' EXIT

if docker compose version >/dev/null 2>&1; then
  if docker compose -f "$base" -f "$overlay" --profile prod --profile federation \
      config >"$rendered" 2>/dev/null; then
    echo "using: docker compose config"
  else
    echo "WARN: docker compose config failed; falling back to the checked-in fixture" >&2
    cp "$fixture" "$rendered"
  fi
else
  if [ ! -f "$fixture" ]; then
    echo "FAIL: docker compose unavailable and no fixture at $fixture" >&2
    exit 1
  fi
  echo "using: checked-in fixture (docker compose unavailable)"
  cp "$fixture" "$rendered"
fi

go run ./cmd/novactl federation compose-policy --file "$rendered"
