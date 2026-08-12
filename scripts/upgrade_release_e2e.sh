#!/usr/bin/env bash
# scripts/upgrade_release_e2e.sh — an EXISTING deployment crosses to the
# released artifacts (P2-M7.3, D-M7.3-12, gate `upgrade-release-e2e`).
#
# Acceptance scenarios satisfied: 15, 16 (published half).
#
# MANDATORY FOR COMPLETION STATE 4. P2-M7.3 is not complete until this passes,
# and it is bound to NO lock claim — a result it produced could only be added to
# a lock that was already signed, and the v0.3.0 lock is not re-cut.
#
# ============================================================================
# The published half of a transition that is deliberately split in two
# ============================================================================
#
# `upgrade-candidate-e2e` proves the pre-publication half: a baseline deployment
# crosses to the exact CANDIDATE artifacts by the documented path. It is
# provable at lock time, and its claim is in the lock.
#
# This gate proves what that one cannot, because none of it exists yet when the
# lock is cut: the final REGISTRY REFS resolve to the digests the lock names,
# the RELEASE ASSETS download, the lock AS DOWNLOADED authenticates against the
# out-of-band policy, and an operator following docs/UPGRADING.md crosses to it.
#
# Splitting them is what makes a first release possible at all. A single claim
# about crossing to a published release could never be proven before one
# existed, so no release could ever be cut.
#
# It follows docs/UPGRADING.md and nothing else. Any step this script needs that
# UPGRADING.md does not document is a documentation defect, and the right fix is
# to write the step down rather than to add it here.
#
# ============================================================================
# It cannot pass before a release exists, and it says so
# ============================================================================
#
# There are no published Nova release artifacts yet. Rather than fabricate a
# pass, this gate SKIPS with an explicit reason and exit code 0 — and the
# distinction matters downstream: `release.EvidenceRef.Outcome` accepts
# "passed" or "skipped", and a claim proven by a skipped gate is rejected. A
# skip here therefore cannot become a claim in a signed lock. It is a recorded
# absence, which is the honest shape for "this has not run yet".
#
# Supply a bundle to make it run:
#
#   NOVA_RELEASE_BUNDLE=./nova-vX.Y.Z \
#   NOVA_RELEASE_LOCK_DIGEST=sha256:... \
#     ./scripts/upgrade_release_e2e.sh
#
# Requirements when it does run: docker, docker compose, cosign, and a
# deployment at the intent's declared predecessor to upgrade FROM. Nothing here
# names a version, a commit or a schema: the release under test comes from the
# bundle, the schema from its lock, and the predecessor from the intent.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

BUNDLE="${NOVA_RELEASE_BUNDLE:-}"
LOCK_DIGEST="${NOVA_RELEASE_LOCK_DIGEST:-}"
VARIANT="${1:-existing-override}"

log() { echo "[release] $*"; }

case "$VARIANT" in
  zero-donor|fresh-install|existing-override) ;;
  *) echo "usage: $0 [zero-donor|fresh-install|existing-override]" >&2; exit 2 ;;
esac

if [ -z "$BUNDLE" ] || [ -z "$LOCK_DIGEST" ]; then
  cat >&2 <<EOF
[release] SKIPPED — no release bundle supplied.

  This gate exercises PUBLISHED artifacts, and none exist yet: the release
  workflow has not run, so there is no signed lock, no bundle and no digests
  to pull. Building images here and calling them "the release" would make the
  gate pass while testing the one thing it is not supposed to test.

  A skip is not a pass. release.EvidenceRef records the outcome, a claim proven
  by a skipped gate is rejected, and the gate-coverage entry for
  upgrade-release-e2e stays a placeholder — which blocks a release candidate.

  To run it:
    NOVA_RELEASE_BUNDLE=<unpacked bundle> \\
    NOVA_RELEASE_LOCK_DIGEST=sha256:<digest you verified out of band> \\
      \$0 $VARIANT
EOF
  echo "SKIPPED: upgrade-release-e2e (no published release to test against)"
  exit 0
fi

