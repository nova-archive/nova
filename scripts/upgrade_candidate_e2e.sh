#!/usr/bin/env bash
# scripts/upgrade_candidate_e2e.sh — the PRE-PUBLICATION half of the transition
# (P2-M7.3, gate `upgrade-candidate-e2e`).
#
# Acceptance scenarios satisfied: 15, 16 (candidate half).
#
# ============================================================================
# Why the transition is two gates
# ============================================================================
#
# `baseline-deployment-transition` could never be proven at lock time. The lock
# is signed BEFORE the release is published, and a claim about crossing to a
# published release cannot be true until one exists. Requiring it would have
# meant a first release that could never be cut — the bootstrap this split
# breaks.
#
# So the transition is two claims with two gates:
#
#   THIS ONE, pre-publication — an existing deployment at the baseline crosses
#   to the EXACT CANDIDATE artifacts by the documented operator path. Provable
#   at lock time, because the candidate exists by then.
#
#   upgrade-release-e2e, post-publication — the final registry refs resolve to
#   the digests the lock names, the release assets download and verify, the lock
#   as DOWNLOADED authenticates, and an operator following UPGRADING.md crosses
#   to it. Mandatory for completion state 4, bound to no lock claim, and never
#   folded back into a lock that was already signed.
#
# ============================================================================
# What "exact candidate artifacts" means here
# ============================================================================
#
# In CI, `--artifacts descriptors/` pins the candidate by OCI digest and this
# script asserts the running binaries carry the matching build stamp.
#
# Run locally without it, the candidate is the working tree, built with the
# Makefile's ldflags so `buildinfo` is stamped rather than `dev`. The script
# then asserts the stamp is real and reports that the digest assertion did not
# run. That difference is why coverage and evidence are separate things: a local
# run characterises what this gate CAN prove; the CI run is what proves it for a
# particular candidate.
#
# Requirements: docker, Go toolchain + libvips dev headers, free ports 15548,
# 19030.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PREDECESSOR="${PREDECESSOR:-$(go run ./internal/release/cmd/novarel predecessor)}"
[ -n "$PREDECESSOR" ] || { echo "[cand] could not read the predecessor from the release intent" >&2; exit 1; }
DESCRIPTORS="${NOVA_CANDIDATE_DESCRIPTORS:-}"

PG_IMAGE="postgres:16-alpine@sha256:57c72fd2a128e416c7fcc499958864df5301e940bca0a56f58fddf30ffc07777"
PG_PORT=15548
API_PORT=19030

WORK="$(mktemp -d)"
COORD_PID=""
FAILURES=0

log()  { echo "[cand] $*"; }
pass() { echo "[cand]   PASS  $*"; }
check() {
    if [ "$2" = "0" ]; then pass "$1"; else echo "[cand]   FAIL  $1" >&2; FAILURES=$((FAILURES + 1)); fi
}
die() {
    echo "[cand] ABORT: $1" >&2
    tail -25 "$WORK/coordinator.log" >&2 2>/dev/null || true
    exit 1
}
cleanup() {
    [ -n "$COORD_PID" ] && kill "$COORD_PID" 2>/dev/null || true
    docker rm -f cand-pg >/dev/null 2>&1 || true
    git worktree remove --force "$WORK/old" 2>/dev/null || true
    rm -rf "$WORK"
}
trap cleanup EXIT

DSN="postgres://postgres:nova@127.0.0.1:$PG_PORT/nova_cand?sslmode=disable"
q() { docker exec cand-pg psql -U postgres -d nova_cand -tAc "$1" | tr -d '[:space:]'; }
x() { docker exec cand-pg psql -U postgres -d nova_cand -q -c "$1" >/dev/null; }

mkdir -p "$WORK/bin" "$WORK/journal" "$WORK/etc-nova" "$WORK/kubo-repo"

# ---------------------------------------------------------------------------
# The candidate, STAMPED. An unstamped build cannot be the subject of a claim
# about which artifacts an operator crossed to.
# ---------------------------------------------------------------------------

VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
REVISION="$(git rev-parse --short=7 HEAD 2>/dev/null || echo unknown)"

# Where the candidate comes from is scripts/lib/candidate.sh's decision, not
# this gate's: with descriptors it is the pushed image, without them a stamped
# local build. Every gate in the release plan asks the same way, so a gate
# cannot accidentally test something the others are not.
. "$ROOT/scripts/lib/candidate.sh"
nova_candidate_ldflags

