#!/usr/bin/env bash
# scripts/smoke.sh — M1 end-to-end smoke test.
#
# Brings up docker-compose postgres, runs cmd/migrate up against it,
# asserts the v3.1 schema, then tears down (leaving the volume).
#
# Exit codes:
#   0  success
#   1  any step failed (compose, migrate, or schema assertion)

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

DC="docker compose -f docker/docker-compose.yml --env-file $ENV_FILE"

echo "[smoke] Bringing up postgres..."
$DC up -d postgres

echo "[smoke] Waiting for postgres to be ready..."
for i in {1..30}; do
    if $DC exec -T postgres pg_isready -U nova -d nova >/dev/null 2>&1; then
        echo "[smoke] Postgres ready after ${i}s"
        break
    fi
    sleep 1
    if [[ $i -eq 30 ]]; then
        echo "[smoke] FAIL: postgres did not become ready in 30s" >&2
        $DC logs postgres
        exit 1
    fi
done

echo "[smoke] Building cmd/migrate..."
go build -o bin/migrate ./cmd/migrate

echo "[smoke] Running migrations..."
DATABASE_URL="postgres://nova:${POSTGRES_PASSWORD}@127.0.0.1:5432/nova?sslmode=disable" ./bin/migrate up

echo "[smoke] Asserting v3.1 schema..."
EXPECTED_TABLES=(
    users master_key_versions data_encryption_keys signing_keys
    collections blobs blob_manifests blob_blocks
    image_metadata collection_items
    nodes pin_assignments pin_audits
    integrity_audits moderation_decisions dmca_cases
    takedown_repeat_infringers signed_url_revocations
    audit_log jobs
)

for table in "${EXPECTED_TABLES[@]}"; do
    found=$(PGPASSWORD="$POSTGRES_PASSWORD" $DC exec -T postgres psql -U nova -d nova -t -c "SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='$table'" | tr -d '[:space:]')
    if [[ "$found" != "1" ]]; then
        echo "[smoke] FAIL: expected table '$table' is missing" >&2
        exit 1
    fi
done

# blobs.envelope_version
has_col=$(PGPASSWORD="$POSTGRES_PASSWORD" $DC exec -T postgres psql -U nova -d nova -t -c "SELECT 1 FROM information_schema.columns WHERE table_name='blobs' AND column_name='envelope_version'" | tr -d '[:space:]')
if [[ "$has_col" != "1" ]]; then
    echo "[smoke] FAIL: blobs.envelope_version column missing" >&2
    exit 1
fi

# integrity_audits is partitioned
is_part=$(PGPASSWORD="$POSTGRES_PASSWORD" $DC exec -T postgres psql -U nova -d nova -t -c "SELECT 1 FROM pg_partitioned_table pt JOIN pg_class c ON c.oid = pt.partrelid WHERE c.relname='integrity_audits'" | tr -d '[:space:]')
if [[ "$is_part" != "1" ]]; then
    echo "[smoke] FAIL: integrity_audits not partitioned" >&2
    exit 1
fi

echo "[smoke] PASS — all v3.1 tables present, envelope_version column exists, integrity_audits partitioned"

echo "[smoke] Tearing down (data preserved)..."
$DC down

echo "[smoke] OK"
