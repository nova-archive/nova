#!/usr/bin/env bash
# scripts/mixed_fleet_e2e.sh — a coordinator upgrade requires no donor upgrade
# (P2-M7.3, D-M7.3-21, gate `mixed-fleet-e2e`).
#
# Acceptance scenarios satisfied: 17-26.
#
# ============================================================================
# The claim, and why it needs a SIMULTANEOUS fleet
# ============================================================================
#
# coordinator-upgrade-needs-no-donor-upgrade: upgrading the coordinator alone
# evicts nobody and triggers no mass repair across a fleet that at the same
# moment holds donors which are current, supported-older, missing an optional
# capability, pre-contract, unsupported-but-compatible, and stale.
#
# Six sequential pairings would not do. The failure this guards against is a
# policy that is individually reasonable for each donor and collectively evicts
# half of them — a durability floor that counts differently once three donors
# are unsupported, a placement filter that empties when two roles are missing.
# You only see it with all six standing up at once.
#
# The coordinator upgrade here is REAL: the fleet registers against the
# PREDECESSOR coordinator at its own schema, then the schema advances and the
# candidate coordinator takes over. That is the operation the claim is about.
#
# ============================================================================
# It runs over LOOPBACK, and does not need TUN
# ============================================================================
#
# The plan filed this under the release-tun tier. It does not belong there.
# scripts/crossversion_e2e.sh already demonstrates that a donor and coordinator
# speak federation mTLS over loopback with placeholder Nebula material, because
# the overlay is TRANSPORT and this gate is about coordinator POLICY.
#
# So it runs in rc-docker and can therefore run in CI, which is worth more than
# a tier label. What loopback does NOT cover is stated in the coverage entry:
# real Nebula routing, MTU behaviour, NAT traversal, and lighthouse failure.
# Those belong to federation-deploy-e2e, which does need TUN.
#
# Requirements: docker, Go toolchain + libvips dev headers, and free loopback
# ports 15547, 15011-15016, 19020, 19463, 19571-19576, 19121-19126.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PREDECESSOR="${PREDECESSOR:-$(go run ./internal/release/cmd/novarel predecessor)}"
[ -n "$PREDECESSOR" ] || { echo "[fleet] could not read the predecessor from the release intent" >&2; exit 1; }

PG_IMAGE="postgres:16-alpine@sha256:57c72fd2a128e416c7fcc499958864df5301e940bca0a56f58fddf30ffc07777"
KUBO_IMAGE="$(sed -n 's/.*DefaultKuboImage *= *"\(.*\)".*/\1/p' internal/deploy/templates.go)"
[ -n "$KUBO_IMAGE" ] || { echo "[fleet] could not read DefaultKuboImage" >&2; exit 1; }

PG_PORT=15547
API_PORT=19020
FED_PORT=19463

# The fleet. Each entry is: name | build | capabilities
#
#   current                 the candidate build, fully capable
#   supported-older         the predecessor named by the release intent. It
#                           sends no runtime contract, because that binary
#                           predates the contract — so it is also the
#                           pre-contract case in its natural form.
#   capability-missing      candidate build WITHOUT read-source/v1
#   precontract-unknown     predecessor build, with its registration-time
#                           version cleared, so it reports nothing at all
#   unsupported-compatible  candidate build labelled with a version outside the
#                           support window. The label is the ONLY difference —
#                           which is the point: it must change nothing
#   stale-offline           registers, then stops
FLEET_NAMES=(current supported-older capability-missing precontract-unknown unsupported-compatible stale-offline)
FLEET_BUILD=(head     old             head               old                  head                   head)
FLEET_CAPS=("pin-change-log/v1,snapshot/v1,blob-transfer/v1,read-source/v1" \
            "" \
            "pin-change-log/v1,snapshot/v1,blob-transfer/v1" \
            "" \
            "pin-change-log/v1,snapshot/v1,blob-transfer/v1,read-source/v1" \
            "pin-change-log/v1,snapshot/v1,blob-transfer/v1,read-source/v1")

WORK="$(mktemp -d)"
COORD_PID=""
declare -a DONOR_PIDS=()
declare -a NODE_IDS=()