log "resolving the candidate"
nova_candidate_bin coordinator "$WORK/bin/cand-coordinator" || die "no candidate coordinator"
nova_candidate_bin migrate     "$WORK/bin/cand-migrate"     || die "no candidate migrate"
nova_candidate_bin novactl     "$WORK/bin/cand-novactl"     || die "no candidate novactl"
log "candidate: $NOVA_CANDIDATE_SOURCE"

log "building the baseline $PREDECESSOR"
git worktree add --force --detach "$WORK/old" "$PREDECESSOR" >/dev/null || die "cannot check out $PREDECESSOR"
(cd "$WORK/old" && go build -o "$WORK/bin/base-coordinator" ./cmd/coordinator \
                && go build -o "$WORK/bin/base-migrate"     ./cmd/migrate) \
    || die "the baseline does not build"

# ---------------------------------------------------------------------------
# A baseline deployment with state worth preserving
# ---------------------------------------------------------------------------

log "starting postgres"
docker run -d --name cand-pg -e POSTGRES_PASSWORD=nova -e POSTGRES_DB=nova_cand \
    -p "127.0.0.1:$PG_PORT:5432" "$PG_IMAGE" >/dev/null
n=0
until docker exec cand-pg psql -U postgres -d nova_cand -tAc 'SELECT 1' >/dev/null 2>&1; do
    n=$((n + 1)); [ "$n" -gt 60 ] && die "postgres never accepted a query"
    sleep 1
done

DATABASE_URL="$DSN" "$WORK/bin/base-migrate" up >/dev/null 2>&1 || die "baseline migrate failed"
BASE_SCHEMA="$(q 'SELECT COALESCE(MAX(version_id),0) FROM goose_db_version WHERE is_applied')"
log "baseline deployment at schema $BASE_SCHEMA"

# Archive state and a registered donor: the two things an operator would never
# forgive losing.
NODE_ID="$(python3 -c 'import uuid; print(uuid.uuid4())')"
x "INSERT INTO users (email) VALUES ('candidate@example.invalid')"
x "INSERT INTO blobs (cid, mime_type, byte_size)
     VALUES ('bafycandidate0000000000000000000000000000000000000000001','application/octet-stream',64)"
x "INSERT INTO blob_manifests (cid, hash_alg, codec, chunker, plaintext_size, envelope_size, block_count)
     VALUES ('bafycandidate0000000000000000000000000000000000000000001','sha2-256','raw','size-262144',64,64,1)"
x "INSERT INTO nodes (id, nebula_cert_fingerprint, federation_cert_fingerprint,
                      capacity_bytes, bandwidth_budget_bytes_per_day, advertised_capabilities,
                      client_version)
     VALUES ('$NODE_ID'::uuid, 'neb-$NODE_ID', 'fed-$NODE_ID', 1000000, 1000000,
             ARRAY['pin-change-log/v1','snapshot/v1','blob-transfer/v1']::text[], 'commit:$PREDECESSOR')"

BEFORE_USERS="$(q 'SELECT count(*) FROM users')"
BEFORE_BLOBS="$(q 'SELECT count(*) FROM blobs')"
BEFORE_NODES="$(q 'SELECT count(*) FROM nodes')"

MASTER_KEY_HEX="$(openssl rand -hex 32)"
OIDC_KEY_HEX="$(openssl rand -hex 32)"
printf '/key/swarm/psk/1.0.0/\n/base16/\n%s\n' "$(openssl rand -hex 32)" > "$WORK/swarm.key"
touch "$WORK/etc-nova/.bootstrap-complete"
cat > "$WORK/operator.yaml" <<EOF
operator:
  hostname: candidate.nova.test
  contact_email: candidate@example.invalid
tls:
  mode: dev-self-signed
auth:
  issuer_url: ""
  paranoid: false
uploads:
  public_uploads: true
tos_url: https://candidate.nova.test/tos
coordinator:
  public_ipfs_dht: false
EOF

start_coordinator() { # $1 = base|cand
    NOVA_CONFIG_FILE="$WORK/operator.yaml" NOVA_CONFIG_DIR="$WORK/etc-nova" \
    DATABASE_URL="$DSN" NOVA_KUBO_REPO="$WORK/kubo-repo" \
    IPFS_SWARM_KEY_FILE="$WORK/swarm.key" NOVA_LISTEN_ADDR="127.0.0.1:$API_PORT" \
    NOVA_MASTER_KEY_ACTIVE=v1 NOVA_MASTER_KEY_V1="$MASTER_KEY_HEX" \
    NOVA_OIDC_SIGNING_KEY="$OIDC_KEY_HEX" NOVA_METRICS_LISTEN_ADDR="" \
        "$WORK/bin/$1-coordinator" >> "$WORK/coordinator.log" 2>&1 &
    COORD_PID=$!
    local i=0
    until curl -sf "http://127.0.0.1:$API_PORT/health" >/dev/null 2>&1; do
        i=$((i + 1))
        kill -0 "$COORD_PID" 2>/dev/null || return 1
        [ "$i" -gt 45 ] && return 1
        sleep 2
    done
    return 0
}
stop_coordinator() {
    [ -n "$COORD_PID" ] && kill "$COORD_PID" 2>/dev/null || true
    wait "$COORD_PID" 2>/dev/null || true
    COORD_PID=""
}

