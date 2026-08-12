#!/usr/bin/env bash
# scripts/upgrade_schema_e2e.sh — the PREDECESSOR coordinator against the
# FORWARD schema (P2-M7.3, D-M7.3-12, gate `upgrade-schema-e2e`).
#
# Acceptance scenarios satisfied: 4, 6.
#
# ============================================================================
# Why this gate exists
# ============================================================================
#
# Nothing tested this. The cross-version gate gives every pairing a FRESH
# database migrated by that side's own binary, so no binary is ever run against
# a schema it did not produce — which means it cannot say anything about the one
# question a rollback turns on: after the migrations have run, does the OLD
# coordinator still work?
#
# That question is what `migrations.Obligations.OldBinaryCompatible` claims an
# answer to, and until now the answer was derived by reading the SQL. Deriving
# it by inspection is not evidence, which is why 0003 is marked conservatively
# false with a note saying so. This gate is what can replace inspection with a
# result.
#
# It is also the ONLY gate that can support a rollback-safe claim. `migrate
# down` does not exist and will not: the contract is restore-from-backup. So
# "rollback" here means exactly one thing — redeploy the previous BINARY against
# the schema the new one left behind — and this is where that is checked.
#
# ============================================================================
# What it does NOT prove
# ============================================================================
#
#   * nothing about donors: none participate;
#   * nothing about released artifacts: both binaries are built from source;
#   * nothing about a schema DOWNGRADE, which is not a supported operation.
#
# Requirements: docker, Go toolchain + libvips dev headers, free port 15545.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# The predecessor comes from the reviewed intent, never a hand-maintained
# constant. See scripts/crossversion_e2e.sh for why a variable that can only
# hold a tag cannot name a commit-anchored baseline.
PREDECESSOR="${PREDECESSOR:-$(go run ./internal/release/cmd/novarel predecessor)}"
[ -n "$PREDECESSOR" ] || { echo "[schema] could not read the predecessor from the release intent" >&2; exit 1; }

# postgres:16-alpine as resolved 2026-08-11. Twins in
# internal/upgrade/apply_test.go and internal/db/migrations/upgrade_runs_test.go.
PG_IMAGE="postgres:16-alpine@sha256:57c72fd2a128e416c7fcc499958864df5301e940bca0a56f58fddf30ffc07777"
PG_PORT="${PG_PORT:-15545}"
API_PORT="${API_PORT:-19010}"

WORK="$(mktemp -d)"
COORD_PID=""

log()  { echo "[schema] $*"; }
fail() {
    echo "[schema] FAIL: $1" >&2
    [ -f "$WORK/coordinator.log" ] && { echo "[schema] ─── coordinator log ───" >&2; tail -40 "$WORK/coordinator.log" >&2; }
    exit 1
}
cleanup() {
    [ -n "$COORD_PID" ] && kill "$COORD_PID" 2>/dev/null || true
    docker rm -f schema-pg >/dev/null 2>&1 || true
    git worktree remove --force "$WORK/old" 2>/dev/null || true
    rm -rf "$WORK"
}
trap cleanup EXIT

DSN="postgres://postgres:nova@127.0.0.1:$PG_PORT/nova_schema?sslmode=disable"
psqlq() { docker exec schema-pg psql -U postgres -d nova_schema -tAc "$1"; }

mkdir -p "$WORK/bin" "$WORK/journal" "$WORK/etc-nova" "$WORK/kubo-repo"

# ---------------------------------------------------------------------------
# Build both sides
# ---------------------------------------------------------------------------

# HEAD is the candidate: with descriptors, the pushed image extracted by
# digest; without them, a stamped local build. The PREDECESSOR is always built
# from source — it has no published image, which is the whole reason the
# baseline is commit-anchored.
. "$ROOT/scripts/lib/candidate.sh"
log "resolving the candidate"
nova_candidate_bin migrate     "$WORK/bin/head-migrate"     || fail "no candidate migrate"
nova_candidate_bin coordinator "$WORK/bin/head-coordinator" || fail "no candidate coordinator"
log "candidate: $NOVA_CANDIDATE_SOURCE"

log "checking out predecessor $PREDECESSOR"
git worktree add --force --detach "$WORK/old" "$PREDECESSOR" >/dev/null \
    || fail "cannot check out $PREDECESSOR — it is named by the release intent"
log "building the predecessor coordinator"
(cd "$WORK/old" && go build -o "$WORK/bin/old-coordinator" ./cmd/coordinator) \
    || fail "the predecessor coordinator does not build"

# ---------------------------------------------------------------------------
# A database at the FORWARD schema
# ---------------------------------------------------------------------------

log "starting postgres"
docker run -d --name schema-pg -e POSTGRES_PASSWORD=nova -e POSTGRES_DB=nova_schema \
    -p "127.0.0.1:$PG_PORT:5432" "$PG_IMAGE" >/dev/null
# A real query, not pg_isready. During initdb the image runs a TEMPORARY server
# that pg_isready happily reports as ready, and the client then connects to a
# database that is about to be restarted out from under it — which is how the
# first run of this gate "failed" with an error nobody could reproduce by hand.
n=0
until docker exec schema-pg psql -U postgres -d nova_schema -tAc 'SELECT 1' >/dev/null 2>&1; do
    n=$((n + 1)); [ "$n" -gt 60 ] && fail "postgres never accepted a query"
    sleep 1
done

# The PREDECESSOR's schema first, so the migration under test is a real
# transition rather than a fresh install. The predecessor's own migrate binary
# applies it, because that is the schema that deployment actually has.
(cd "$WORK/old" && go build -o "$WORK/bin/old-migrate" ./cmd/migrate) || fail "predecessor migrate does not build"
log "applying the predecessor's schema"
DATABASE_URL="$DSN" "$WORK/bin/old-migrate" up > "$WORK/old-migrate.log" 2>&1 \
    || { tail -20 "$WORK/old-migrate.log" >&2; fail "predecessor migrate up failed"; }
