#!/usr/bin/env bash
# P2-M7 (D-M7-3), repinned in P2-M7.3 (T22): mixed-version binary
# compatibility. Pairings:
#   head-head | head-coord-old-donor | old-coord-head-donor | all
# EVERY pairing proves join → serve → audit against REAL binaries: register
# over loopback federation mTLS, replicate one uploaded blob to the donor,
# then a hash-verified serve proof — the coordinator's local Kubo repo is
# wiped and the blob is re-read THROUGH the coordinator read path, forcing a
# donor-backed fetch (an ack is DB choreography; this proves bytes).
# Drain/decommission is exercised ONLY when the coordinator is HEAD (an N−1
# coordinator has no drain).
#
# NOT a schema-downgrade test: each pairing gets a FRESH database migrated by
# the coordinator side's OWN migrate binary, so each side reaches its own schema
# ceiling and neither is asked to run against the other's. Schema-upgrade
# coverage lives in internal/db/migrations and internal/upgrade tests, and the
# old-binary-on-a-forward-schema case is upgrade-schema-e2e (Task 25) — not
# here. Naming the two ceilings in a comment was how this file went stale: it
# said "HEAD → 0016; N−1 → 0015" three milestones after both moved.
#
# The predecessor comes from the RELEASE INTENT, not from a shell variable.
# `PRIOR_TAG="${PRIOR_TAG:-p2-m6-possession-audits}"` was wrong twice: a default
# nobody updates goes stale silently, and the first supported predecessor is
# COMMIT-anchored — the deployment in the field is a local build of a commit
# with no product tag — so a variable that can only hold a tag cannot name it.
#
# Requirements: docker (postgres + kubo sidecars), Go toolchain + libvips dev
# headers (host coordinator build), free loopback ports 15544/15001/19000/
# 19443/19555/19100. Local gate (make crossversion-e2e), never a per-push job.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# The predecessor ref, read from the reviewed intent (P2-M7.3, T22). Overridable
# for a one-off drill against some other artifact, but never defaulted to a
# hand-maintained constant.
PREDECESSOR="${PREDECESSOR:-$(go run ./internal/release/cmd/novarel predecessor)}"
[ -n "$PREDECESSOR" ] || { echo "[xv] could not read the predecessor from the release intent" >&2; exit 1; }
PAIRING="${1:-all}"

# Sidecars are pinned BY DIGEST. This milestone is about knowing which bytes
# produced a result, and a gate whose Postgres or Kubo changes underneath it
# reports a pass about software nobody can name.
#
# Kubo is read from internal/deploy/templates.go so the gate and the donor
# bundles cannot disagree about which Kubo Nova supports.
KUBO_IMAGE="$(sed -n 's/.*DefaultKuboImage *= *"\(.*\)".*/\1/p' internal/deploy/templates.go)"
[ -n "$KUBO_IMAGE" ] || { echo "[xv] could not read DefaultKuboImage from internal/deploy/templates.go" >&2; exit 1; }
# postgres:16-alpine as resolved 2026-08-11. Its twins live in
# internal/upgrade/apply_test.go and internal/db/migrations/upgrade_runs_test.go.
PG_IMAGE="postgres:16-alpine@sha256:57c72fd2a128e416c7fcc499958864df5301e940bca0a56f58fddf30ffc07777"

PG_PORT=15544
KUBO_PORT=15001
API_PORT=19000
FED_PORT=19443
SRC_PORT=19555
HEALTH_PORT=19100

WORK="$(mktemp -d)"
COORD_PID=""
DONOR_PID=""

log() { echo "[xv] $*"; }

fail() {
    echo "[xv] FAIL: $1" >&2
    echo "[xv] ─── coordinator log tail ───" >&2; tail -30 "$WORK/coordinator.log" >&2 2>/dev/null || true
    echo "[xv] ─── donor log tail ───" >&2; tail -30 "$WORK/donor.log" >&2 2>/dev/null || true
    exit 1
}

cleanup() {
    [ -n "$DONOR_PID" ] && kill "$DONOR_PID" 2>/dev/null || true
    [ -n "$COORD_PID" ] && kill "$COORD_PID" 2>/dev/null || true
    docker rm -f xv-pg xv-kubo >/dev/null 2>&1 || true
    git worktree remove --force "$WORK/old" 2>/dev/null || true
    rm -rf "$WORK"
}
trap cleanup EXIT

# The HEAD side is the candidate, and in a release run the candidate is the
# pushed image rather than a rebuild of the same tree. The PREDECESSOR side is
# always built from source: it has no published image, which is exactly why the
# baseline is commit-anchored in the intent.
. "$ROOT/scripts/lib/candidate.sh"