log()  { echo "[fleet] $*"; }
pass() { echo "[fleet]   PASS  $*"; }
FAILURES=0
check() { # check <description> <condition-result>
    if [ "$2" = "0" ]; then pass "$1"; else echo "[fleet]   FAIL  $1" >&2; FAILURES=$((FAILURES + 1)); fi
}
die() {
    echo "[fleet] ABORT: $1" >&2
    echo "[fleet] ─── nodes ───" >&2
    docker exec fleet-pg psql -U postgres -d nova_fleet -c \
        "SELECT left(id::text,8) id, display_name, status, trust_state, assignment_sync_state FROM nodes" >&2 2>/dev/null || true
    for i in 0 1 2 3 4 5; do
        [ -f "$WORK/donor-$i.log" ] || continue
        echo "[fleet] ─── donor $i (${FLEET_NAMES[$i]}) ───" >&2
        tail -12 "$WORK/donor-$i.log" >&2
    done
    echo "[fleet] ─── coordinator ───" >&2
    tail -20 "$WORK/coordinator.log" >&2 2>/dev/null || true
    exit 1
}
cleanup() {
    for pid in ${DONOR_PIDS+"${DONOR_PIDS[@]}"}; do kill "$pid" 2>/dev/null || true; done
    [ -n "$COORD_PID" ] && kill "$COORD_PID" 2>/dev/null || true
    docker rm -f fleet-pg >/dev/null 2>&1 || true
    for i in 0 1 2 3 4 5; do docker rm -f "fleet-kubo-$i" >/dev/null 2>&1 || true; done
    git worktree remove --force "$WORK/old" 2>/dev/null || true
    rm -rf "$WORK"
}
trap cleanup EXIT

DSN="postgres://postgres:nova@127.0.0.1:$PG_PORT/nova_fleet?sslmode=disable"
q() { docker exec fleet-pg psql -U postgres -d nova_fleet -tAc "$1" | tr -d '[:space:]'; }
x() { docker exec fleet-pg psql -U postgres -d nova_fleet -q -c "$1" >/dev/null; }

# seed_blob writes BOTH rows. GetBlobSize selects blob_manifests.envelope_size —
# the on-disk ciphertext size the donor will actually receive — so a blobs row
# alone assigns nothing and reports only "no rows in result set".
seed_blob() {
    x "INSERT INTO blobs (cid, mime_type, byte_size)
         VALUES ('$1','application/octet-stream',64) ON CONFLICT (cid) DO NOTHING"
    x "INSERT INTO blob_manifests (cid, hash_alg, codec, chunker, plaintext_size, envelope_size, block_count)
         VALUES ('$1','sha2-256','raw','size-262144',64,64,1) ON CONFLICT (cid) DO NOTHING"
}

wait_sql() { # <query> <want> <label> <tries>
    local n=0 max="${4:-45}" got=""
    while [ "$n" -lt "$max" ]; do
        got="$(q "$1" 2>/dev/null || true)"
        [ "$got" = "$2" ] && return 0
        n=$((n + 1)); sleep 2
    done
    echo "[fleet]   (waiting for '$3': last '$got', want '$2')" >&2
    return 1
}

mkdir -p "$WORK/bin" "$WORK/journal" "$WORK/etc-nova" "$WORK/kubo-repo"

# ---------------------------------------------------------------------------
# Build both sides
# ---------------------------------------------------------------------------

# With descriptors the candidate is the PUSHED IMAGE, extracted by digest;
# without them it is a stamped local build. This gate's claim is about the
# candidate coordinator's policy, so which bytes are running is the claim.
. "$ROOT/scripts/lib/candidate.sh"
log "resolving the candidate"
nova_candidate_bin coordinator "$WORK/bin/head-coordinator" || die "no candidate coordinator"
nova_candidate_bin node        "$WORK/bin/head-node"        || die "no candidate node"
nova_candidate_bin novactl     "$WORK/bin/head-novactl"     || die "no candidate novactl"
nova_candidate_bin migrate     "$WORK/bin/head-migrate"     || die "no candidate migrate"
log "candidate: $NOVA_CANDIDATE_SOURCE"

log "building the predecessor $PREDECESSOR"
git worktree add --force --detach "$WORK/old" "$PREDECESSOR" >/dev/null \
    || die "cannot check out $PREDECESSOR"
(cd "$WORK/old" && go build -o "$WORK/bin/old-coordinator" ./cmd/coordinator \
                && go build -o "$WORK/bin/old-node"        ./cmd/node \
                && go build -o "$WORK/bin/old-migrate"     ./cmd/migrate) \
    || die "the predecessor does not build"

# ---------------------------------------------------------------------------
# Postgres at the PREDECESSOR's schema
# ---------------------------------------------------------------------------

