#!/usr/bin/env bash
# scripts/backup_restore_e2e.sh — the backup is RESTORED, not merely taken
# (P2-M7.3, Task 27).
#
# Acceptance scenario satisfied: 8.
#
# ============================================================================
# Why this exists
# ============================================================================
#
# UPGRADING.md tells an operator to take a backup before migrating, and the
# composed migration obligations can make "restore from backup" the literal
# rollback boundary — for 0003 it is the only way back. So the backup is not a
# precaution here; it is a documented recovery path, and a recovery path nobody
# has walked is a hope.
#
# The sentence the operator sequence turns on is: A BACKUP THAT HAS NEVER PASSED
# RESTORE VERIFICATION DOES NOT COUNT. This script is what makes that sentence
# checkable rather than advice.
#
# ============================================================================
# What is backed up, and why all three
# ============================================================================
#
#   1. Postgres — the archive's metadata: blobs, manifests, assignments,
#      registrations, audit history. Without it the blobs on donors are
#      unreachable ciphertext.
#   2. nova-secrets — the master key, the OIDC signing seed, the swarm key.
#      Without the master key every blob in the archive stays encrypted
#      forever; a Postgres-only backup restores a catalogue of things nobody
#      can open.
#   3. nova-fedpki — the federation and Nebula CA keys. Without them the
#      operator cannot issue, rotate or revoke a donor identity, and the
#      federation slowly becomes unmanageable as certificates expire.
#
# Restoring one or two of the three is the failure mode worth catching, and it
# is the one an untested backup produces: each is a different volume, and
# nothing in the deployment says they belong together except this script and
# the documentation it verifies.
#
# Requirements: docker, Go toolchain + libvips dev headers. No TUN: the
# federation identities are exercised by re-reading them, not by an overlay.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PG_IMAGE="postgres:16-alpine@sha256:57c72fd2a128e416c7fcc499958864df5301e940bca0a56f58fddf30ffc07777"
PG_PORT="${PG_PORT:-15546}"

WORK="$(mktemp -d)"
log()  { echo "[backup] $*"; }
fail() { echo "[backup] FAIL: $1" >&2; exit 1; }
cleanup() {
    docker rm -f backup-pg backup-pg-restored >/dev/null 2>&1 || true
    rm -rf "$WORK"
}
trap cleanup EXIT

mkdir -p "$WORK/bin" "$WORK/backup" "$WORK/secrets" "$WORK/fedpki" "$WORK/journal" \
         "$WORK/runtime-config" "$WORK/runtime-secrets"

DSN="postgres://postgres:nova@127.0.0.1:$PG_PORT/nova_backup?sslmode=disable"
psqlq() { docker exec "$1" psql -U postgres -d nova_backup -tAc "$2"; }

wait_pg() { # $1=container
    local n=0
    until docker exec "$1" psql -U postgres -d nova_backup -tAc 'SELECT 1' >/dev/null 2>&1; do
        n=$((n + 1)); [ "$n" -gt 60 ] && fail "$1 never accepted a query"
        sleep 1
    done
}

# ---------------------------------------------------------------------------
# A deployment with state worth losing
# ---------------------------------------------------------------------------

log "building"
go build -o "$WORK/bin/migrate" ./cmd/migrate
go build -o "$WORK/bin/novactl" ./cmd/novactl

log "starting postgres"
docker run -d --name backup-pg -e POSTGRES_PASSWORD=nova -e POSTGRES_DB=nova_backup \
    -p "127.0.0.1:$PG_PORT:5432" "$PG_IMAGE" >/dev/null
wait_pg backup-pg

log "applying the schema"
DATABASE_URL="$DSN" NOVA_UPGRADE_JOURNAL_DIR="$WORK/journal" \
    "$WORK/bin/migrate" up >/dev/null 2>&1 || fail "migrate up failed"

