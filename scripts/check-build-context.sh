#!/usr/bin/env bash
# scripts/check-build-context.sh — the Docker build context is controlled
# (P2-M7.3, D-M7.3-4 / P0-a).
#
# All three Dockerfiles do `COPY . .`. Once release digests are contractual,
# whatever happens to be in the working tree becomes part of the signed
# artifact: operator secrets leak in, and the digest changes for reasons that
# have nothing to do with the release.
#
# WHY THIS IS A PROBE BUILD AND NOT A PATTERN MATCHER.
# `git ls-files` cannot model Docker's ignore semantics, never enumerates
# `.git`, and would filter out `docker/.env` — the single most important file
# to keep out — before the check ever saw it. Reimplementing Moby's matcher
# would mean testing our copy of the rules rather than the ones the daemon
# applies. So this builds a throwaway image that copies the real context with
# the real .dockerignore and asks the result what arrived.
#
# The probe plants sentinels in the working tree and removes exactly what it
# planted. A positive sentinel carrying a fresh nonce proves the probe observed
# the current tree rather than a cached layer.
#
# Requires Docker. Set NOVA_SKIP_DOCKER_PROBE=1 to run only the static
# assertions; the probe is the part that actually proves anything, so skipping
# it is for environments without a daemon, not for convenience.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
ok()   { echo "ok: $*"; }

# --- static assertions ------------------------------------------------------
# Presence and shape only. This is not an attempt to model the semantics; it
# catches the file being deleted or a brace-expansion pattern creeping back in.

[ -f .dockerignore ] || { echo "FAIL: .dockerignore is missing" >&2; exit 1; }

if grep -nE '\{[^}]*,[^}]*\}' .dockerignore; then
  fail ".dockerignore uses brace expansion, which Docker does not support"
fi

for required in '.git' 'docker/.env' 'deploy/operator/invites' 'node_modules'; do
  grep -qxF "$required" .dockerignore \
    || fail ".dockerignore does not exclude $required"
done

if grep -qxF 'web' .dockerignore; then
  fail ".dockerignore excludes web/, but the coordinator image builds the SPAs"
fi

if [ "${NOVA_SKIP_DOCKER_PROBE:-0}" = "1" ]; then
  echo "WARNING: NOVA_SKIP_DOCKER_PROBE=1 — the probe build did not run," >&2
  echo "         so nothing here proves what the daemon actually receives." >&2
  [ "$failures" -eq 0 ] || exit 1
  exit 0
fi

command -v docker >/dev/null 2>&1 || {
  echo "FAIL: docker is required. Set NOVA_SKIP_DOCKER_PROBE=1 to run static checks only." >&2
  exit 1
}

# --- sentinels --------------------------------------------------------------
nonce="$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
planted=()
planted_dirs=()

plant() { # plant <path> <content>
  local p="$1" body="$2" d
  d="$(dirname "$p")"
  if [ ! -d "$d" ]; then
    mkdir -p "$d"
    planted_dirs+=("$d")
  fi
  if [ -e "$p" ]; then
    echo "FAIL: sentinel $p already exists; refusing to clobber it" >&2
    exit 1
  fi
  printf '%s\n' "$body" > "$p"
  planted+=("$p")
}

cleanup() {
  local p d
  for p in "${planted[@]:-}"; do [ -n "$p" ] && rm -f "$p"; done
  for d in "${planted_dirs[@]:-}"; do [ -n "$d" ] && rmdir "$d" 2>/dev/null || true; done
  rm -f "$listing" 2>/dev/null || true
  docker image rm -f "$probe_tag" >/dev/null 2>&1 || true
}
listing="$(mktemp)"
probe_tag="nova-build-context-probe:${nonce}"
trap cleanup EXIT