log "the baseline coordinator is serving"
start_coordinator base || die "the baseline deployment did not come up"
stop_coordinator

echo
log "=== the documented operator path ==="

# Step 1 — record what is running. Works with the database up and would work
# with it down, which is the point of a compiled-in catalog.
DATABASE_URL="$DSN" "$WORK/bin/cand-novactl" upgrade status --json > "$WORK/status.json" 2>/dev/null \
    && STATUS_OK=0 || STATUS_OK=1
check "upgrade status reports the deployment before anything is touched" "$STATUS_OK"

STAMPED="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["binary"]["stamped"])' "$WORK/status.json" 2>/dev/null || echo False)"
check "the candidate is STAMPED, so the transition names real artifacts (version $VERSION)" \
      "$([ "$STAMPED" = "True" ] && echo 0 || echo 1)"

REPORTED_SCHEMA="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["schema"]["applied"])' "$WORK/status.json" 2>/dev/null || echo "?")"
check "it reports the baseline's applied schema ($REPORTED_SCHEMA)" \
      "$([ "$REPORTED_SCHEMA" = "$BASE_SCHEMA" ] && echo 0 || echo 1)"

# Step 2 — the STARTUP FLOOR. An operator who swaps the binary before applying
# the migrations must be refused, not silently served 500s an hour later.
log "starting the candidate against the STALE schema (must refuse)"
if start_coordinator cand; then
    check "the candidate refuses to serve against a stale schema" 1
    stop_coordinator
else
    check "the candidate refuses to serve against a stale schema" 0
fi
grep -q "refusing to start" "$WORK/coordinator.log" \
    && REASON=0 || REASON=1
check "and it says why, naming the schema it expects" "$REASON"

