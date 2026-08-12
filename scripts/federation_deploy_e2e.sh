#!/usr/bin/env bash
# scripts/federation_deploy_e2e.sh — the P2-M7.2 clean-room acceptance test.
#
# Follows docs/quickstart/federation-operator.md VERBATIM. That is the point:
# any divergence between this script, the quickstart and docs/UPGRADING.md is a
# bug in one of the three, and this is what notices.
#
# Acceptance criterion (D-M7.2 exit): operator enables federation -> doctor
# passes -> operator emits a complete invite -> donor runs it unchanged ->
# operator sees a healthy, sourceable donor. One tested product path.
#
# Requires TUN and privileged networking, so it runs on the self-hosted /
# privileged release gate rather than on every PR. The cheap render, config and
# secret-leak checks run on every PR via `make artifact-gates`.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

# The dev overlay carries the build sections; the base and the federation
# overlay name released artifacts by digest (P2-M7.3, D-M7.3-14). Order
# matters: dev after base, federation last.
COMPOSE="docker compose -f docker/docker-compose.yml -f docker/docker-compose.dev.yml -f deploy/operator/compose.federation.yaml"
OVERLAY_CIDR="${OVERLAY_CIDR:-10.42.0.0/24}"
OPERATOR_IP="${OPERATOR_IP:-10.42.0.1}"
DONOR_IP="${DONOR_IP:-10.42.0.10/24}"
HOSTNAME_="${HOSTNAME_:-nova.e2e.test}"
LIGHTHOUSE="${LIGHTHOUSE:-127.0.0.1:4242}"
METRICS="${METRICS:-http://127.0.0.1:2112}"

step() { printf '\n=== %s ===\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }

require_tun() {
  [ -c /dev/net/tun ] || fail "/dev/net/tun is unavailable; this gate needs TUN"
}

cleanup() { $COMPOSE --profile prod --profile federation down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

require_tun

# --- 1. Base operator, no federation ---------------------------------------
step "1. boot the base operator stack"
$COMPOSE --profile prod up -d
sleep 5

# --- 2. federation init -----------------------------------------------------
step "2. federation init"
$COMPOSE --profile federation run --rm nova-admin \
  federation init \
    --overlay-cidr "$OVERLAY_CIDR" \
    --operator-overlay-ip "$OPERATOR_IP" \
    --lighthouse-public "$LIGHTHOUSE" \
    --hostname "$HOSTNAME_"

# --- 3. recreate with the overlay ------------------------------------------
# operator.yaml is read once at boot, so this recreate is mandatory, not
# incidental. See D-M7.2-8a.
step "3. recreate with the federation overlay"
$COMPOSE --profile prod --profile federation up -d

# --- 4. THE ORDERING REGRESSION GUARD ---------------------------------------
# The coordinator must be alive and serving while nebula1 is still absent.
# Before this milestone it exited instead, which deadlocked the very sidecar
# that creates the interface.
step "4. coordinator alive but federation NOT ready while nebula1 is absent"
alive=0
for _ in $(seq 1 30); do
  if curl -fsS "$METRICS/metrics" >/dev/null 2>&1; then alive=1; break; fi
  sleep 1
done
[ "$alive" -eq 1 ] || fail "coordinator did not come up at all"

ready_body="$(curl -sS "$METRICS/readyz" || true)"
if echo "$ready_body" | grep -q '"ready":true'; then
  echo "note: the overlay came up faster than the probe; ordering assertion skipped"
else
  echo "$ready_body" | grep -q '"ready":false' \
    || fail "expected federation ready=false while waiting for the interface"
  echo "ok: alive and serving, federation not ready (as designed)"
fi

# --- 5. the listener binds once the overlay appears -------------------------
step "5. federation listener binds after nebula1 appears"
bound=0
for _ in $(seq 1 60); do
  if curl -sS "$METRICS/readyz" | grep -q '"ready":true'; then bound=1; break; fi
  sleep 2
done
[ "$bound" -eq 1 ] || fail "federation listener never became ready"
curl -sS "$METRICS/metrics" | grep -q '^nova_federation_listener_ready 1' \
  || fail "nova_federation_listener_ready did not flip to 1"

# --- 6. all three doctor planes ---------------------------------------------
step "6. federation doctor (Planes A and B)"
$COMPOSE --profile federation run --rm nova-doctor federation doctor --live \
  || fail "doctor reported failures"

step "6c. compose custody policy (Plane C)"
make compose-custody-check || fail "custody policy violated"

# --- 7. issue one invite ----------------------------------------------------
step "7. node invite"
DIGEST="${NODE_IMAGE_DIGEST:-}"
[ -n "$DIGEST" ] || fail "set NODE_IMAGE_DIGEST to the nova-node image digest under test"

$COMPOSE --profile federation run --rm nova-admin \
  node invite \
    --name e2e-donor \
    --nebula-ip "$DONOR_IP" \
    --image "$DIGEST" \
    --out /invites/e2e-donor

bundle="deploy/operator/invites/e2e-donor"
[ -d "$bundle" ] || fail "invite bundle was not written to $bundle"

step "7b. the generated bundle is valid compose"
docker compose -f "$bundle/compose.yaml" config >/dev/null \
  || fail "generated compose.yaml is not valid"

step "7c. the bundle carries no operator secrets"
go run ./cmd/gen-deploy >/dev/null 2>&1 || true
go test ./internal/federation/bootstrap/ -run TestAssertBundleClean -count=1 >/dev/null \
  || fail "bundle secret assertion failed"

# --- 8. start the donor UNCHANGED -------------------------------------------
step "8. start the generated donor topology, unchanged"
(cd "$bundle" && docker compose up -d)

# --- 9/10. registration, immediate heartbeat, read-source without a restart --
step "9. donor registers and heartbeats immediately"
registered=0
for _ in $(seq 1 60); do
  if (cd "$bundle" && docker compose logs nova-node 2>/dev/null) | grep -q "nova-node registered"; then
    registered=1; break
  fi
  sleep 2
done
[ "$registered" -eq 1 ] || fail "donor never registered"

step "10. read-source starts WITHOUT a restart"
sourced=0
for _ in $(seq 1 30); do
  if (cd "$bundle" && docker compose logs nova-node 2>/dev/null) | grep -q "node.source.started"; then
    sourced=1; break
  fi
  sleep 2
done
if [ "$sourced" -ne 1 ]; then
  (cd "$bundle" && docker compose logs nova-node | tail -30)
  fail "read-source did not start in-process; a restart must not be required"
fi
(cd "$bundle" && docker compose logs nova-node 2>/dev/null) | grep -q "node.source.deferred" \
  && fail "read-source was deferred — the first-boot restart bug has regressed"

# --- 11. the coordinator sees a healthy donor -------------------------------
step "11. coordinator sees the donor"
$COMPOSE exec -T coordinator novactl node list | grep -q "e2e-donor" \
  || fail "coordinator does not list the donor"

# --- 12. drain and revoke cleanly -------------------------------------------
step "12. drain and revoke"
node_id="$($COMPOSE exec -T coordinator novactl node list | awk '/e2e-donor/{print $1}')"
[ -n "$node_id" ] || fail "could not determine the donor node id"
$COMPOSE exec -T coordinator novactl node drain --id "$node_id"
$COMPOSE exec -T coordinator novactl node revoke --id "$node_id" --no-confirm

(cd "$bundle" && docker compose down -v) || true

printf '\nfederation-deploy-e2e: PASS\n'
