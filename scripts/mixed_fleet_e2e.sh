#!/usr/bin/env bash
# scripts/mixed_fleet_e2e.sh — a coordinator upgrade requires no donor upgrade
# (P2-M7.3, D-M7.3-21, gate `mixed-fleet-e2e`).
#
# Acceptance scenarios satisfied: 17-26.
#
# ============================================================================
# The claim
# ============================================================================
#
# coordinator-upgrade-needs-no-donor-upgrade: upgrading the coordinator alone
# evicts nobody and triggers no mass repair across a fleet holding, at the same
# time, donors that are current, supported-older, missing an optional
# capability, pre-contract (reporting nothing), unsupported-but-compatible, and
# stale.
#
# It is one claim about the WHOLE fleet, which is why it needs a simultaneous
# fleet rather than six sequential pairings: the failure this guards against is
# a policy that is individually reasonable for each donor and collectively
# evicts half of them.
#
# ============================================================================
# SCOPE, STATED PLAINLY
# ============================================================================
#
# This script is INCOMPLETE and exits non-zero. What exists is the fleet
# taxonomy and the assertions, written down so the shape is reviewable; what
# does not exist is the harness that stands six real donors up simultaneously
# over a TUN overlay.
#
# The gate-coverage entry for mixed-fleet-e2e therefore remains a PLACEHOLDER,
# which blocks a release candidate — and that is the correct state. A script
# that exercised two donors and printed the six-donor claim would be worse than
# one that refuses: it would retire the placeholder and put an unearned claim in
# a signed lock.
#
# What HAS landed from this task is the part that could not wait, because it is
# a product defect rather than a test gap: the coordinator-side safe
# reactivation path. See internal/federation/coordinator/reactivation.go and its
# tests, which run today.
#
# ============================================================================
# The fleet this needs
# ============================================================================
#
#   current                  the candidate donor build
#   supported-older          the predecessor named by the release intent
#   capability-missing       current, but not advertising read-source/v1
#   precontract-unknown      a donor that sends no runtime contract at all
#   unsupported-compatible   older than the support window, speaking fed/v1
#   stale-offline            registered, then stopped
#
# ============================================================================
# What it must assert
# ============================================================================
#
#   1.  A coordinator upgrade alone evicts nobody and starts no mass repair.
#   2.  Supported older donors keep receiving AND acknowledging assignments
#       across the upgrade.
#   3.  A version label, an invalid version string and a digest mismatch each
#       alter NOTHING about durability counting or placement. They are census
#       facts.
#   4.  A missing optional capability disables exactly its role. A donor without
#       read-source/v1 stops being chosen as a read source and keeps everything
#       else, including its replicas.
#   5.  An unsupported donor's replica is REPLACED BEFORE the donor is demoted,
#       and only after an operator initiates drain. Unsupported status alone
#       never triggers replacement — that is what makes the support window a
#       statement rather than a weapon.
#   6.  A sole holder is never demoted. It is the repair source of last resort,
#       and demoting it trades a reputation signal for the only copy.
#   7.  A change to the mandatory profile strands nobody and never sets
#       draining_at automatically.
#   8.  Every supported donor artifact completes register, heartbeat,
#       diff/snapshot sync, assign/fetch/ack, unpin, and its applicable
#       read/repair/audit behaviour.
#   9.  A rollback clears the runtime contract and removes the role, while a
#       legacy donor's OMISSION does not — the two produce byte-identical
#       heartbeats, and runtime_contract_observed_at is what separates them.
#  10.  EVICTION RECOVERY: a supported pre-contract donor, offline past the
#       30-day threshold, returns with its durable registration and existing
#       certificate and recovers WITHOUT re-enrollment or manual state deletion,
#       completes snapshot reconciliation, and regains only safe eligibility.
#
# Assertion 10's implementation is done and unit-tested; the rest need the
# harness.
set -euo pipefail

cat >&2 <<'EOF'
mixed-fleet-e2e is NOT IMPLEMENTED.

  The taxonomy and the assertions are written down in this file's header so the
  shape is reviewable. The harness that stands six donors up simultaneously over
  a TUN overlay is not written.

  The gate-coverage entry for mixed-fleet-e2e stays a PLACEHOLDER, which blocks
  a release candidate. That is the correct state: a script that exercised two
  donors and printed the six-donor claim would retire the placeholder and put an
  unearned claim in a signed lock.

  What DID land from this task, because it is a product defect rather than a
  test gap:

    the coordinator-side safe reactivation path for evicted donors
    (internal/federation/coordinator/reactivation.go)

  A supported donor offline past the eviction threshold used to be useless
  INDEFINITELY and could not be taught otherwise: the agent loads its durable
  registration once and never re-registers, the coordinator told it to
  re-register, and the agent reduced that to a warning it ignored. The fix could
  not live in a binary already deployed on other people's machines. It is
  unit-tested and runs today.

EOF
exit 1