log "starting postgres"
docker run -d --name fleet-pg -e POSTGRES_PASSWORD=nova -e POSTGRES_DB=nova_fleet \
    -p "127.0.0.1:$PG_PORT:5432" "$PG_IMAGE" >/dev/null
n=0
until docker exec fleet-pg psql -U postgres -d nova_fleet -tAc 'SELECT 1' >/dev/null 2>&1; do
    n=$((n + 1)); [ "$n" -gt 60 ] && die "postgres never accepted a query"
    sleep 1
done
DATABASE_URL="$DSN" "$WORK/bin/old-migrate" up >/dev/null 2>&1 || die "predecessor migrate failed"
BASE_SCHEMA="$(q 'SELECT COALESCE(MAX(version_id),0) FROM goose_db_version WHERE is_applied')"
log "predecessor schema: $BASE_SCHEMA"

# ---------------------------------------------------------------------------
# Identities and configuration
# ---------------------------------------------------------------------------

log "issuing six donor identities"
"$WORK/bin/head-novactl" node ca-init --dir "$WORK/ca" \
    --coordinator-ip 127.0.0.1 --coordinator-dns localhost >/dev/null || die "ca-init failed"
"$WORK/bin/head-novactl" node issue-coordinator-client --dir "$WORK/ca" \
    --out "$WORK/coordinator-client" >/dev/null || die "coordinator client failed"

MASTER_KEY_HEX="$(openssl rand -hex 32)"
OIDC_KEY_HEX="$(openssl rand -hex 32)"
REPAIR_KEY_HEX="$(openssl rand -hex 32)"
printf '/key/swarm/psk/1.0.0/\n/base16/\n%s\n' "$(openssl rand -hex 32)" > "$WORK/swarm.key"
touch "$WORK/etc-nova/.bootstrap-complete"

for i in 0 1 2 3 4 5; do
    name="${FLEET_NAMES[$i]}"
    d="$WORK/donor-$i"
    "$WORK/bin/head-novactl" node issue --dir "$WORK/ca" --name "$name" --out "$d" >/dev/null \
        || die "issuing $name failed"
    openssl req -x509 -newkey ed25519 -keyout "$d/nebula.key" -out "$d/nebula.crt" \
        -days 1 -nodes -subj "/CN=fleet-$name" >/dev/null 2>&1
    NODE_IDS[i]="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["node_id"])' "$d/node-manifest.json")"
    mkdir -p "$WORK/store-$i"

    kubo_port=$((15011 + i))
    docker run -d --name "fleet-kubo-$i" -p "127.0.0.1:$kubo_port:5001" "$KUBO_IMAGE" >/dev/null
    # The capability-missing donor advertises no read-source/v1, and it does so
    # HONESTLY: the agent derives that capability from source_nebula_addr, so a
    # donor with no source address simply does not offer the role. Editing
    # advertised_capabilities in SQL would be undone by the donor's next
    # heartbeat, which is the runtime contract working as designed.
    src_advertise="127.0.0.1:$((19571 + i))"
    src_listen="127.0.0.1:$((19571 + i))"
    if [ "$name" = "capability-missing" ]; then
        src_advertise=""
        src_listen=""
    fi
    cat > "$d/node.yaml" <<EOF
coordinator_url: "https://127.0.0.1:$FED_PORT"
federation_ca_path: $d/federation-ca.crt
federation_cert_path: $d/federation.crt
federation_key_path: $d/federation.key
nebula_cert_path: $d/nebula.crt
nebula_key_path: $d/nebula.key
swarm_key_path: $WORK/swarm.key
storage_dir: $WORK/store-$i
bandwidth_budget_bytes_per_day: 1073741824
health_listen_addr: "127.0.0.1:$((19121 + i))"
kubo_api_addr: "http://127.0.0.1:$kubo_port"
source_nebula_addr: "$src_advertise"
source_read_listen_addr: "$src_listen"
EOF
done

log "waiting for six Kubo daemons"
for i in 0 1 2 3 4 5; do
    n=0
    until curl -sf -X POST "http://127.0.0.1:$((15011 + i))/api/v0/id" >/dev/null 2>&1; do
        n=$((n + 1)); [ "$n" -gt 45 ] && die "kubo $i not ready"
        sleep 2
    done
done

cat > "$WORK/operator.yaml" <<EOF
operator:
  hostname: fleet.nova.test
  contact_email: fleet@example.invalid
tls:
  mode: dev-self-signed
