#!/usr/bin/env bash
# scripts/check-compose-topology.sh — every DOCUMENTED Compose combination
# renders, and renders the artifacts it is supposed to (P2-M7.3, D-M7.3-14).
#
# The operator's deployment is assembled from up to four files, and which
# artifact each service runs depends on which of them were passed and in what
# order. That is exactly the kind of thing nobody checks until an upgrade puts
# the wrong image somewhere: a base that still carried `build:` would let a
# `docker compose build` from months ago outrank the release you just verified,
# and the symptom is a version string, not a crash.
#
# The combinations below are the documented ones. base+override is included
# because it is the LIVE deployment's observed configuration — Nova's commands
# pass -f explicitly, which suppresses Compose's automatic
# docker-compose.override.yml discovery, so the override only applies when it
# is named.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

if ! docker compose version >/dev/null 2>&1; then
  echo "SKIP: docker compose is not available; this gate needs the real renderer" >&2
  echo "      (modelling Compose's merge semantics in shell is how a gate ends up" >&2
  echo "       agreeing with itself rather than with Docker)" >&2
  exit 0
fi

BASE="docker/docker-compose.yml"
DEV="docker/docker-compose.dev.yml"
FED="deploy/operator/compose.federation.yaml"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# An override that carries `build:`, which is what an operator with a local
# customization actually writes. It must not break any combination.
OVERRIDE="$work/override.yml"
cat > "$OVERRIDE" <<'EOF'
services:
  coordinator:
    build:
      context: ..
      dockerfile: docker/coordinator.Dockerfile
    environment:
      NOVA_TRUSTED_PROXIES: "10.0.0.0/8"
EOF

# Secrets the base requires. Values are irrelevant: nothing is started.
ENVFILE="$work/.env"
cat > "$ENVFILE" <<'EOF'
POSTGRES_PASSWORD=render-only
EOF

# A release env, as the bootstrap would install it.
RELENV="$work/release.env"
cat > "$RELENV" <<'EOF'
NOVA_RELEASE_VERSION=v0.0.0-render
NOVA_COORDINATOR_REF=ghcr.io/nova-archive/nova-coordinator@sha256:1111111111111111111111111111111111111111111111111111111111111111
NOVA_ADMIN_REF=ghcr.io/nova-archive/nova-admin@sha256:2222222222222222222222222222222222222222222222222222222222222222
EOF

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }

render() { # render <out> <compose args...>
  local out="$1"; shift
  if ! docker compose "$@" --env-file "$ENVFILE" --profile prod --profile federation \
        --profile upgrade config > "$out" 2>"$out.err"; then
    fail "$* did not render:"
    sed 's/^/      /' "$out.err" >&2
    return 1
  fi
  return 0
}

# service_image <rendered> <service>
service_image() {
  python3 - "$1" "$2" <<'PY'
import sys
try:
    import yaml
except ImportError:
    sys.exit(0)  # reported by the caller as "unknown"
doc = yaml.safe_load(open(sys.argv[1]))
svc = (doc.get("services") or {}).get(sys.argv[2])
print("" if svc is None else svc.get("image", ""))
PY
}

# service_field <rendered> <service> <key>
service_field() {
  python3 - "$1" "$2" "$3" <<'PY'
import json, sys
try:
    import yaml
except ImportError:
    sys.exit(0)
doc = yaml.safe_load(open(sys.argv[1]))
svc = (doc.get("services") or {}).get(sys.argv[2])
if svc is None:
    print("")
else:
    v = svc.get(sys.argv[3], "")
    print(v if isinstance(v, str) else json.dumps(v))
PY
}

if ! python3 -c "import yaml" 2>/dev/null; then
  echo "SKIP: python3 yaml is not available; this gate reads the rendered config" >&2
  exit 0
fi

# ── 1. Every documented combination renders ─────────────────────────────────
render "$work/base.yml"            -f "$BASE"                                 || true
render "$work/base-dev.yml"        -f "$BASE" -f "$DEV"                       || true
render "$work/base-fed.yml"        -f "$BASE" -f "$FED"                       || true
render "$work/base-dev-fed.yml"    -f "$BASE" -f "$DEV" -f "$FED"             || true
render "$work/base-ovr.yml"        -f "$BASE" -f "$OVERRIDE"                  || true
render "$work/base-ovr-fed.yml"    -f "$BASE" -f "$OVERRIDE" -f "$FED"        || true

