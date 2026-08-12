#!/usr/bin/env bash
# donor-update.sh — the volunteer's update script (P2-M7.3, D-M7.3-15).
#
# Run it from inside this bundle directory:
#
#   ./donor-update.sh check      what would change. Changes nothing.
#   ./donor-update.sh apply      apply the refs in donor-lock.json
#   ./donor-update.sh rollback   go back, where the lock says that is safe
#
# ============================================================================
# What this script is NOT
# ============================================================================
#
# It holds no coordinator authority. It cannot drain your node, cannot approve
# a rollout, cannot talk to the coordinator's admin API and does not have the
# credentials to. Your operator drains you before you run this and undrains you
# after they have confirmed you came back healthy. That split is deliberate: a
# script on a volunteer's machine that could reconfigure the federation would be
# a federation with as many administrators as it has donors.
#
# It also never removes volumes. `docker compose down -v` on this topology
# deletes the Kubo blockstore holding your replicas and the node-data volume
# holding your registration — an update that can destroy the thing being
# updated is not an update. Nothing here passes -v, and a test enforces that.
#
# ============================================================================
# Why .env is written before the recreate
# ============================================================================
#
# Compose reads .env when it renders, which happens at `up`. Writing the new
# refs afterwards would recreate the containers on the OLD digest and then leave
# a file claiming the new one — an update that reports success and changed
# nothing. So: verify, persist, recreate, verify again.
set -euo pipefail

cd "$(cd "$(dirname "$0")" && pwd)"

LOCK="donor-lock.json"
ENV_FILE=".env"
PYTHON="${NOVA_DONOR_PYTHON:-python3}"
DOCKER="${NOVA_DONOR_DOCKER:-docker}"

die()  { printf 'donor-update: %s\n' "$*" >&2; exit 1; }
note() { printf 'donor-update: %s\n' "$*" >&2; }

usage() {
  cat >&2 <<'EOF'
usage: ./donor-update.sh <check|apply|rollback> [--expect-lock-digest sha256:...]

  check       print current vs. locked refs. Changes nothing.
  apply       write .env from donor-lock.json, then recreate the services.
  rollback    revert the components whose lock evidence says it is safe.

  --expect-lock-digest
              the digest your operator sent you. When given, the lock file is
              re-hashed and refused on any difference — which is the only thing
              that makes "the lock says so" mean anything.
EOF
  exit 2
}

action="${1:-}"
[ -n "$action" ] || usage
shift || true

expect_digest=""
while [ $# -gt 0 ]; do
  case "$1" in
    --expect-lock-digest) expect_digest="${2:-}"; shift 2 ;;
    -h|--help) usage ;;
    *) die "unknown option $1" ;;
  esac
done

case "$action" in check|apply|rollback) ;; *) die "unknown action \"$action\"" ;; esac
command -v "$PYTHON" >/dev/null 2>&1 || die "$PYTHON not found; it is needed to read $LOCK"
[ -f "$LOCK" ] || die "$LOCK is missing. This bundle predates the update contract; ask your
    operator for a converted bundle (they run: novactl node convert-bundle)."

# ---------------------------------------------------------------------------
# The lock's identity
# ---------------------------------------------------------------------------

if [ -n "$expect_digest" ]; then
  actual="$("$PYTHON" -c 'import hashlib,sys; print("sha256:"+hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$LOCK")"
  [ "$actual" = "$expect_digest" ] || die "$LOCK hashes to $actual, not the $expect_digest your
    operator sent. This is a different document; refusing."
  note "lock verified: $expect_digest"
else
  note "WARNING: no --expect-lock-digest. The refs below are whatever is in $LOCK, which is
    not the same as what your operator authorized. Ask them for the digest."
fi

# read_lock <field-path...> — prints one value per line.
lock_refs() {
  "$PYTHON" - "$LOCK" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
c = d["components"]
for name, var in (("nova-node", "NOVA_NODE_REF"),
                  ("nebula",    "NOVA_NEBULA_REF"),
                  ("kubo",      "NOVA_KUBO_REF")):
    print("%s=%s" % (var, c[name]["ref"]))
PY
}