# Step 3 — the target-bounded apply, with obligations acknowledged by id.
TARGET="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["target_schema"])' \
    "$(ls releases/intent/*.json | head -1)")"
ACK="$(DATABASE_URL="$DSN" NOVA_UPGRADE_JOURNAL_DIR="$WORK/journal" \
       "$WORK/bin/cand-migrate" apply --to "$TARGET" 2>&1 \
       | grep -oE '^\s+- [a-z0-9-]+$' | sed 's/^[[:space:]]*- //' | sed 's/^/--acknowledge /' | tr '\n' ' ' || true)"
# shellcheck disable=SC2086
if DATABASE_URL="$DSN" NOVA_UPGRADE_JOURNAL_DIR="$WORK/journal" \
     "$WORK/bin/cand-migrate" apply --to "$TARGET" $ACK > "$WORK/apply.log" 2>&1; then
    check "migrate apply --to $TARGET crossed ($BASE_SCHEMA, $TARGET]" 0
else
    sed 's/^/[cand]         /' "$WORK/apply.log" >&2
    check "migrate apply --to $TARGET crossed ($BASE_SCHEMA, $TARGET]" 1
fi

RUN_RECORDED="$(q "SELECT count(*) FROM upgrade_runs WHERE to_schema = $TARGET")"
check "the run is recorded in upgrade_runs ($RUN_RECORDED)" \
      "$([ "$RUN_RECORDED" -ge 1 ] && echo 0 || echo 1)"
FP="$(q "SELECT config_fingerprint_before FROM upgrade_runs ORDER BY started_at DESC LIMIT 1")"
check "with the configuration fingerprinted going in" \
      "$([ -n "$FP" ] && echo 0 || echo 1)"

# Step 4 — the candidate serves.
log "starting the candidate against the migrated schema"
start_coordinator cand || die "the candidate did not start after the migration"

# Step 5 — the state an operator would never forgive losing.
AFTER_USERS="$(q 'SELECT count(*) FROM users')"
AFTER_BLOBS="$(q 'SELECT count(*) FROM blobs')"
AFTER_NODES="$(q 'SELECT count(*) FROM nodes')"
SAME_NODE="$(q "SELECT count(*) FROM nodes WHERE id = '$NODE_ID'::uuid")"
check "users survived ($AFTER_USERS, was $BEFORE_USERS)"  "$([ "$AFTER_USERS" = "$BEFORE_USERS" ] && echo 0 || echo 1)"
check "archive rows survived ($AFTER_BLOBS, was $BEFORE_BLOBS)" "$([ "$AFTER_BLOBS" = "$BEFORE_BLOBS" ] && echo 0 || echo 1)"
check "donors survived ($AFTER_NODES, was $BEFORE_NODES)"  "$([ "$AFTER_NODES" = "$BEFORE_NODES" ] && echo 0 || echo 1)"
check "and the donor kept its node id — not a re-enrollment" \
      "$([ "$SAME_NODE" = 1 ] && echo 0 || echo 1)"

CAPS_BACKFILLED="$(q "SELECT count(*) FROM nodes
    WHERE id = '$NODE_ID'::uuid AND effective_capabilities @> ARRAY['blob-transfer/v1']")"
check "0019 backfilled effective_capabilities from what the donor already advertised" \
      "$([ "$CAPS_BACKFILLED" = 1 ] && echo 0 || echo 1)"

# Step 6 — verify.
DATABASE_URL="$DSN" "$WORK/bin/cand-novactl" upgrade verify --plane database \
    --run-id "candidate-$$" --report-dir "$WORK/reports" >/dev/null 2>&1 \
    && VERIFIED=0 || VERIFIED=1
check "upgrade verify --plane database passes against the new schema" "$VERIFIED"
VERIFY_EVENT="$(q "SELECT count(*) FROM upgrade_events WHERE phase = 'verify'")"
check "and the verify phase is recorded in upgrade_events ($VERIFY_EVENT)" \
      "$([ "$VERIFY_EVENT" -ge 1 ] && echo 0 || echo 1)"

stop_coordinator

# ---------------------------------------------------------------------------
# The digest assertion, when there is something to assert against
# ---------------------------------------------------------------------------

if [ -n "$DESCRIPTORS" ]; then
    log "checking the candidate against the supplied descriptors"
    for name in nova-coordinator nova-node nova-admin; do
        [ -f "$DESCRIPTORS/$name.json" ] || { check "descriptor for $name" 1; continue; }
        d="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["descriptor"]["digest"])' "$DESCRIPTORS/$name.json")"
        case "$d" in sha256:*) check "$name is pinned by digest" 0 ;; *) check "$name is pinned by digest" 1 ;; esac
    done

    # The binaries that just ran came OUT of the pushed image. Asserted rather
    # than assumed: an empty descriptor directory or a reordered edit would
    # otherwise leave every assertion above passing about the wrong artifact,
    # and passing loudly is worse than failing.
    if nova_candidate_assert_digests nova-coordinator; then
        check "the candidate binaries were extracted from the pushed coordinator digest" 0
    else
        check "the candidate binaries were extracted from the pushed coordinator digest" 1
    fi

    # And the extracted binary is stamped with the version being released,
    # rather than with a `git describe` of whatever the runner checked out.
    if [ -n "${NOVA_CANDIDATE_VERSION:-}" ]; then
        got_version="$("$WORK/bin/cand-novactl" version 2>/dev/null | head -n1 || true)"
        case "$got_version" in
            *"$NOVA_CANDIDATE_VERSION"*)
                check "the extracted binary is stamped $NOVA_CANDIDATE_VERSION" 0 ;;
            *)
                echo "[cand]   version reported: ${got_version:-nothing}" >&2
                check "the extracted binary is stamped $NOVA_CANDIDATE_VERSION" 1 ;;
        esac
    fi
    DIGESTS_CHECKED="yes — the candidate under test IS the pushed image"
else
    DIGESTS_CHECKED="NO — run with NOVA_CANDIDATE_DESCRIPTORS to test the pushed images themselves"
fi

echo
if [ "$FAILURES" -gt 0 ]; then
    echo "[cand] $FAILURES assertion(s) failed" >&2
    exit 1
fi
cat <<EOF

OK: upgrade-candidate-e2e
    proves:  candidate-baseline-transition
    path:    baseline $PREDECESSOR at schema $BASE_SCHEMA -> candidate $VERSION at $TARGET
    digests: $DIGESTS_CHECKED
    acceptance scenarios: 15, 16 (candidate half)

    Does NOT cover the PUBLISHED half — registry refs, release assets, the
    downloaded lock, or an operator following UPGRADING.md against a real
    release. That is upgrade-release-e2e, it cannot run until the release
    exists, and P2-M7.3 is not complete until it passes.
EOF