# The federation authority, created by the PACKAGED BOOTSTRAP rather than by the
# expert primitives. `federation init` is what a real operator runs, and it is
# what puts BOTH trust roots — the Nova federation CA and the Nebula CA — in the
# admin-only volume. `node ca-init` creates only the first, which is exactly the
# kind of partial backup this drill exists to catch: the first draft of this
# script used it and the verification correctly refused, because the Nebula CA
# was never there to lose.
log "creating the federation authority (packaged bootstrap)"
"$WORK/bin/novactl" federation init \
    --root "$WORK/fedpki" \
    --runtime-config-dir "$WORK/runtime-config" \
    --runtime-secrets-dir "$WORK/runtime-secrets" \
    --hostname backup.nova.test \
    --overlay-cidr 10.42.0.0/24 \
    --operator-overlay-ip 10.42.0.1 \
    --lighthouse-public 127.0.0.1:4242 \
    --skip-preflight > "$WORK/fedinit.log" 2>&1 \
    || { tail -20 "$WORK/fedinit.log" >&2; fail "federation init failed"; }

# --i-know-what-im-doing because the doctor's config-manifest check reads
# /etc/nova/operator.yaml, and there is no coordinator here to have written one.
# Doctor readiness is scripts/federation_deploy_e2e.sh's subject; this drill is
# about whether the ISSUED MATERIAL survives a disaster, and it needs a real
# donor identity to have something to lose.
"$WORK/bin/novactl" node invite --root "$WORK/fedpki" --name backup-donor \
    --nebula-ip 10.42.0.10/24 \
    --image "ghcr.io/nova-archive/nova-node@sha256:$(printf 'a%.0s' {1..64})" \
    --out "$WORK/donor" --i-know-what-im-doing > "$WORK/invite.log" 2>&1 \
    || { tail -20 "$WORK/invite.log" >&2; fail "issuing a donor identity failed"; }

DONOR_ID="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["node_id"])' \
    "$WORK/donor/invite-manifest.json")"
[ -n "$DONOR_ID" ] || fail "could not read the donor id from the invite manifest"

# The seeded donor's version label comes from the reviewed intent, not from a
# literal. A drill that hard-codes one predecessor stops describing the fleet
# the moment the intent names a different one.
PREDECESSOR="${PREDECESSOR:-$(go run ./internal/release/cmd/novarel predecessor)}"
[ -n "$PREDECESSOR" ] || fail "could not read the predecessor from the release intent"

ACTIVE="$WORK/fedpki/active"
[ -d "$ACTIVE" ] || ACTIVE="$WORK/fedpki"
CA_FP_BEFORE="$(openssl x509 -in "$ACTIVE/federation-ca.crt" -noout -fingerprint -sha256)"
# Keep the pre-disaster donor certificate OUTSIDE the volumes being destroyed,
# so the restore can be asked the question that matters: does the restored
# authority still vouch for what it issued before?
cp "$WORK/donor/federation/federation.crt" "$WORK/backup/old-donor.crt"

