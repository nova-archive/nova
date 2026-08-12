#!/usr/bin/env bash
# scripts/upgrade_wire_e2e.sh — mixed-version PROTOCOL compatibility on a fresh
# database (P2-M7.3, D-M7.3-12, gate `upgrade-wire-e2e`).
#
# Acceptance scenarios satisfied: 1, 2, 3, 5.
#
# ============================================================================
# Why this is a thin wrapper and not a fourth implementation
# ============================================================================
#
# scripts/crossversion_e2e.sh already stands up exactly this: a HEAD coordinator
# and a BASELINE donor, real binaries, real federation mTLS, on a fresh database
# migrated by the coordinator's own migrate. Writing a second script that did
# the same thing would give the fleet two harnesses to keep in sync and one more
# place for a caveat to go stale — which is the failure this milestone spent its
# first four tasks undoing.
#
# What was missing was not a harness. It was a NAMED gate whose coverage entry
# says what it proves, so a claim in a signed lock can reference it. This script
# is that name. It runs the one pairing that IS the wire case and asserts the
# claim explicitly.
#
# ============================================================================
# The claim, and its limits
# ============================================================================
#
# PROVES  baseline-donor-interop: a donor built at the baseline commit
#         registers, heartbeats, syncs, and accepts, fetches and acknowledges
#         assignments against the candidate coordinator.
#
# DOES NOT PROVE
#   * anything about the schema. Every pairing gets a fresh database migrated by
#     the coordinator side's own binary, so no binary runs against a schema it
#     did not produce. That is upgrade-schema-e2e.
#   * anything about released artifacts. Both sides are built from source. That
#     is upgrade-release-e2e.
#
# Requirements: docker, Go toolchain + libvips dev headers. See the cross-version
# gate's header for the port list.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PREDECESSOR="${PREDECESSOR:-$(go run ./internal/release/cmd/novarel predecessor)}"
[ -n "$PREDECESSOR" ] || { echo "[wire] could not read the predecessor from the release intent" >&2; exit 1; }

echo "[wire] gate: upgrade-wire-e2e"
echo "[wire] claim: baseline-donor-interop"
echo "[wire] candidate coordinator (HEAD) x baseline donor ($PREDECESSOR)"
echo

# head-coord-old-donor is the wire case: the CANDIDATE coordinator serving a
# donor built at the baseline. The reverse pairing is a different question and
# belongs to the mixed-fleet gate.
PREDECESSOR="$PREDECESSOR" ./scripts/crossversion_e2e.sh head-coord-old-donor

echo
echo "OK: upgrade-wire-e2e"
echo "    proves:  baseline-donor-interop"
echo "    acceptance scenarios: 1, 2, 3, 5"
echo "    does NOT prove: schema compatibility (upgrade-schema-e2e),"
echo "                    released artifacts (upgrade-release-e2e)"