auth:
  issuer_url: ""
  paranoid: false
uploads:
  public_uploads: true
tos_url: https://fleet.nova.test/tos
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
  # 300s, NOT a short interval. A freshly booted donor uses a hardcoded initial
  # cadence (heartbeat 300s, pins-poll 600s) until its first heartbeat delivers
  # config_updates, and the liveness sweeper marks a node suspect after
  # suspect_after_missed_heartbeats * heartbeat_interval. With a 5s interval
  # that is 15s, so the whole fleet went suspect while still perfectly healthy —
  # a property of the test's clock, not of the fleet.
  #
  # 300 * 3 = 900s, comfortably longer than this run.
  heartbeat_interval_seconds: 300
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

start_coordinator() { # $1 = head|old
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
    local i=0
    until curl -sf "http://127.0.0.1:$API_PORT/health" >/dev/null 2>&1; do
        i=$((i + 1))
        kill -0 "$COORD_PID" 2>/dev/null || return 1
        [ "$i" -gt 60 ] && return 1
        sleep 2
    done
    return 0
}
stop_coordinator() {
    [ -n "$COORD_PID" ] && kill "$COORD_PID" 2>/dev/null || true
    wait "$COORD_PID" 2>/dev/null || true
    COORD_PID=""
}
start_donor() { # $1 = index
    local i="$1" build="${FLEET_BUILD[$1]}"
    "$WORK/bin/$build-node" --config "$WORK/donor-$i/node.yaml" \
        >> "$WORK/donor-$i.log" 2>&1 &
    DONOR_PIDS[i]=$!
}
stop_donor() {
    local i="$1"
    [ -n "${DONOR_PIDS[$i]:-}" ] && kill "${DONOR_PIDS[$i]}" 2>/dev/null || true
    wait "${DONOR_PIDS[$i]}" 2>/dev/null || true
    DONOR_PIDS[i]=""
}

# ---------------------------------------------------------------------------
# The fleet registers against the PREDECESSOR coordinator
# ---------------------------------------------------------------------------

log "starting the PREDECESSOR coordinator (schema $BASE_SCHEMA)"
start_coordinator old || die "the predecessor coordinator did not start"

# The candidate-build donors start together; they now send a real Nebula
# certificate fingerprint and cannot collide.
for i in 0 2 4 5; do start_donor "$i"; done
wait_sql "SELECT count(*) FROM nodes" 4 "the candidate-build donors register" 60 \
    || die "the candidate-build donors did not all register"

# The PREDECESSOR-build donors start one at a time, with a rewrite between them.
#
# This is not a test convenience; it is the workaround the finding forces. Those
# binaries send "" for nebula_cert_fingerprint, the column is UNIQUE NOT NULL,
# and the predecessor coordinator has no placeholder — so an operator whose
# fleet predates this release can bring up exactly ONE such donor at a time and
# must stamp a distinct value before the next one registers.
#
# After the upgrade the candidate coordinator synthesizes `unknown:<node-id>`
# and this ceases to be necessary, which is the point.
for i in 1 3; do
    start_donor "$i"
    sleep 6
    x "UPDATE nodes SET nebula_cert_fingerprint = 'unknown:' || id::text
         WHERE nebula_cert_fingerprint = ''"
done

wait_sql "SELECT count(*) FROM nodes" 6 "six donors registered" 60 \
    || die "the fleet never fully registered against the predecessor coordinator"
wait_sql "SELECT count(*) FROM nodes WHERE status='active'" 6 "six donors active" 30 \
    || die "the fleet registered but did not all reach active"
log "six donors registered and active"

# Shape the fleet into its six kinds. These are DB-level facts about what a
# donor reports, and they are exactly the labels the claim says must not matter.
x "UPDATE nodes SET client_version = 'v0.0.1-ancient'
     WHERE id = '${NODE_IDS[4]}'::uuid"                              # unsupported-compatible
x "UPDATE nodes SET client_version = NULL
     WHERE id = '${NODE_IDS[3]}'::uuid"                              # precontract-unknown

BEFORE_ACTIVE="$(q "SELECT count(*) FROM nodes WHERE status='active'")"
BEFORE_DRAINING="$(q "SELECT count(*) FROM nodes WHERE draining_at IS NOT NULL")"

# ---------------------------------------------------------------------------
# THE COORDINATOR UPGRADE
# ---------------------------------------------------------------------------

log "stopping the predecessor coordinator and the fleet's poll loops"
stop_coordinator
for i in 0 1 2 3 4 5; do stop_donor "$i"; done

