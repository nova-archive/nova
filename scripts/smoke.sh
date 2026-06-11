#!/usr/bin/env bash
# scripts/smoke.sh — M14 full-stack end-to-end smoke test.
#
# Exercises the real compose artifact, start to finish:
#   1. Build the coordinator image (compose build stanza; warm cache).
#   2. Bring up postgres, wait for readiness.
#   3. Headless first-run setup (`novactl setup --config-file`) via a one-off
#      coordinator container: migrations + secrets + operator user + rendered
#      two-vhost nova.conf + .bootstrap-complete sentinel.
#   4. Bring up the prod profile (coordinator + nginx + certbot).
#   5. Prove upload → read-back → transform → delete through nginx TLS:
#        [1/6] anonymous multipart PNG upload (public vhost :8443)
#        [2/6] byte-identical read-back of /blob/{cid}
#        [3/6] image transform /i/{cid}/w320.png → 200
#        [4/6] operator login (admin vhost :8445) + DELETE /api/v1/blobs/{cid}
#        [5/6] deleted blob no longer served (404/410)
#        [6/6] coordinator restart comes back healthy (the entrypoint's
#              root-phase chown -R must survive the cap_drop floors on
#              RESTART, when nova-owned 0700 dirs already exist)
#
# Self-contained and idempotent: artifacts live in a mktemp dir; teardown is
# `down -v` in the EXIT trap. The seeded docker/.env persists across runs
# (pre-existing behavior; it is gitignored).
#
# Exit codes:
#   0  success
#   1  any step failed

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

ENV_FILE="docker/.env"
if [[ ! -f "$ENV_FILE" ]]; then
    if [[ -f "docker/.env.example" ]]; then
        echo "[smoke] docker/.env not found; copying from docker/.env.example"
        cp docker/.env.example "$ENV_FILE"
        # Generate a random password for smoke runs to avoid placeholder leaking.
        sed -i.bak "s/changeme/$(openssl rand -hex 16)/" "$ENV_FILE" && rm -f "$ENV_FILE.bak"
    else
        echo "[smoke] FAIL: $ENV_FILE missing and no .env.example to seed from" >&2
        exit 1
    fi
fi

# shellcheck disable=SC1090
source "$ENV_FILE"

DC=(docker compose -f docker/docker-compose.yml --env-file "$ENV_FILE")

HOST="smoke.nova.test"
ADMIN_EMAIL="operator@example.invalid"
ADMIN_PW="$(openssl rand -hex 12)"
TMP="$(mktemp -d)"