build_side() { # $1=dir $2=outprefix — coordinator, node, novactl, migrate
    if [ "$2" = head ] && [ -n "${NOVA_CANDIDATE_DESCRIPTORS:-}" ]; then
        log "resolving the candidate for $2"
        nova_candidate_bin coordinator "$WORK/bin/$2-coordinator" &&
        nova_candidate_bin node        "$WORK/bin/$2-node" &&
        nova_candidate_bin novactl     "$WORK/bin/$2-novactl" &&
        nova_candidate_bin migrate     "$WORK/bin/$2-migrate" || return 1
        log "candidate: $NOVA_CANDIDATE_SOURCE"
        return 0
    fi
    log "building $2 binaries from $1"
    (cd "$1" && go build -o "$WORK/bin/$2-coordinator" ./cmd/coordinator \
              && go build -o "$WORK/bin/$2-node"        ./cmd/node \
              && go build -o "$WORK/bin/$2-novactl"     ./cmd/novactl \
              && go build -o "$WORK/bin/$2-migrate"     ./cmd/migrate)
}

DSN="postgres://postgres:nova@127.0.0.1:$PG_PORT/nova_scratch?sslmode=disable"

psqlq() { docker exec xv-pg psql -U postgres -d nova_scratch -tAc "$1"; }

wait_sql() { # $1=query $2=want $3=label $4=max-tries(2s each)
    local n=0 max="${4:-60}" got=""
    while [ "$n" -lt "$max" ]; do
        got="$(psqlq "$1" 2>/dev/null | head -n1 | tr -d '[:space:]' || true)"
        [ "$got" = "$2" ] && return 0
        n=$((n + 1))
        sleep 2
    done
    log "wait_sql '$3' timed out (last: '$got', want '$2')"
    return 1
}

start_pg() {
    docker run -d --name xv-pg -e POSTGRES_PASSWORD=nova -e POSTGRES_DB=nova_scratch \
        -p "127.0.0.1:$PG_PORT:5432" "$PG_IMAGE" >/dev/null
    local n=0
    until docker exec xv-pg pg_isready -U postgres -d nova_scratch >/dev/null 2>&1; do
        n=$((n + 1)); [ "$n" -gt 30 ] && fail "postgres not ready"
        sleep 1
    done
}

start_kubo() {
    docker run -d --name xv-kubo -p "127.0.0.1:$KUBO_PORT:5001" "$KUBO_IMAGE" >/dev/null
    local n=0
    until curl -sf -X POST "http://127.0.0.1:$KUBO_PORT/api/v0/id" >/dev/null 2>&1; do
        n=$((n + 1)); [ "$n" -gt 45 ] && fail "kubo API not ready"
        sleep 2
    done
}