# The secrets an operator's nova-secrets volume holds.
openssl rand -hex 32 > "$WORK/secrets/master-key-v1"
openssl rand -hex 32 > "$WORK/secrets/oidc-signing-key"
printf '/key/swarm/psk/1.0.0/\n/base16/\n%s\n' "$(openssl rand -hex 32)" > "$WORK/secrets/swarm.key"
chmod 0600 "$WORK/secrets"/*
MASTER_BEFORE="$(sha256sum "$WORK/secrets/master-key-v1" | cut -d' ' -f1)"

# Database state that a restore has to bring back. A registered donor is the
# right shape: it joins the schema, the federation identity and the archive.
log "seeding archive state"
docker exec backup-pg psql -U postgres -d nova_backup -q -c "
    INSERT INTO nodes (id, nebula_cert_fingerprint, federation_cert_fingerprint,
                       capacity_bytes, bandwidth_budget_bytes_per_day,
                       advertised_capabilities, effective_capabilities, client_version)
    VALUES ('$DONOR_ID'::uuid, 'neb-$DONOR_ID', 'fed-$DONOR_ID', 1000000, 1000000,
            ARRAY['pin-change-log/v1','snapshot/v1']::text[],
            ARRAY['pin-change-log/v1','snapshot/v1']::text[], 'commit:$PREDECESSOR');
    INSERT INTO users (email) VALUES ('backup@example.invalid');
" >/dev/null || fail "seeding failed"

NODES_BEFORE="$(psqlq backup-pg 'SELECT count(*) FROM nodes')"
USERS_BEFORE="$(psqlq backup-pg 'SELECT count(*) FROM users')"
SCHEMA_BEFORE="$(psqlq backup-pg 'SELECT COALESCE(MAX(version_id),0) FROM goose_db_version WHERE is_applied')"
log "state: schema $SCHEMA_BEFORE, $NODES_BEFORE node(s), $USERS_BEFORE user(s)"

# ---------------------------------------------------------------------------
# THE BACKUP — all three, as one operation
# ---------------------------------------------------------------------------

log "taking the backup"
docker exec backup-pg pg_dump -U postgres -Fc nova_backup > "$WORK/backup/nova.dump" \
    || fail "pg_dump failed"
tar -C "$WORK" -czf "$WORK/backup/nova-secrets.tar.gz" secrets || fail "secrets archive failed"
tar -C "$WORK" -czf "$WORK/backup/nova-fedpki.tar.gz" fedpki  || fail "fedpki archive failed"

for f in nova.dump nova-secrets.tar.gz nova-fedpki.tar.gz; do
    [ -s "$WORK/backup/$f" ] || fail "$f is empty; a zero-byte backup is the classic silent failure"
    log "  $f  $(stat -c%s "$WORK/backup/$f") bytes"
done

# ---------------------------------------------------------------------------
# THE DISASTER — everything the backup covers is destroyed
# ---------------------------------------------------------------------------
#
# Not "stopped". Removed. A restore drill against a deployment that is still
# there tests almost nothing, because anything the restore forgets is still
# quietly present.

log "destroying the deployment"
docker rm -f backup-pg >/dev/null
rm -rf "$WORK/secrets" "$WORK/fedpki" "$WORK/donor" "$WORK/runtime-config" "$WORK/runtime-secrets"
[ -d "$WORK/secrets" ] && fail "the secrets survived a destroy that was supposed to remove them"

# ---------------------------------------------------------------------------
# THE RESTORE
# ---------------------------------------------------------------------------

log "restoring postgres into a fresh container"
docker run -d --name backup-pg-restored -e POSTGRES_PASSWORD=nova -e POSTGRES_DB=nova_backup \
    -p "127.0.0.1:$PG_PORT:5432" "$PG_IMAGE" >/dev/null
wait_pg backup-pg-restored

docker exec -i backup-pg-restored pg_restore -U postgres -d nova_backup --no-owner \
    < "$WORK/backup/nova.dump" >/dev/null 2>&1 || fail "pg_restore failed"

log "restoring the secrets and the federation authority"
tar -C "$WORK" -xzf "$WORK/backup/nova-secrets.tar.gz" || fail "secrets restore failed"
tar -C "$WORK" -xzf "$WORK/backup/nova-fedpki.tar.gz"  || fail "fedpki restore failed"

# ---------------------------------------------------------------------------
# VERIFICATION — the part that makes the backup count
# ---------------------------------------------------------------------------

log "verifying"

SCHEMA_AFTER="$(psqlq backup-pg-restored 'SELECT COALESCE(MAX(version_id),0) FROM goose_db_version WHERE is_applied')"
NODES_AFTER="$(psqlq backup-pg-restored 'SELECT count(*) FROM nodes')"
USERS_AFTER="$(psqlq backup-pg-restored 'SELECT count(*) FROM users')"

[ "$SCHEMA_AFTER" = "$SCHEMA_BEFORE" ] \
    || fail "schema is $SCHEMA_AFTER, was $SCHEMA_BEFORE — a restore that lands on a different
    schema is a restore into a deployment the binary cannot serve"
[ "$NODES_AFTER" = "$NODES_BEFORE" ] || fail "nodes: $NODES_AFTER, was $NODES_BEFORE"
[ "$USERS_AFTER" = "$USERS_BEFORE" ] || fail "users: $USERS_AFTER, was $USERS_BEFORE"
log "  postgres: schema $SCHEMA_AFTER, $NODES_AFTER node(s), $USERS_AFTER user(s)"

# The registered donor is still the SAME donor. A restore that brings back a row
# with a different id has re-enrolled the volunteer, which is the thing the
# whole track forbids.
RESTORED_ID="$(psqlq backup-pg-restored "SELECT id::text FROM nodes WHERE id = '$DONOR_ID'::uuid")"
[ "$RESTORED_ID" = "$DONOR_ID" ] || fail "the donor's registration did not come back with its id"
log "  donor $DONOR_ID is still registered"

# The master key. Without it every blob in that restored catalogue is
# permanently unreadable, and nothing else in this script would notice.
MASTER_AFTER="$(sha256sum "$WORK/secrets/master-key-v1" | cut -d' ' -f1)"
[ "$MASTER_AFTER" = "$MASTER_BEFORE" ] \
    || fail "the master key came back different; the archive is a catalogue of ciphertext
    nobody can open"
log "  master key intact"
for f in oidc-signing-key swarm.key; do
    [ -s "$WORK/secrets/$f" ] || fail "$f did not come back"
    perm="$(stat -c%a "$WORK/secrets/$f")"
    [ "$perm" = "600" ] || fail "$f restored as mode $perm; a restore that widens secret
    permissions has traded one outage for a disclosure"
done
log "  secrets intact, permissions preserved"

# The federation authority: the CA is the same CA, and the donor certificate it
# signed still verifies against it. That is the property that matters — not
# "the file exists", but "this authority still recognises the identities it
# issued before the disaster".
CA_FP_AFTER="$(openssl x509 -in "$ACTIVE/federation-ca.crt" -noout -fingerprint -sha256)"
[ "$CA_FP_AFTER" = "$CA_FP_BEFORE" ] \
    || fail "the federation CA came back with a different fingerprint; every donor
    certificate in the fleet is now signed by an authority that no longer exists"
log "  federation CA fingerprint unchanged"

# Reissue a donor identity from the restored authority and verify the OLD
# donor's certificate against the RESTORED CA. Both directions matter: the
# authority must still be able to issue, and must still vouch for what it issued.
"$WORK/bin/novactl" node invite --root "$WORK/fedpki" --name post-restore-donor \
    --nebula-ip 10.42.0.11/24 \
    --image "ghcr.io/nova-archive/nova-node@sha256:$(printf 'a%.0s' {1..64})" \
    --out "$WORK/donor-new" --i-know-what-im-doing > "$WORK/invite2.log" 2>&1 \
    || { tail -20 "$WORK/invite2.log" >&2; fail "the restored authority cannot issue; the
    operator can no longer onboard, rotate or revoke anyone"; }
log "  the restored authority can still issue"

# The question that actually matters. Not "the CA file came back" — "the
# restored authority still vouches for the identities it issued before the
# disaster". A guard that silently skips when the fixture is missing would make
# this the check nobody notices stopped running, so a missing fixture FAILS.
[ -f "$WORK/backup/old-donor.crt" ] \
    || fail "the pre-disaster donor certificate was never stashed, so the one assertion that
    distinguishes a restored authority from a new one cannot run"
openssl verify -CAfile "$ACTIVE/federation-ca.crt" "$WORK/backup/old-donor.crt" >/dev/null 2>&1 \
    || fail "a certificate issued before the disaster no longer verifies against the restored
    CA; every existing donor would have to re-enroll"
log "  a pre-disaster donor certificate still verifies against the restored CA"

# The Nebula CA travels in the same volume and is a separate trust root. Losing
# it silently is easy, because nothing fails until the first certificate
# expires.
[ -s "$ACTIVE/nebula-ca.crt" ] || fail "the Nebula CA certificate did not come back"
[ -s "$ACTIVE/nebula-ca.key" ] || fail "the Nebula CA KEY did not come back; the overlay
    cannot be extended or repaired, and nothing will fail until a certificate expires"
log "  both trust roots present"

echo
echo "OK: backup and restore verified"
echo "    postgres:      schema $SCHEMA_AFTER, $NODES_AFTER node(s), donor registration intact"
echo "    nova-secrets:  master key, OIDC seed and swarm key, 0600 preserved"
echo "    nova-fedpki:   federation CA fingerprint unchanged, still able to issue;"
echo "                   Nebula CA present"
echo "    acceptance scenario: 8"
echo
echo "This is what \"a backup that has never passed restore verification does not"
echo "count\" means operationally. Run it against YOUR backup procedure, not only"
echo "against this one."
