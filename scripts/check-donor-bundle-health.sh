#!/usr/bin/env bash
# scripts/check-donor-bundle-health.sh — a generated donor bundle's nova-node
# service actually reaches Docker's `healthy` state (P2-M7.3, P0-b).
#
# The defect this closes: donor-compose.yaml.tmpl overrode the probe with
# `/nova-node`, while the binary is installed at /usr/local/bin/nova-node. Every
# donor produced by the documented path reported unhealthy while working
# perfectly. No gate had ever started a generated bundle, so nothing noticed.
#
# HOW THIS TEST OBTAINS OR AVOIDS /dev/net/tun.
# It avoids it. The canonical topology puts kubo and nova-node inside Nebula's
# network namespace, which needs NET_ADMIN and a TUN device. This gate starts
# ONLY the nova-node service, with a TUN-free override that replaces
# `network_mode: service:nebula` with an ordinary bridge network, and
# `--no-deps` so nebula and kubo are never created. That is sound for what is
# being proven: node.yaml binds the health endpoint on 127.0.0.1, and the probe
# runs inside the same container, so the overlay is not on the path. Whole-
# topology behaviour belongs to the Docker+TUN tier (federation_deploy_e2e.sh).
#
# TWO WORKAROUNDS, DELIBERATELY LOUD.
# The generated bundle cannot start as shipped, for two reasons that are older
# than this milestone and outside P0-b's scope. The fixture works around both
# and prints them on every run, so a green gate never makes them invisible:
#
#   1. `node invite` writes secrets/ 0600 and every directory 0700, owned by
#      whoever ran it. nova-node's image runs as distroless `nonroot` (65532),
#      which matches no uid on a volunteer's machine, so the container cannot
#      read its own key material. Loosening the modes would contradict
#      TestInvite_SecretsAreNotWorldReadable, a deliberate M7.2 decision, so the
#      fixture chmods its own copy instead of changing the product.
#
#   2. `node invite` emits no nebula/nebula.crt and no secrets/nebula_key, and
#      the "Nebula CA" federation init mints is an X.509 Ed25519 CA rather than
#      a Nebula-format one — `nebula-cert sign` rejects it outright. A real
#      donor overlay identity therefore cannot be produced from a Nova invite
#      today. nova-node only stats these two paths, so the fixture supplies
#      placeholders. This gate proves the health probe, not the overlay.
#
# Requires Docker. Builds nova-node from docker/node.Dockerfile unless
# NOVA_NODE_IMAGE names an image to use instead.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