BASE_SCHEMA="$(psqlq 'SELECT COALESCE(MAX(version_id),0) FROM goose_db_version WHERE is_applied')"
log "predecessor schema: $BASE_SCHEMA"

TARGET="$(go run ./internal/release/cmd/novarel show "$(ls releases/intent/*.json | head -1)" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["target_schema"])')"
log "applying (${BASE_SCHEMA}, ${TARGET}] with HEAD's migrate"

# Acknowledge every procedure the range carries. This gate is about what happens
# AFTER the migrations, so an unacknowledged obligation blocking here would be
# the wrong failure — and the acknowledgement ids come from the tool rather than
# being typed in, which is also how an operator would get them.
ACK="$(DATABASE_URL="$DSN" NOVA_UPGRADE_JOURNAL_DIR="$WORK/journal" \
       "$WORK/bin/head-migrate" apply --to "$TARGET" --json 2>&1 \
       | grep -oE '^\s+- [a-z0-9-]+$' | sed 's/^[[:space:]]*- //' | sed 's/^/--acknowledge /' | tr '\n' ' ' || true)"
# shellcheck disable=SC2086
DATABASE_URL="$DSN" NOVA_UPGRADE_JOURNAL_DIR="$WORK/journal" \
    "$WORK/bin/head-migrate" apply --to "$TARGET" $ACK >/dev/null \
    || fail "HEAD migrate apply --to $TARGET failed"

APPLIED="$(psqlq 'SELECT COALESCE(MAX(version_id),0) FROM goose_db_version WHERE is_applied')"
[ "$APPLIED" = "$TARGET" ] || fail "schema is $APPLIED, expected $TARGET"
log "schema is now $APPLIED"

# ---------------------------------------------------------------------------
# THE TEST: the predecessor coordinator, against that forward schema
# ---------------------------------------------------------------------------

MASTER_KEY_HEX="$(openssl rand -hex 32)"
OIDC_KEY_HEX="$(openssl rand -hex 32)"
printf '/key/swarm/psk/1.0.0/\n/base16/\n%s\n' "$(openssl rand -hex 32)" > "$WORK/swarm.key"
touch "$WORK/etc-nova/.bootstrap-complete"

cat > "$WORK/operator.yaml" <<EOF
operator:
  hostname: schema.nova.test
  contact_email: schema@example.invalid
tls:
  mode: dev-self-signed
auth:
  issuer_url: ""
  paranoid: false
uploads:
  public_uploads: true
tos_url: https://schema.nova.test/tos
coordinator:
  public_ipfs_dht: false
EOF

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

log "starting the PREDECESSOR coordinator against schema $APPLIED"
: > "$WORK/coordinator.log"
if ! start_coordinator old; then
    fail "the predecessor coordinator did not come up against schema $APPLIED.
    THIS IS THE RESULT THE GATE EXISTS TO PRODUCE: the range ($BASE_SCHEMA, $TARGET]
    is NOT old-binary compatible, and rolling the coordinator back after these
    migrations means restoring the database. Record it in
    internal/db/migrations/obligations.go rather than treating it as a flake."
fi
log "the predecessor coordinator is serving"

# Serving /health is a low bar. Exercise a read path that touches the tables the
# migrations changed, because "it started" and "its queries still work" are
# different claims and only the second one matters for a rollback.
# A login attempt with bad credentials. It is unauthenticated (so no token
# setup), it reaches a handler rather than stopping at middleware, and it
# QUERIES THE USERS TABLE — which is what makes it evidence. A 401 here means
# the predecessor's generated queries still work against the forward schema; a
# 500 means a column moved under them, which is precisely what
# OldBinaryCompatible is a claim about.
code="$(curl -sS -o "$WORK/probe.json" -w '%{http_code}' \
        -H 'Content-Type: application/json' \
        -d '{"email":"nobody@example.invalid","password":"wrong"}' \
        "http://127.0.0.1:$API_PORT/api/v1/auth/login" 2>/dev/null || echo 000)"
case "$code" in
  400|401|403)
    log "the predecessor's query path answers ($code)" ;;
  500|000)
    cat "$WORK/probe.json" >&2 2>/dev/null || true
    fail "the predecessor coordinator returned $code from a DB-backed path against schema
    $APPLIED. THIS IS THE RESULT THE GATE EXISTS TO PRODUCE: its queries do not survive
    the forward schema, so ($BASE_SCHEMA, $TARGET] is NOT old-binary compatible." ;;
  *)
    fail "unexpected $code from /api/v1/auth/login; the probe needs updating, not the
    coordinator — a gate that cannot tell success from a routing change proves nothing" ;;
esac

stop_coordinator

# ---------------------------------------------------------------------------
# And HEAD still works against the same database, so the result above is about
# the predecessor rather than about the database being broken.
# ---------------------------------------------------------------------------

log "control: HEAD against the same schema"
: > "$WORK/coordinator.log"
start_coordinator head || fail "HEAD does not run against the schema it produced — the
    result above says nothing about the predecessor, only that the database is unusable"
stop_coordinator

echo
echo "OK: the predecessor coordinator ($PREDECESSOR) runs and serves against schema $APPLIED"
echo "    range: ($BASE_SCHEMA, $TARGET]"
echo "    acceptance scenarios: 4, 6"
echo "    gate: upgrade-schema-e2e"
echo
echo "This is the evidence a rollback-safe claim needs. It does NOT cover donors,"
echo "released artifacts, or a schema downgrade."