lock_rollback() {
  "$PYTHON" - "$LOCK" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
c = d["components"]
for name, var in (("nova-node", "NOVA_NODE_REF"),
                  ("nebula",    "NOVA_NEBULA_REF"),
                  ("kubo",      "NOVA_KUBO_REF")):
    r = c[name].get("rollback", {})
    print("%s\t%s\t%s\t%s" % (name, var,
                              "safe" if r.get("safe") else "unsafe",
                              r.get("predecessor", "") or "-"))
PY
}

current_ref() { # $1=VAR
  if [ -f "$ENV_FILE" ]; then
    sed -n "s/^$1=//p" "$ENV_FILE" | tail -n1
  fi
}

# ---------------------------------------------------------------------------
# check
# ---------------------------------------------------------------------------

if [ "$action" = check ]; then
  printf '%-18s %-46s %s\n' "COMPONENT" "RUNNING" "LOCKED"
  while IFS='=' read -r var ref; do
    cur="$(current_ref "$var")"
    [ -n "$cur" ] || cur="(bundle default)"
    printf '%-18s %-46s %s\n' "$var" "$cur" "$ref"
  done < <(lock_refs)
  echo
  note "nothing was changed. Run 'apply' when your operator has drained you."
  exit 0
fi

# ---------------------------------------------------------------------------
# rollback — per component, and only where the evidence says so
# ---------------------------------------------------------------------------

if [ "$action" = rollback ]; then
  unsafe=""
  : > "$ENV_FILE.next"
  while IFS=$'\t' read -r name var verdict predecessor; do
    if [ "$verdict" = safe ] && [ "$predecessor" != "-" ]; then
      printf '%s=%s\n' "$var" "$predecessor" >> "$ENV_FILE.next"
    else
      unsafe="$unsafe $name"
    fi
  done < <(lock_rollback)

  if [ -n "$unsafe" ]; then
    rm -f "$ENV_FILE.next"
    report="rollback-blocked-$(date -u +%Y%m%dT%H%M%SZ).txt"
    {
      echo "Nova donor rollback BLOCKED at $(date -u +%Y-%m-%dT%H:%M:%SZ)"
      echo
      echo "These components have no evidence that going back is safe:$unsafe"
      echo
      lock_rollback
      echo
      echo "Nothing was changed. Your volumes, your $LOCK and your $ENV_FILE are"
      echo "all intact. Send this file to your operator."
      echo
      echo "Why this stops rather than trying: the Kubo repo holds your replicas and"
      echo "node-data holds your registration. Starting older software against state"
      echo "newer software already wrote can corrupt both, and a rollback that"
      echo "destroys the replicas it was protecting is worse than staying put."
    } > "$report"
    note "ROLLBACK BLOCKED — wrote $report"
    die "no component was reverted; nothing was changed"
  fi

  mv -f "$ENV_FILE.next" "$ENV_FILE"
  note "reverted to the evidenced predecessors; recreating"
  "$DOCKER" compose up -d
  exit 0
fi

# ---------------------------------------------------------------------------
# apply
# ---------------------------------------------------------------------------

# PERSIST FIRST. Written to a sibling temp file and renamed, so an interrupted
# write cannot leave a .env Compose would read as half a topology.
tmp="$ENV_FILE.next"
{
  echo "# Written by donor-update.sh from $LOCK. Do not edit by hand:"
  echo "# the next update overwrites this file, and a ref that is not in the"
  echo "# lock is a ref your operator did not authorize."
  lock_refs
} > "$tmp"
mv -f "$tmp" "$ENV_FILE"
note "persisted the locked refs to $ENV_FILE"

# RECREATE. No -v, ever: those volumes are the replicas and the registration.
"$DOCKER" compose up -d

# VERIFY. `up -d` returning zero means Compose was happy, not that the node
# came back.
note "waiting for nova-node to report healthy"
ok=0
for _ in $(seq 1 60); do
  if "$DOCKER" compose ps --format '{{.Service}} {{.Health}}' 2>/dev/null \
      | grep -q '^nova-node healthy$'; then
    ok=1
    break
  fi
  sleep 5
done
if [ "$ok" -ne 1 ]; then
  note "nova-node did not report healthy. Nothing was removed — your volumes and both"
  note "lock files are intact. Logs:"
  "$DOCKER" compose logs --tail 40 nova-node >&2 || true
  die "update applied but the node is not healthy; tell your operator BEFORE they undrain you"
fi

note "nova-node healthy on the locked refs"
note "tell your operator; they will confirm your assignments and undrain you"