cleanup() {
    rm -rf "$TMP"
    "${DC[@]}" --profile prod down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

# fail <reason>: print the failure, dump the last 50 compose log lines, exit 1.
fail() {
    echo "[smoke] FAIL: $1" >&2
    echo "[smoke] ─── last 50 compose log lines ───" >&2
    "${DC[@]}" --profile prod logs --tail 50 >&2 || true
    exit 1
}

# Both vhosts share server_name $HOST; --resolve makes SNI/Host match the
# dev-self-signed cert, -k accepts the self-signed chain.
PUB="https://$HOST:8443"
ADM="https://$HOST:8445"
CURL_PUB=(curl -ksS --resolve "$HOST:8443:127.0.0.1")
CURL_ADM=(curl -ksS --resolve "$HOST:8445:127.0.0.1")

echo "[smoke] Ensuring a clean slate (down -v)..."
"${DC[@]}" --profile prod down -v --remove-orphans >/dev/null 2>&1 || true

echo "[smoke] Building the coordinator image..."
"${DC[@]}" build coordinator >/dev/null || fail "coordinator image build failed"

echo "[smoke] Bringing up postgres..."
"${DC[@]}" up -d postgres

echo "[smoke] Waiting for postgres to be ready..."
for i in {1..30}; do
    if "${DC[@]}" exec -T postgres pg_isready -U nova -d nova >/dev/null 2>&1; then
        echo "[smoke] Postgres ready after ${i}s"
        break
    fi
    sleep 1
    if [[ $i -eq 30 ]]; then
        fail "postgres did not become ready in 30s"
    fi
done

echo "[smoke] Headless setup (novactl setup --config-file)..."
cat > "$TMP/answers.yaml" <<EOF
hostname: $HOST
contact_email: smoke@example.invalid
admin_email: $ADMIN_EMAIL
admin_password: $ADMIN_PW
tls_mode: dev-self-signed
auth_mode: local
public_uploads: true
tos_url: https://$HOST/tos
paranoid: false
EOF
# One-off container: the image entrypoint always execs the coordinator, so
# bypass it with --entrypoint. Runs as root (no USER in the Dockerfile); the
# prod boot's chown -R in entrypoint.sh fixes volume ownership afterwards.
# DATABASE_URL/NOVA_*_DIR come from the coordinator service environment.
"${DC[@]}" run --rm -T --entrypoint /bin/sh \
    -v "$TMP/answers.yaml:/answers.yaml:ro" \
    coordinator -c "/usr/local/bin/migrate up && /usr/local/bin/novactl setup --config-file /answers.yaml" \
    || fail "headless setup (migrate + novactl setup) failed"

echo "[smoke] Bringing up the prod profile..."
"${DC[@]}" --profile prod up -d

echo "[smoke] Waiting for /health through nginx (public vhost)..."
health_code=""
for i in {1..60}; do
    health_code="$("${CURL_PUB[@]}" -o /dev/null -w '%{http_code}' "$PUB/health" 2>/dev/null || true)"
    if [[ "$health_code" == "200" ]]; then
        echo "[smoke] Health OK after $((i * 2))s"
        break
    fi
    sleep 2
    if [[ $i -eq 60 ]]; then
        fail "public /health did not return 200 within 120s (last code: ${health_code:-none})"
    fi
done

echo "[smoke] Seeding a public collection (visibility floor: no membership => private)..."
# Blobs with no collection membership resolve to private visibility
# (pkg/coordinator/storage/types.go resolveVisibility), so an anonymous /blob
# read would 401. Phase 1 has no collection-creation API; seed one owned by
# the setup-created operator, exactly as the M4 integration test does.
# No PGPASSWORD needed: compose exec runs inside the container, where the
# postgres image grants trust auth to POSTGRES_USER on local sockets.
COL_ID="$("${DC[@]}" exec -T postgres psql -U nova -d nova -tA -c \
    "INSERT INTO collections (owner_id, name, slug, visibility, public_archival)
     SELECT id, 'smoke-public', 'smoke-public', 'public', false
     FROM users WHERE email = '$ADMIN_EMAIL'
     RETURNING id;" | head -n1 | tr -d '[:space:]')" || fail "seeding the public collection failed"
[[ -n "$COL_ID" ]] || fail "public-collection seed returned an empty id (operator user missing?)"
echo "[smoke]       collection_id=$COL_ID"

echo "[smoke] [1/6] Anonymous multipart PNG upload..."
python3 - "$TMP/fixture.png" <<'PYEOF'
import struct, sys, zlib
def chunk(t, d):
    return struct.pack(">I", len(d)) + t + d + struct.pack(">I", zlib.crc32(t + d) & 0xffffffff)
w = h = 16
raw = b"".join(b"\x00" + b"\xc8\x32\x32" * w for _ in range(h))
png = (b"\x89PNG\r\n\x1a\n"
       + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0))
       + chunk(b"IDAT", zlib.compress(raw))
       + chunk(b"IEND", b""))
open(sys.argv[1], "wb").write(png)
PYEOF
# product=image so the /i/* transform routes accept the blob (raw blobs 415).
code="$("${CURL_PUB[@]}" -o "$TMP/upload.json" -w '%{http_code}' \
    -F "file=@$TMP/fixture.png;type=image/png;filename=fixture.png" \
    -F "product=image" \
    -F "collection_id=$COL_ID" \
    "$PUB/api/v1/blobs")" || fail "upload request failed"
if [[ "$code" != "201" ]]; then
    cat "$TMP/upload.json" >&2 || true
    fail "upload returned $code (want 201)"
fi
CID="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["cid"])' "$TMP/upload.json")" \
    || fail "could not extract cid from upload response"