# MUST NOT reach the daemon.
plant './.nova-ctx-probe.key'                          "root-level key $nonce"
plant './web/.nova-ctx-probe.pem'                      "nested pem $nonce"
plant './deploy/operator/invites/.nova-ctx-probe'      "invite material $nonce"
plant './deploy/operator/import/.nova-ctx-probe'       "imported authority $nonce"
plant './docker/.env.nova-ctx-probe'                   "operator secrets $nonce"
plant './reports/.nova-ctx-probe'                      "local report $nonce"

# MUST reach the daemon. The nonce proves this run's tree was copied.
plant './cmd/.nova-ctx-probe-included'                 "included $nonce"

# --- probe build ------------------------------------------------------------
# Dockerfile on stdin so the probe itself never becomes part of the context.
# No `# syntax=` line: this must work on the legacy builder and BuildKit alike.
probe_base='debian:bookworm-slim@sha256:60eac759739651111db372c07be67863818726f754804b8707c90979bda511df'

echo "probing the build context (nonce ${nonce})..."

# The probe must end up in the local daemon so we can run `find` inside it.
# setup-buildx-action's default builder uses the docker-container driver, which
# does not load its output without --load; the legacy builder rejects that flag.
if docker buildx version >/dev/null 2>&1; then
  probe_build=(docker buildx build --load -q -f - -t "$probe_tag" .)
else
  probe_build=(docker build -q -f - -t "$probe_tag" .)
fi

printf 'FROM %s\nCOPY . /ctx\n' "$probe_base" | "${probe_build[@]}" >/dev/null

docker run --rm "$probe_tag" \
  find /ctx -mindepth 1 -printf '%P\n' > "$listing"

present() { grep -qxF "$1" "$listing"; }

# --- assertions -------------------------------------------------------------
# Freshness first: every other assertion is vacuous if the context is stale.
if present 'cmd/.nova-ctx-probe-included'; then
  copied_nonce="$(docker run --rm "$probe_tag" cat /ctx/cmd/.nova-ctx-probe-included)"
  case "$copied_nonce" in
    *"$nonce") ok "the probe observed this run's working tree" ;;
    *) fail "the probe read a stale context (expected nonce $nonce, got '$copied_nonce')" ;;
  esac
else
  fail "a required source file was excluded: cmd/.nova-ctx-probe-included"
fi

excluded() { # excluded <path> <why>
  if present "$1"; then
    fail "$1 reached the build context — $2"
  else
    ok "excluded: $1"
  fi
}

excluded '.git'                                  'branch state would make the image digest unstable'
excluded 'docker/.env'                           'these are the operator secrets'
excluded 'docker/.env.nova-ctx-probe'            'docker/.env.* must be excluded too'
excluded '.nova-ctx-probe.key'                   'a root-level private key'
excluded 'web/.nova-ctx-probe.pem'               'a nested certificate; **/*.pem must match at depth'
excluded 'deploy/operator/invites'               'invites carry donor key material'
excluded 'deploy/operator/import'                'the import directory holds an adopted CA'
excluded 'reports'                               'local reports vary per developer'
excluded 'node_modules'                          'the host tree would overwrite the npm ci output'

included() { # included <path>
  if present "$1"; then ok "included: $1"; else fail "$1 was excluded but the images need it"; fi
}

included 'go.mod'
included 'go.sum'
included 'cmd/coordinator/main.go'
included 'cmd/node/main.go'
included 'docker/init/entrypoint.sh'
included 'package.json'
included 'package-lock.json'
included 'web/admin/package.json'
included 'internal/db/migrations/MANIFEST.sha256'

if [ "$failures" -gt 0 ]; then
  echo >&2
  echo "$failures build-context failure(s). Adjust .dockerignore — never the gate." >&2
  echo "Context listing kept at: $listing" >&2
  trap - EXIT
  docker image rm -f "$probe_tag" >/dev/null 2>&1 || true
  for p in "${planted[@]:-}"; do [ -n "$p" ] && rm -f "$p"; done
  for d in "${planted_dirs[@]:-}"; do [ -n "$d" ] && rmdir "$d" 2>/dev/null || true; done
  exit 1
fi

echo
echo "OK: the build context excludes secrets and local state, and still carries every source the images need"