# ── 2. The federation overlay uses the RELEASED admin digest ────────────────
#
# When a release env is in play, nova-admin must be the digest it names. The
# service that holds issuance authority is the last one that should be running
# "whichever image happened to be in the local cache".
if docker compose -f "$BASE" -f "$FED" --env-file "$ENVFILE" --env-file "$RELENV" \
     --profile prod --profile federation config > "$work/released.yml" 2>"$work/released.err"; then
  got="$(service_image "$work/released.yml" nova-admin)"
  case "$got" in
    *"@sha256:2222"*) ;;
    *) fail "nova-admin renders as '$got', not the released digest from the release env" ;;
  esac
  gotc="$(service_image "$work/released.yml" coordinator)"
  case "$gotc" in
    *"@sha256:1111"*) ;;
    *) fail "coordinator renders as '$gotc', not the released digest from the release env" ;;
  esac
else
  fail "base+federation with a release env did not render:"
  sed 's/^/      /' "$work/released.err" >&2
fi

# The release env must WIN over docker/.env, because it is passed last. A stale
# ref in the operator's own file must not outrank the release they verified.
cat > "$work/stale.env" <<'EOF'
POSTGRES_PASSWORD=render-only
NOVA_ADMIN_REF=ghcr.io/nova-archive/nova-admin@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
EOF
if docker compose -f "$BASE" -f "$FED" --env-file "$work/stale.env" --env-file "$RELENV" \
     --profile prod --profile federation config > "$work/ordered.yml" 2>/dev/null; then
  got="$(service_image "$work/ordered.yml" nova-admin)"
  case "$got" in
    *"@sha256:2222"*) ;;
    *) fail "with --env-file docker/.env --env-file release.env, nova-admin resolved to '$got'; the release env is passed LAST and must win" ;;
  esac
fi

# ── 3. nova-admin mounts the federation PKI and nova-doctor does not ────────
#
# Same image, different mounts. This is the custody boundary, and it is a
# property of the rendered topology rather than of any running container.
admin_vols="$(service_field "$work/base-fed.yml" nova-admin volumes)"
doctor_vols="$(service_field "$work/base-fed.yml" nova-doctor volumes)"
case "$admin_vols" in
  *fedpki*) ;;
  *) fail "nova-admin does not mount nova-fedpki; it is the only service that may" ;;
esac
case "$doctor_vols" in
  *fedpki*) fail "nova-doctor mounts nova-fedpki; a diagnostic needs the CA CERTIFICATE, never a CA key" ;;
esac

# ── 4. The dev overlay builds, and the base does not ────────────────────────
for svc in coordinator migrate; do
  policy="$(service_field "$work/base-dev.yml" "$svc" pull_policy)"
  if [ "$policy" != "build" ]; then
    fail "dev overlay: $svc pull_policy is '$policy', want 'build' — otherwise which bytes you run depends on whether a stale local image exists"
  fi
  if [ -n "$(service_field "$work/base.yml" "$svc" build)" ]; then
    fail "base: $svc still carries a build section; a release deployment must not be able to run a locally built image by accident"
  fi
done
if [ -n "$(service_field "$work/base-fed.yml" nova-admin build)" ]; then
  fail "federation overlay: nova-admin still carries a build section"
fi

# ── 5. An override carrying build: does not break the released topology ─────
#
# It is the operator's file and it wins, by design — but only for the service
# it names, and the federation overlay must still render on top of it.
if [ -z "$(service_field "$work/base-ovr-fed.yml" nova-admin image)" ]; then
  fail "base+override+federation lost nova-admin"
fi

if [ "$failures" -gt 0 ]; then
  echo >&2
  echo "$failures compose-topology problem(s)." >&2
  exit 1
fi
echo "OK: every documented compose combination renders the artifacts it should"