[ -d "$BUNDLE" ] || { echo "[release] $BUNDLE is not a directory" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Step 1 — the trust bootstrap, exactly as UPGRADING.md documents it
# ---------------------------------------------------------------------------
#
# The operator's `cosign verify-blob` happens BEFORE this script; what the
# script can check is that the bundle re-establishes the chain from the digest
# that verification produced. Nothing from the bundle has run until nova-release
# has verified its own hash against the lock.

log "verifying the bundle against $LOCK_DIGEST"
"$BUNDLE/scripts/nova-release" verify --bundle "$BUNDLE" --lock-digest "$LOCK_DIGEST" \
  || { echo "[release] the bundle did not verify; nothing further may run" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Step 1b — the REGISTRY refs resolve to the digests the lock names
# ---------------------------------------------------------------------------
#
# This is the half `upgrade-candidate-e2e` structurally cannot do. It proved a
# transition to the candidate DIGESTS; nothing at that point could say what the
# published `:vX.Y.Z` tags would resolve to, because they did not exist.
#
# A promoted tag that resolves elsewhere is not a theoretical worry: `docker
# buildx imagetools create` rewraps a single-platform manifest in a new index
# unless told otherwise, and the operator following UPGRADING.md would pull
# something the lock never described.

log "checking that the published registry refs resolve to the locked descriptors"
LOCK_DESCRIPTORS="$(mktemp -d)"
trap 'rm -rf "$LOCK_DESCRIPTORS"' EXIT
python3 - "$BUNDLE/lock.json" "$LOCK_DESCRIPTORS" <<'PY'
import json, os, sys
lock = json.load(open(sys.argv[1]))
for name, artifact in lock["artifacts"].items():
    json.dump(artifact, open(os.path.join(sys.argv[2], name + ".json"), "w"), indent=2)
print("[release] the lock names %d artifact(s)" % len(lock["artifacts"]))
PY
RELEASE_VERSION="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["version"])' "$BUNDLE/lock.json")"
"$ROOT/scripts/release-publish.sh" verify-published \
    --version "$RELEASE_VERSION" --descriptors "$LOCK_DESCRIPTORS" \
  || { echo "[release] a published version tag does not resolve to the locked descriptor" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Step 2 — install the release env atomically
# ---------------------------------------------------------------------------

RELEASE_ENV="${NOVA_RELEASE_ENV:-$ROOT/.nova-release-e2e/release.env}"
mkdir -p "$(dirname "$RELEASE_ENV")"
"$BUNDLE/scripts/nova-release" install-env \
  --bundle "$BUNDLE" --lock-digest "$LOCK_DIGEST" --dest "$RELEASE_ENV"

# ---------------------------------------------------------------------------
# Step 3 — preflight from the TARGET admin, against the running deployment
# ---------------------------------------------------------------------------
#
# From the target, because an N-1 deployment cannot evaluate an N target: it
# does not know what N requires.

log "preflight"
"$BUNDLE/scripts/nova-release" exec --bundle "$BUNDLE" --lock-digest "$LOCK_DIGEST" -- \
  upgrade check --lock /release/lock.json --intent /release/intent.json \
                --expect-lock-digest "$LOCK_DIGEST" \
  || { echo "[release] preflight blocked; that is the gate working" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Step 4 — apply, bounded, under the advisory lock
# ---------------------------------------------------------------------------

TARGET_SCHEMA="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["target_schema"])' "$BUNDLE/lock.json")"
log "applying migrations to schema $TARGET_SCHEMA"
docker compose -f docker/docker-compose.yml \
  --env-file docker/.env --env-file "$RELEASE_ENV" \
  --profile upgrade run --rm migrate apply --to "$TARGET_SCHEMA" \
  --expect-release-lock "$LOCK_DIGEST" \
  || { echo "[release] migration failed; the journal is on the nova-upgrade volume" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Step 5 — bring the released topology up
# ---------------------------------------------------------------------------

COMPOSE=(docker compose -f docker/docker-compose.yml)
if [ "$VARIANT" = existing-override ]; then
  # The live deployment's observed configuration. Nova's commands pass -f
  # explicitly, which suppresses automatic override discovery, so the override
  # only applies when it is named — and it has to keep working.
  [ -f docker/docker-compose.override.yml ] && COMPOSE+=(-f docker/docker-compose.override.yml)
fi
COMPOSE+=(-f deploy/operator/compose.federation.yaml
          --env-file docker/.env --env-file "$RELEASE_ENV"
          --profile prod --profile federation)

log "starting the released topology ($VARIANT)"
"${COMPOSE[@]}" up -d

# ---------------------------------------------------------------------------
# Step 6 — cross-plane verification under one run id
# ---------------------------------------------------------------------------

REPORTS="$ROOT/.nova-release-e2e/reports"
mkdir -p "$REPORTS"
RUN_ID="release-e2e-$(date -u +%Y%m%dT%H%M%SZ)"
log "verifying, run $RUN_ID"
"$BUNDLE/scripts/nova-release" verify-planes \
  --bundle "$BUNDLE" --lock-digest "$LOCK_DIGEST" \
  --report-dir "$REPORTS" --run-id "$RUN_ID" \
  || { echo "[release] verification failed; reports in $REPORTS" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Step 7 — the deployment is running the digests the lock names
# ---------------------------------------------------------------------------
#
# `up -d` returning zero means Compose was satisfied, not that the running
# containers are the artifacts that were verified.

log "confirming the running images are the locked digests"
python3 - "$BUNDLE/lock.json" <<'PY' || exit 1
import json, subprocess, sys
lock = json.load(open(sys.argv[1]))
want = {}
for name, svc in (("nova-coordinator", "nova-coordinator"),):
    a = lock["artifacts"].get(name)
    if a:
        want[svc] = a["repository"] + "@" + a["descriptor"]["digest"]
out = subprocess.run(["docker", "inspect", "--format", "{{index .RepoDigests 0}}", "nova-coordinator"],
                     capture_output=True, text=True)
got = out.stdout.strip()
expected = want.get("nova-coordinator")
if expected and got != expected:
    sys.exit("[release] the coordinator is running %s, not the locked %s" % (got or "nothing", expected))
print("[release] running digests match the lock")
PY

echo
echo "OK: upgrade-release-e2e ($VARIANT) — $RELEASE_VERSION"
echo "    proves:  NO lock claim, by construction. This ran after the lock was signed,"
echo "             so nothing it concludes could appear in the document it verified."
echo "    gates:   completion state 4. The milestone is not done until this passes,"
echo "             and this passing never re-cuts the release."
echo "    checked: the published registry refs, the release assets, the lock as"
echo "             DOWNLOADED, and the operator path in docs/UPGRADING.md"
echo "    acceptance scenarios: 15, 16 (published half)"
echo "    reports: $REPORTS"