# write_configs generates all key material + operator.yaml + node.yaml against
# the REAL config schemas. Only fields BOTH versions know are written (P2-M7's
# metrics_listen_addr is passed as env, which the N−1 binary simply ignores).
write_configs() { # $1=coord-side $2=donor-side
    local cnovactl="$WORK/bin/$1-novactl"

    # Federation CA + donor bundle + coordinator client identity (PEM material
    # is version-independent; issued by the coordinator side's own novactl).
    "$cnovactl" node ca-init --dir "$WORK/ca" --coordinator-ip 127.0.0.1 --coordinator-dns localhost >/dev/null
    "$cnovactl" node issue --dir "$WORK/ca" --name xv-donor --out "$WORK/donor" >/dev/null
    "$cnovactl" node issue-coordinator-client --dir "$WORK/ca" --out "$WORK/coordinator-client" >/dev/null
    XV_NODE="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["node_id"])' "$WORK/donor/node-manifest.json")"

    # Secrets: master key (32B hex), OIDC signing seed (32B hex), repair
    # signing seed (32B hex), private-swarm PSK.
    MASTER_KEY_HEX="$(openssl rand -hex 32)"
    OIDC_KEY_HEX="$(openssl rand -hex 32)"
    REPAIR_KEY_HEX="$(openssl rand -hex 32)"
    printf '/key/swarm/psk/1.0.0/\n/base16/\n%s\n' "$(openssl rand -hex 32)" > "$WORK/swarm.key"

    # The donor's config validator requires readable nebula cert/key files;
    # loopback needs no real overlay, so self-signed placeholders suffice.
    openssl req -x509 -newkey ed25519 -keyout "$WORK/donor/nebula.key" -out "$WORK/donor/nebula.crt" \
        -days 1 -nodes -subj "/CN=xv-loopback" >/dev/null 2>&1

    mkdir -p "$WORK/etc-nova" "$WORK/kubo-repo" "$WORK/donor-store"
    touch "$WORK/etc-nova/.bootstrap-complete"

    cat > "$WORK/operator.yaml" <<EOF
operator:
  hostname: xv.nova.test
  contact_email: xv@example.invalid
tls:
  mode: dev-self-signed
auth:
  issuer_url: ""
  paranoid: false
uploads:
  public_uploads: true
tos_url: https://xv.nova.test/tos
orchestrator:
  tick_interval_seconds: 2
  step_seconds: 2
  replication:
    factor:
      important: 2
      normal: 2
      cache: 2
  mass_casualty_threshold_ratio: 0.5
  mass_casualty_window_seconds: 3600
  capacity_runway_floor_days: 7
moderation:
  takedown_default_action: quarantine
  dmca_counter_notification_days: 14
coordinator:
  public_ipfs_dht: false
federation:
  listen_addr: "127.0.0.1:$FED_PORT"
  federation_ca_path: $WORK/ca/federation-ca.crt
  federation_cert_path: $WORK/ca/coordinator-federation.crt
  federation_key_path: $WORK/ca/coordinator-federation.key
  federation_client_cert_path: $WORK/coordinator-client/federation-client.crt
  federation_client_key_path: $WORK/coordinator-client/federation-client.key
  heartbeat_interval_seconds: 5
  pins_poll_interval_seconds: 2
  max_pin_concurrency: 4
  suspect_after_missed_heartbeats: 3
  unreachable_after_seconds: 3600
  evicted_after_seconds: 2592000
  repair_token_ttl_seconds: 300
possession_audit:
  base_interval_seconds: 3
  deadline_seconds: 10
EOF

    cat > "$WORK/node.yaml" <<EOF
coordinator_url: "https://127.0.0.1:$FED_PORT"
federation_ca_path: $WORK/donor/federation-ca.crt
federation_cert_path: $WORK/donor/federation.crt
federation_key_path: $WORK/donor/federation.key
nebula_cert_path: $WORK/donor/nebula.crt
nebula_key_path: $WORK/donor/nebula.key
swarm_key_path: $WORK/swarm.key
storage_dir: $WORK/donor-store
bandwidth_budget_bytes_per_day: 1073741824
health_listen_addr: "127.0.0.1:$HEALTH_PORT"
kubo_api_addr: "http://127.0.0.1:$KUBO_PORT"
source_nebula_addr: "127.0.0.1:$SRC_PORT"
source_read_listen_addr: "127.0.0.1:$SRC_PORT"
EOF
}

# start_coordinator: cmd/coordinator has NO --config flag — operator.yaml via
# NOVA_CONFIG_FILE; every env-only secret is set explicitly. The repair
# signing key + federation client key ride the documented env secret chains.
start_coordinator() { # $1=side
    NOVA_CONFIG_FILE="$WORK/operator.yaml" \
    NOVA_CONFIG_DIR="$WORK/etc-nova" \
    DATABASE_URL="$DSN" \
    NOVA_KUBO_REPO="$WORK/kubo-repo" \
    IPFS_SWARM_KEY_FILE="$WORK/swarm.key" \
    NOVA_LISTEN_ADDR="127.0.0.1:$API_PORT" \
    NOVA_MASTER_KEY_ACTIVE=v1 \
    NOVA_MASTER_KEY_V1="$MASTER_KEY_HEX" \
    NOVA_OIDC_SIGNING_KEY="$OIDC_KEY_HEX" \
    NOVA_FEDERATION_REPAIR_SIGNING_KEY="$REPAIR_KEY_HEX" \
    NOVA_METRICS_LISTEN_ADDR="" \
        "$WORK/bin/$1-coordinator" >> "$WORK/coordinator.log" 2>&1 &
    COORD_PID=$!

    local n=0
    until curl -sf "http://127.0.0.1:$API_PORT/health" >/dev/null 2>&1; do
        n=$((n + 1))
        kill -0 "$COORD_PID" 2>/dev/null || fail "$1-coordinator exited during startup"
        [ "$n" -gt 60 ] && fail "$1-coordinator /health not up in 120s"
        sleep 2
    done
}

stop_coordinator() {
    [ -n "$COORD_PID" ] && kill "$COORD_PID" 2>/dev/null || true
    wait "$COORD_PID" 2>/dev/null || true
    COORD_PID=""
}

