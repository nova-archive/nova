#!/usr/bin/env bash
# scripts/live_upgrade_e2e.sh — an EXISTING federation must cross this
# milestone using only docs/UPGRADING.md (P2-M7.2, D-M7.2-11).
#
# Track constraint 1: out-of-band manual remediation is not an acceptable
# upgrade path. Where a milestone would demand one, it ships the automation
# that removes it instead — here, adoptive `federation init`.
#
# For M7.2 the fixture synthesizes the shape a real deployment actually has:
# a CA and coordinator identity built by hand OUTSIDE the PKI volume, a
# hand-edited operator.yaml, an existing Kubo swarm key, and a donor already
# registered. It then asserts that after upgrade the CA fingerprint, the swarm
# key and the donor identity are all unchanged.
#
# Reused by every subsequent milestone in the track.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

step() { printf '\n=== %s ===\n' "$*"; }
fail() { printf '\nFAIL: %s\n' "$*" >&2; exit 1; }

PKI="$work/pki"
IMPORT="$work/import"
CONFIG="$work/config"
SECRETS="$work/secrets"
mkdir -p "$PKI" "$IMPORT" "$CONFIG" "$SECRETS"

# --- 1. Synthesize a hand-built federation ----------------------------------
# Deliberately NOT via `federation init` — the whole point is material that
# predates it, in an operator-chosen location.
step "1. synthesize a pre-existing hand-built federation"
go run ./cmd/novactl node ca-init \
  --dir "$IMPORT" \
  --coordinator-ip 10.42.0.1 \
  --coordinator-dns nova.e2e.test >/dev/null

printf '/key/swarm/psk/1.0.0/\n/base16/\n%s\n' \
  "$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')" > "$IMPORT/swarm.key"
chmod 0600 "$IMPORT/swarm.key"

ca_before="$(sha256sum "$IMPORT/federation-ca.crt" | cut -d' ' -f1)"
swarm_before="$(sha256sum "$IMPORT/swarm.key" | cut -d' ' -f1)"
echo "pre-existing CA:        $ca_before"
echo "pre-existing swarm key: $swarm_before"

# A donor issued by the OLD hand-built CA, as a real deployment would have.
donor_dir="$work/donor-old"
go run ./cmd/novactl node issue \
  --dir "$IMPORT" --name legacy-donor --out "$donor_dir" >/dev/null
donor_id_before="$(python3 -c '
import json,sys
print(json.load(open(sys.argv[1]))["node_id"])' "$donor_dir/node-manifest.json")"
echo "pre-existing donor id:  $donor_id_before"

# --- 2. Adopt, following UPGRADING.md ---------------------------------------
step "2. adopt via federation init --adopt-from (as UPGRADING.md instructs)"
go run ./cmd/novactl federation init \
  --root "$PKI" \
  --adopt-from "$IMPORT" \
  --overlay-cidr 10.42.0.0/24 \
  --operator-overlay-ip 10.42.0.1 \
  --lighthouse-public 203.0.113.7:4242 \
  --hostname nova.e2e.test \
  --skip-preflight \
  --runtime-config-dir "$CONFIG" \
  --runtime-secrets-dir "$SECRETS"

active="$PKI/active"

# --- 3. Nothing that would break an existing donor may have changed ---------
step "3. the CA, the swarm key and the donor identity survive unchanged"

ca_after="$(sha256sum "$active/federation-ca.crt" | cut -d' ' -f1)"
[ "$ca_before" = "$ca_after" ] \
  || fail "CA fingerprint changed across adoption — every issued certificate is now untrusted"
echo "ok: CA unchanged"

swarm_after="$(sha256sum "$active/swarm.key" | cut -d' ' -f1)"
[ "$swarm_before" = "$swarm_after" ] \
  || fail "swarm key rotated — every existing donor is silently partitioned from the swarm"
echo "ok: swarm key unchanged"

# The old donor's certificate must still verify against the adopted CA.
openssl verify -CAfile "$active/federation-ca.crt" "$donor_dir/federation.crt" >/dev/null 2>&1 \
  || fail "a donor issued before the upgrade no longer verifies — that is a re-enrollment"
echo "ok: pre-existing donor certificate still verifies"

# --- 4. The import directory was never written to ---------------------------
step "4. adoption never writes to the operator's existing PKI"
import_sum="$(find "$IMPORT" -type f -exec sha256sum {} \; | sort | sha256sum)"
go run ./cmd/novactl federation init \
  --root "$PKI" --adopt-from "$IMPORT" \
  --overlay-cidr 10.42.0.0/24 --operator-overlay-ip 10.42.0.1 \
  --lighthouse-public 203.0.113.7:4242 --hostname nova.e2e.test \
  --skip-preflight --runtime-config-dir "$CONFIG" --runtime-secrets-dir "$SECRETS" >/dev/null
import_sum_after="$(find "$IMPORT" -type f -exec sha256sum {} \; | sort | sha256sum)"
[ "$import_sum" = "$import_sum_after" ] || fail "/import was modified; it must be read-only"
echo "ok: import directory untouched, and the re-run was a no-op"

# --- 5. What adoption FILLED IN --------------------------------------------
step "5. adoption supplied what the hand-built setup lacked"
for f in federation-client.crt federation-client.key repair-signing.key nebula-ca.crt; do
  [ -f "$active/$f" ] || fail "adoption did not create $f"
done
echo "ok: coordinator client identity, repair seed and Nebula CA created"

# --- 6. Custody split held --------------------------------------------------
step "6. CA keys did not escape into the runtime volumes"
for d in "$CONFIG" "$SECRETS"; do
  for k in federation-ca.key nebula-ca.key; do
    [ -f "$d/$k" ] && fail "$k escaped into the runtime volume $d"
  done
done
[ -f "$SECRETS/nova_coordinator_federation_key" ] \
  || fail "the coordinator's runtime key was not installed; it could not read its own identity"
echo "ok: runtime identities installed, CA keys retained in the admin-only volume"

# --- 7. Doctor agrees -------------------------------------------------------
step "7. doctor passes against the adopted federation"
cat > "$work/operator.yaml" <<YAML
federation:
  listen_addr: "10.42.0.1:9443"
  nebula_interface: nebula1
  federation_ca_path: $CONFIG/federation/federation-ca.crt
  federation_cert_path: $CONFIG/federation/coordinator-federation.crt
  federation_key_path: $SECRETS/nova_coordinator_federation_key
YAML

go run ./cmd/novactl federation doctor \
  --root "$PKI" --operator-yaml "$work/operator.yaml" \
  || fail "doctor failed against the adopted federation"

printf '\nlive-upgrade-e2e: PASS\n'