[[ -n "$CID" ]] || fail "upload response contained an empty cid"
echo "[smoke]       cid=$CID"

echo "[smoke] [2/6] Read-back byte identity (/blob/$CID)..."
code="$("${CURL_PUB[@]}" -o "$TMP/readback.png" -w '%{http_code}' "$PUB/blob/$CID")" \
    || fail "read-back request failed"
[[ "$code" == "200" ]] || fail "read-back returned $code (want 200)"
cmp -s "$TMP/fixture.png" "$TMP/readback.png" || fail "read-back bytes differ from the uploaded fixture"

# w320 is the smallest width in the shipped allowlist (nova-image
# DefaultConfig AllowedWidths: 320/512/1024/2048; anything else 400s).
echo "[smoke] [3/6] Image transform (/i/$CID/w320.png)..."
code="$("${CURL_PUB[@]}" -o "$TMP/transform.png" -w '%{http_code}' "$PUB/i/$CID/w320.png")" \
    || fail "transform request failed"
[[ "$code" == "200" ]] || fail "transform returned $code (want 200)"
[[ -s "$TMP/transform.png" ]] || fail "transform returned an empty body"

echo "[smoke] [4/6] Operator login via admin vhost + DELETE via public vhost (/api/v1/blobs/$CID)..."
code="$("${CURL_ADM[@]}" -o "$TMP/login.json" -w '%{http_code}' \
    -H 'Content-Type: application/json' \
    -d "{\"username\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PW\"}" \
    "$ADM/api/v1/auth/login")" || fail "login request failed"
if [[ "$code" != "200" ]]; then
    cat "$TMP/login.json" >&2 || true
    fail "login returned $code (want 200)"
fi
TOKEN="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["access_token"])' "$TMP/login.json")" \
    || fail "could not extract access_token from login response"
[[ -n "$TOKEN" ]] || fail "login response contained an empty access_token"

code="$("${CURL_PUB[@]}" -o "$TMP/delete.json" -w '%{http_code}' -X DELETE \
    -H "Authorization: Bearer $TOKEN" \
    "$PUB/api/v1/blobs/$CID")" || fail "delete request failed"
# [[ RHS ]] of != is a glob: 2* matches 200/201/204/… (bash [[ ]] semantics).
# shellcheck disable=SC2053
if [[ "$code" != 2* ]]; then
    cat "$TMP/delete.json" >&2 || true
    fail "delete returned $code (want 2xx)"
fi

echo "[smoke] [5/6] Deleted blob no longer served..."
# nginx caches /blob 200s for 1y; bust the cache key with a benign query param
# (the signed-URL guard only engages on sig/exp/aud/kid) so the probe reaches
# the coordinator instead of the step-[2/6] cache entry.
code="$("${CURL_PUB[@]}" -o /dev/null -w '%{http_code}' "$PUB/blob/$CID?nocache=$RANDOM")" \
    || fail "post-delete read request failed"
if [[ "$code" != "404" && "$code" != "410" ]]; then
    fail "deleted blob still served: GET /blob/$CID returned $code (want 404 or 410)"
fi

echo "[smoke] [6/6] Coordinator restart survives the hardening floors..."
# Regression guard for the cap_drop/read_only floors: the entrypoint's root
# phase (chown -R over volumes that an earlier boot already made nova-owned
# 0700, e.g. /etc/nova/tls and the Kubo keystore) must work on RESTART, not
# just first boot — this is exactly the path a first-boot-only smoke misses.
"${DC[@]}" --profile prod restart coordinator || fail "coordinator restart failed"
restart_code=""
for i in {1..30}; do
    restart_code="$("${CURL_PUB[@]}" -o /dev/null -w '%{http_code}' "$PUB/health?nocache=$RANDOM" 2>/dev/null || true)"
    if [[ "$restart_code" == "200" ]]; then
        echo "[smoke]       healthy again after restart"
        break
    fi
    sleep 2
    if [[ $i -eq 30 ]]; then
        fail "coordinator did not come back healthy after restart (last code: ${restart_code:-none})"
    fi
done

echo "[smoke] PASS — upload → read → transform → delete → restart proven through the prod stack."