seed_fixture() {
    python3 - "$WORK/fixture.png" <<'PYEOF'
import struct, sys, zlib
def chunk(t, d):
    return struct.pack(">I", len(d)) + t + d + struct.pack(">I", zlib.crc32(t + d) & 0xffffffff)
w = h = 32
raw = b"".join(b"\x00" + b"\x1f\x8b\x2d" * w for _ in range(h))
png = (b"\x89PNG\r\n\x1a\n"
       + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0))
       + chunk(b"IDAT", zlib.compress(raw))
       + chunk(b"IEND", b""))
open(sys.argv[1], "wb").write(png)
PYEOF
    XV_SHA256="$(sha256sum "$WORK/fixture.png" | cut -d' ' -f1)"
}

# seed_one_blob: a REAL readable object through the production write path —
# anonymous multipart upload into a seeded public collection (visibility floor:
# no membership ⇒ private ⇒ anonymous read 401s).
seed_one_blob() {
    COL_ID="$(psqlq "WITH u AS (
                       INSERT INTO users (email) VALUES ('xv@example.invalid')
                       ON CONFLICT (email) DO UPDATE SET updated_at = now()
                       RETURNING id)
                     INSERT INTO collections (owner_id, name, slug, visibility, public_archival)
                     SELECT id, 'xv-public', 'xv-public', 'public', false FROM u
                     RETURNING id;" | head -n1 | tr -d '[:space:]')"
    [ -n "$COL_ID" ] || fail "collection seed returned empty id"
    log "seeded public collection $COL_ID"

    local code
    code="$(curl -sS -o "$WORK/upload.json" -w '%{http_code}' \
        -F "file=@$WORK/fixture.png;type=image/png;filename=fixture.png" \
        -F "product=image" \
        -F "collection_id=$COL_ID" \
        "http://127.0.0.1:$API_PORT/api/v1/blobs")" || fail "upload request failed"
    [ "$code" = "201" ] || { cat "$WORK/upload.json" >&2 || true; fail "upload returned $code (want 201)"; }
    XV_CID="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["cid"])' "$WORK/upload.json")"
    [ -n "$XV_CID" ] || fail "upload returned empty cid"
    log "seeded blob cid=$XV_CID sha256=$XV_SHA256"
}

# serve_proof: fetch through the coordinator read path and hash-verify. For
# the donor-backed case the coordinator's local Kubo repo was wiped first, so
# the bytes MUST come from the donor's read-source endpoint — which can only
# verify read grants after it captures the coordinator's repair pubkey from
# the donor's FIRST heartbeat (hardcoded 300s initial cadence), hence the long
# retry window.
serve_proof() { # $1=label $2=max-tries(2s each)
    local max="${2:-1}" n=0 code="" got=""
    while [ "$n" -lt "$max" ]; do
        code="$(curl -sS -o "$WORK/served.png" -w '%{http_code}' "http://127.0.0.1:$API_PORT/blob/$XV_CID?nocache=$RANDOM" 2>/dev/null || echo 000)"
        if [ "$code" = "200" ]; then
            got="$(sha256sum "$WORK/served.png" | cut -d' ' -f1)"
            [ "$got" = "$XV_SHA256" ] || fail "serve_proof ($1): bytes hash $got != seeded $XV_SHA256"
            log "serve_proof ($1): hash-verified"
            return 0
        fi
        n=$((n + 1))
        sleep 2
    done
    fail "serve_proof ($1): GET /blob/$XV_CID never returned 200 (last code $code)"
}