step() { printf '\n=== %s ===\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }
note() { printf 'NOTE: %s\n' "$*"; }

command -v docker >/dev/null 2>&1 || fail "docker is required"

work="$(mktemp -d)"
project="nova-donor-bundlehealth-$$"
cleanup() {
  if [ -f "$work/bundle/compose.yaml" ]; then
    docker compose -p "$project" \
      -f "$work/bundle/compose.yaml" -f "$work/bundle/compose.tunfree.yaml" \
      down -v --remove-orphans >/dev/null 2>&1 || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

step "0. the donor image under test"
image="${NOVA_NODE_IMAGE:-}"
if [ -z "$image" ]; then
  image="nova-node:bundle-health"
  docker build -f docker/node.Dockerfile -t "$image" . >"$work/build.log" 2>&1 \
    || { tail -30 "$work/build.log" >&2; fail "could not build the donor image"; }
fi
echo "image: $image"

# The image must declare its own probe — inheriting nothing is not a fix.
docker image inspect -f '{{if .Config.Healthcheck}}{{.Config.Healthcheck.Test}}{{end}}' "$image" \
  | grep -q -- '--healthcheck' \
  || fail "$image declares no HEALTHCHECK; the generated bundle would inherit nothing"
echo "ok: the image declares its own probe"

step "1. mint a federation and issue a real invite"
mkdir -p "$work/pki" "$work/cfg" "$work/sec"
go run ./cmd/novactl federation init \
  --root "$work/pki" \
  --overlay-cidr 10.42.0.0/24 \
  --operator-overlay-ip 10.42.0.1 \
  --lighthouse-public 203.0.113.7:4242 \
  --hostname nova.bundle-health.test \
  --skip-preflight \
  --runtime-config-dir "$work/cfg" \
  --runtime-secrets-dir "$work/sec" >/dev/null

# A locally built image has no registry manifest digest, and `node invite`
# refuses a mutable tag. Its config digest is a syntactically valid pin derived
# from the exact bytes under test; the TUN-free override supplies the runnable
# local reference.
config_digest="$(docker image inspect --format '{{.Id}}' "$image" | cut -d: -f2)"
go run ./cmd/novactl node invite \
  --root "$work/pki" \
  --name bundle-health \
  --nebula-ip 10.42.0.10/24 \
  --image "${image%%:*}:bundle-health@sha256:${config_digest}" \
  --out "$work/bundle" \
  --operator-yaml "$work/absent-operator.yaml" \
  -i-know-what-im-doing >/dev/null

[ -f "$work/bundle/compose.yaml" ] || fail "node invite produced no compose.yaml"

step "2. the generated compose declares no healthcheck override"
if grep -qE '^[[:space:]]+healthcheck:' "$work/bundle/compose.yaml"; then
  grep -n -A4 -E '^[[:space:]]+healthcheck:' "$work/bundle/compose.yaml" >&2
  fail "the bundle overrides the probe; the image's own HEALTHCHECK is the single source"
fi
echo "ok: nova-node inherits the image's probe"

step "3. fixture workarounds (see this script's header)"
note "chmod: the container runs as uid 65532 and cannot read a 0700/0600 bundle"
chmod 0755 "$work/bundle/federation" "$work/bundle/nebula" "$work/bundle/secrets"
chmod 0644 "$work"/bundle/secrets/*
note "placeholders: node invite emits no nebula/nebula.crt or secrets/nebula_key"
printf 'PLACEHOLDER (bundle-health fixture): nova-node only stats this path.\n' \
  > "$work/bundle/nebula/nebula.crt"
printf 'PLACEHOLDER (bundle-health fixture): nova-node only stats this path.\n' \
  > "$work/bundle/secrets/nebula_key"
chmod 0644 "$work/bundle/nebula/nebula.crt" "$work/bundle/secrets/nebula_key"

step "4. start nova-node alone, without TUN"
cat > "$work/bundle/compose.tunfree.yaml" <<YAML
# TUN-free override for the hermetic tier. nova-node's health endpoint binds
# 127.0.0.1 and the probe runs inside this container, so leaving Nebula's
# namespace changes nothing about what is being tested.
services:
  nova-node:
    image: ${image}
    network_mode: "bridge"
YAML

compose=(docker compose -p "$project"
  -f "$work/bundle/compose.yaml" -f "$work/bundle/compose.tunfree.yaml")

"${compose[@]}" up -d --no-deps nova-node >/dev/null

cid="$("${compose[@]}" ps -q nova-node)"
[ -n "$cid" ] || fail "nova-node container was not created"

step "5. wait for Docker to report healthy"
# The image's probe is interval=30s retries=3, so allow for two intervals plus
# the container's own startup.
deadline=$((SECONDS + 150))
status=""
while [ "$SECONDS" -lt "$deadline" ]; do
  status="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$cid")"
  case "$status" in
    healthy) break ;;
    unhealthy) break ;;
  esac
  sleep 5
done

if [ "$status" != healthy ]; then
  echo "health status: ${status:-unknown}" >&2
  echo "--- probe output ---" >&2
  docker inspect -f '{{if .State.Health}}{{range .State.Health.Log}}exit={{.ExitCode}} out={{printf "%q" .Output}}{{"\n"}}{{end}}{{end}}' "$cid" >&2 || true
  echo "--- container log ---" >&2
  docker logs "$cid" 2>&1 | tail -20 >&2 || true
  fail "the generated donor bundle never became healthy"
fi

printf '\ndonor-bundle-health: PASS — the generated bundle reports healthy\n'