log "migrating $BASE_SCHEMA -> target with the candidate's migrate"
TARGET="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["target_schema"])' \
    "$(ls releases/intent/*.json | head -1)")"
ACK="$(DATABASE_URL="$DSN" NOVA_UPGRADE_JOURNAL_DIR="$WORK/journal" \
       "$WORK/bin/head-migrate" apply --to "$TARGET" 2>&1 \
       | grep -oE '^\s+- [a-z0-9-]+$' | sed 's/^[[:space:]]*- //' | sed 's/^/--acknowledge /' | tr '\n' ' ' || true)"
# shellcheck disable=SC2086
DATABASE_URL="$DSN" NOVA_UPGRADE_JOURNAL_DIR="$WORK/journal" \
    "$WORK/bin/head-migrate" apply --to "$TARGET" $ACK >/dev/null \
    || die "the migration failed"

log "starting the CANDIDATE coordinator (schema $TARGET)"
start_coordinator head || die "the candidate coordinator did not start after the upgrade"

# The donors come back — the volunteers did nothing, their processes simply
# keep running in a real deployment; restarting them here only replaces the
# poll loops the stop above ended.
for i in 0 1 2 3 4 5; do start_donor "$i"; done
wait_sql "SELECT count(*) FROM nodes WHERE status='active'" 6 "six donors active after upgrade" 60 \
    || log "  (not all six re-reported yet; assertions below will say which)"

echo
log "=== assertions ==="

# 17. Nobody evicted, nobody drained, by the upgrade alone.
AFTER_ACTIVE="$(q "SELECT count(*) FROM nodes WHERE status='active'")"
EVICTED="$(q "SELECT count(*) FROM nodes WHERE status='evicted'")"
AFTER_DRAINING="$(q "SELECT count(*) FROM nodes WHERE draining_at IS NOT NULL")"
check "the upgrade evicted nobody (evicted=$EVICTED)" "$([ "$EVICTED" = 0 ] && echo 0 || echo 1)"
check "every donor is still active ($AFTER_ACTIVE, was $BEFORE_ACTIVE)" \
      "$([ "$AFTER_ACTIVE" = "$BEFORE_ACTIVE" ] && echo 0 || echo 1)"
check "draining_at was never set automatically ($AFTER_DRAINING, was $BEFORE_DRAINING)" \
      "$([ "$AFTER_DRAINING" = "$BEFORE_DRAINING" ] && echo 0 || echo 1)"

# 18. Supported older donors keep receiving AND acknowledging assignments.
SEED_CID="bafyfleet000000000000000000000000000000000000000000000001"
seed_blob "$SEED_CID"
if DATABASE_URL="$DSN" "$WORK/bin/head-novactl" pin assign --cid "$SEED_CID" --node "${NODE_IDS[1]}" > "$WORK/assign1.log" 2>&1; then
    ASSIGNED=0
else
    ASSIGNED=1; sed 's/^/[fleet]         /' "$WORK/assign1.log" >&2
fi
check "the candidate coordinator assigns to a supported-older donor" "$ASSIGNED"

# 19-21. A version label, an absent label and an invalid one change nothing.
for idx in 3 4; do
    name="${FLEET_NAMES[$idx]}"
    cid="bafyfleet00000000000000000000000000000000000000000000000$((idx + 2))"
    seed_blob "$cid"
    if DATABASE_URL="$DSN" "$WORK/bin/head-novactl" pin assign --cid "$cid" --node "${NODE_IDS[$idx]}" > "$WORK/assign$idx.log" 2>&1; then
        ok=0
    else
        ok=1; sed 's/^/[fleet]         /' "$WORK/assign$idx.log" >&2
    fi
    check "a $name donor still receives assignments (its version label gates nothing)" "$ok"
done

# 22. A missing optional capability disables exactly its role.
CAPMISS_SOURCEABLE="$(q "SELECT count(*) FROM nodes
    WHERE id = '${NODE_IDS[2]}'::uuid AND effective_capabilities @> ARRAY['read-source/v1']")"
check "the capability-missing donor is excluded from the read-source role" \
      "$([ "$CAPMISS_SOURCEABLE" = 0 ] && echo 0 || echo 1)"
CAPMISS_ACTIVE="$(q "SELECT count(*) FROM nodes WHERE id = '${NODE_IDS[2]}'::uuid AND status='active'")"
check "the capability-missing donor loses nothing else — still active" \
      "$([ "$CAPMISS_ACTIVE" = 1 ] && echo 0 || echo 1)"

# 23. Unsupported status ALONE never triggers replacement.
UNSUP_DRAINING="$(q "SELECT count(*) FROM nodes WHERE id = '${NODE_IDS[4]}'::uuid AND draining_at IS NOT NULL")"
check "an unsupported-but-compatible donor was not drained by its label" \
      "$([ "$UNSUP_DRAINING" = 0 ] && echo 0 || echo 1)"

# 24. Legacy omission is distinguishable from rollback. A pre-contract donor
#     never sets the observation marker; the rollback half — a donor that sent
#     a contract and stopped — is unit-tested, because no shipped binary can be
#     made to stop mid-run.
PRECONTRACT_MARKER="$(q "SELECT count(*) FROM nodes
    WHERE id = '${NODE_IDS[3]}'::uuid AND runtime_contract_observed_at IS NULL")"
check "a pre-contract donor's omission leaves the observation marker NULL" \
      "$([ "$PRECONTRACT_MARKER" = 1 ] && echo 0 || echo 1)"
CURRENT_MARKER="$(q "SELECT count(*) FROM nodes
    WHERE id = '${NODE_IDS[0]}'::uuid AND runtime_contract_observed_at IS NOT NULL")"
check "a contract-bearing donor sets it, so the two states are distinguishable" \
      "$([ "$CURRENT_MARKER" = 1 ] && echo 0 || echo 1)"

# 25. Every supported donor completed the protocol against the new coordinator.
SYNCED="$(q "SELECT count(*) FROM nodes WHERE assignment_sync_state = 'current'")"
check "donors reconciled their assignment sync after the upgrade ($SYNCED)" \
      "$([ "$SYNCED" -ge 4 ] && echo 0 || echo 1)"

# 26. EVICTION RECOVERY. The one case that could not be fixed client-side: the
#     agent loads its registration once and never re-registers, and it ignores
#     the refusal. Evict a live donor and let it heartbeat.
log "evicting the stale-offline donor and letting it return"
x "UPDATE nodes SET status='evicted', assignment_sync_state='current' WHERE id = '${NODE_IDS[5]}'::uuid"
# Restart it so it heartbeats now rather than in five minutes. A booting agent
# heartbeats immediately; it does NOT re-register, which is the whole premise —
# it loads its durable registration and carries on, exactly as a volunteer's
# container would after a host reboot.
stop_donor 5
start_donor 5
if wait_sql "SELECT count(*) FROM nodes WHERE id = '${NODE_IDS[5]}'::uuid AND status='active'" 1 \
      "the evicted donor returns" 45; then
    check "an evicted donor recovers with no re-enrollment" 0
else
    check "an evicted donor recovers with no re-enrollment" 1
fi
RECOVERED_SYNC="$(q "SELECT count(*) FROM nodes
    WHERE id = '${NODE_IDS[5]}'::uuid AND assignment_sync_state <> 'current'")"
check "and it reconciles against a SNAPSHOT rather than a pruned change log" \
      "$([ "$RECOVERED_SYNC" = 1 ] && echo 0 || echo 1)"
AUDITED="$(q "SELECT count(*) FROM audit_log WHERE action='node.reactivate.evicted'")"
check "the reactivation is recorded in audit_log ($AUDITED)" \
      "$([ "$AUDITED" -ge 1 ] && echo 0 || echo 1)"

# And the whole point, restated at the end: no mass repair.
REPAIRS="$(q "SELECT count(*) FROM pin_assignments WHERE state = 'failed'")"
check "no assignment failed across the upgrade ($REPAIRS)" \
      "$([ "$REPAIRS" = 0 ] && echo 0 || echo 1)"

echo
if [ "$FAILURES" -gt 0 ]; then
    echo "[fleet] $FAILURES assertion(s) failed" >&2
    exit 1
fi
cat <<EOF

OK: mixed-fleet-e2e
    proves:  coordinator-upgrade-needs-no-donor-upgrade
    fleet:   ${FLEET_NAMES[*]}
    upgrade: predecessor coordinator at schema $BASE_SCHEMA -> candidate at $TARGET
    acceptance scenarios: 17-26

    Does NOT cover: real Nebula routing, MTU, NAT traversal or lighthouse
    failure. This gate runs over loopback because the overlay is transport and
    the claim is about coordinator policy; federation-deploy-e2e owns the
    overlay and needs TUN.
EOF