run_pairing() { # $1=coord-side (head|old) $2=donor-side (head|old)
    log "=== pairing: $1-coordinator × $2-donor ==="
    rm -rf "$WORK/etc-nova" "$WORK/kubo-repo" "$WORK/donor-store" "$WORK/ca" "$WORK/donor" "$WORK/coordinator-client"
    : > "$WORK/coordinator.log"; : > "$WORK/donor.log"
    start_pg
    start_kubo
    # The coordinator side's OWN schema ceiling. HEAD's migrate journals every
    # apply (P2-M7.3, D-M7.3-9b) and its default location is a container volume,
    # so the gate points it at the scratch directory.
    DATABASE_URL="$DSN" NOVA_UPGRADE_JOURNAL_DIR="$WORK/upgrade-journal" \
        "$WORK/bin/$1-migrate" up >/dev/null || fail "$1-migrate up failed"
    write_configs "$1" "$2"
    seed_fixture
    start_coordinator "$1"
    seed_one_blob
    serve_proof "local-origin" 3

    "$WORK/bin/$2-node" --config "$WORK/node.yaml" >> "$WORK/donor.log" 2>&1 &
    DONOR_PID=$!

    # join: registered, active, and sync-current.
    wait_sql "SELECT count(*) FROM nodes WHERE status='active' AND assignment_sync_state='current'" 1 "donor join" 45 \
        || fail "donor never reached active/current"

    # Assign the pin, THEN restart the donor. TWO donor realities drive this
    # (both documented, both version-stable):
    #   1. a donor's read-source server only starts on a boot that already has
    #      a persisted registration ("fail-closed by construction"), and
    #   2. a fresh donor's initial cadences are hardcoded (heartbeat 300s,
    #      pins-poll 600s) until the first heartbeat delivers config_updates —
    #      but a booting agent syncs IMMEDIATELY, so restarting after the
    #      assign makes the transfer happen within seconds.
    DATABASE_URL="$DSN" "$WORK/bin/$1-novactl" pin assign --cid "$XV_CID" --node "$XV_NODE" >/dev/null \
        || fail "pin assign failed"
    kill "$DONOR_PID" 2>/dev/null || true; wait "$DONOR_PID" 2>/dev/null || true
    "$WORK/bin/$2-node" --config "$WORK/node.yaml" >> "$WORK/donor.log" 2>&1 &
    DONOR_PID=$!
    wait_sql "SELECT count(*) FROM pin_assignments WHERE state='acked'" 1 "pin ack" 60 \
        || fail "donor never acked the pin"

    # audit: short cadence → at least one decided pass against the donor's now-
    # running source server.
    #
    # The p2-m6-era caveat that lived here — that the N−1 coordinator's audit
    # "pass" rows were fabricated without reaching the donor — described the
    # P2-M6 binary. The predecessor is now 143c459, which contains that fix, and
    # the 2026-08-11 run recorded a decided pass in all three pairings.
    wait_sql "SELECT count(*) > 0 FROM pin_audits WHERE result='pass'" t "audit pass" 90 \
        || fail "no passing possession audit recorded"

    # serve (donor-backed): wipe the coordinator's local Kubo repo and re-read
    # THROUGH the coordinator — the bytes must now come from the donor
    # (hash-verified). The retry window covers the donor's first-heartbeat
    # pubkey capture (~300s).
    #
    # HEAD-coordinator only, and the reason is now a gate limitation rather than
    # a defect claim. The old comment said the N−1 coordinator could never
    # accept a real donor serving cert; that was the P2-M6 binary, and 143c459
    # contains the TLS fix. This gate simply does not exercise donor-backed
    # reads on the baseline side, and the coverage table says "untested here,
    # not known broken" rather than guessing.
    if [ "$1" = head ]; then
        stop_coordinator
        rm -rf "$WORK/kubo-repo"; mkdir -p "$WORK/kubo-repo"
        start_coordinator "$1"
        serve_proof "donor-backed" 200
    fi

    # drain: HEAD coordinator ONLY (an N−1 coordinator has no drain).
    if [ "$1" = head ]; then
        DATABASE_URL="$DSN" "$WORK/bin/head-novactl" node drain --id "$XV_NODE" --no-confirm >/dev/null \
            || fail "novactl node drain failed"
        wait_sql "SELECT count(*) FROM nodes WHERE draining_at IS NOT NULL" 1 "drain marker" 5 \
            || fail "draining_at not set"
        DATABASE_URL="$DSN" "$WORK/bin/head-novactl" node undrain --id "$XV_NODE" >/dev/null \
            || fail "novactl node undrain failed"
    fi

    kill "$DONOR_PID" 2>/dev/null || true; wait "$DONOR_PID" 2>/dev/null || true; DONOR_PID=""
    stop_coordinator
    docker rm -f xv-pg xv-kubo >/dev/null
    log "=== pairing $1 × $2 PASSED ==="
}

mkdir -p "$WORK/bin"
build_side . head
if [ "$PAIRING" != "head-head" ]; then
    log "adding worktree for predecessor $PREDECESSOR"
    git worktree add --force --detach "$WORK/old" "$PREDECESSOR" >/dev/null \
        || fail "cannot check out predecessor $PREDECESSOR — it is named by the release intent, so either the intent is wrong or this clone is shallow"
    build_side "$WORK/old" old
fi

case "$PAIRING" in
  head-head)            run_pairing head head ;;
  head-coord-old-donor) run_pairing head old ;;
  old-coord-head-donor) run_pairing old head ;;
  all)                  run_pairing head head; run_pairing head old; run_pairing old head ;;
  *) echo "usage: $0 [head-head|head-coord-old-donor|old-coord-head-donor|all]" >&2; exit 2 ;;
esac

echo "OK: crossversion pairing(s) $PAIRING passed"
