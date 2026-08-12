#!/usr/bin/env bash
# scripts/release_pipeline_rehearsal.sh — drive the ENTIRE release document
# pipeline offline, twice, for two different releases (P2-M7.3, amendment, gate
# `release-pipeline-rehearsal`).
#
# Runner tier: pr-static. No Docker, no registry, no network, no Sigstore.
#
# ============================================================================
# What can and cannot be rehearsed
# ============================================================================
#
# A release run does two kinds of work. One kind needs the outside world: build
# and push, OIDC, Cosign, GHCR, the approval gate. The other kind is pure
# document assembly — plan, evidence, bundle, lock, summary, verification — and
# that is where every release so far has actually stopped.
#
# This gate runs the second kind end to end with synthesized descriptors, so
# `dry_run: true` fails here, in seconds, on a laptop, rather than after twenty
# minutes of pushing and signing images.
#
# It does NOT claim to prove the workflow will succeed on GitHub. OIDC,
# environment approvals, GHCR permissions and immutable releases cannot be
# exercised locally, and pretending otherwise would be the same fabricated pass
# the post-publication gate refuses to produce.
#
# ============================================================================
# Twice, for two releases, because "works for the first one" is not the property
# ============================================================================
#
# Everything here is dispatched for an exact version and reads an exact intent.
# A pipeline that only works for v0.3.0 would pass every check written against
# v0.3.0. So it runs a second time against a synthetic v0.3.1 that differs in
# every dimension a real second release would:
#
#   - a VERSION-kind predecessor, not just the commit-anchored baseline;
#   - TWO predecessors, so the support window has a shape;
#   - a different, smaller claim set, so the plan schedules different gates;
#   - MULTI-PLATFORM artifacts, so the descriptor path goes through an index
#     with child manifests instead of a single manifest.
#
# It lives in internal/release/testdata rather than releases/intent because the
# compiled-in catalog is generated from the greatest version there: a fixture
# alongside the real intents would rewrite the product's own identity.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

PASS=0
FAIL=0
ok()  { PASS=$((PASS + 1)); printf '  ok   %s\n' "$*"; }
bad() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$*"; }
head_() { printf '\n== %s\n' "$*"; }

# One build of the tool, reused. `go run` per invocation would dominate the
# runtime of a gate whose whole point is being fast enough to run on every PR.
NOVAREL="$WORK/novarel"
go build -o "$NOVAREL" ./internal/release/cmd/novarel

COMMIT="$(git rev-parse HEAD)"
BUILT_AT="2026-08-12T00:00:00Z"

# synth_descriptors writes one descriptor per artifact for a platform list.
#
# A single platform produces a single MANIFEST carrying its own platform; two or
# more produce an INDEX with one child per platform. That is what a real build
# produces, and it is the difference the lock's platform validation turns on.
synth_descriptors() {
  local dir="$1" version="$2"; shift 2
  mkdir -p "$dir"
  rm -f "$dir"/*.json
  local name
  for name in nova-coordinator nova-node nova-admin; do
    python3 - "$dir/$name.json" "$name" "$version" "$@" <<'PY'
import hashlib, json, sys

out, name, version = sys.argv[1:4]
platforms = sys.argv[4:]

def descriptor(body, media, platform=None):
    raw = json.dumps(body, sort_keys=True, separators=(",", ":")).encode()
    d = {"mediaType": media, "digest": "sha256:" + hashlib.sha256(raw).hexdigest(),
         "size": len(raw)}
    if platform:
        d["platform"] = platform
    return d

MANIFEST = "application/vnd.oci.image.manifest.v1+json"
INDEX = "application/vnd.oci.image.index.v1+json"

def as_platform(p):
    parts = p.split("/")
    out = {"os": parts[0], "architecture": parts[1]}
    if len(parts) > 2:
        out["variant"] = parts[2]
    return out

if len(platforms) == 1:
    desc = descriptor({"artifact": name, "version": version, "platform": platforms[0]},
                      MANIFEST, as_platform(platforms[0]))
    artifact = {"repository": "ghcr.io/nova-archive/" + name, "descriptor": desc}
else:
    children = [descriptor({"artifact": name, "version": version, "platform": p},
                           MANIFEST, as_platform(p)) for p in platforms]
    # A real BuildKit index also carries an attestation manifest, marked with
    # architecture "unknown". It must be ignored rather than counted as a
    # platform, so the fixture includes one.
    children.append(descriptor({"attestation": name}, MANIFEST,
                               {"os": "unknown", "architecture": "unknown"}))
    index = descriptor({"artifact": name, "children": [c["digest"] for c in children]},
                       INDEX)
    artifact = {"repository": "ghcr.io/nova-archive/" + name,
                "descriptor": index, "manifests": children}

json.dump(artifact, open(out, "w"), indent=2)
PY
  done
}

# rehearse runs the whole document pipeline for one release.
rehearse() {
  local label="$1" intent_dir="$2" version="$3"; shift 3
  local platforms=("$@")
  local base="$WORK/$version"
  rm -rf "$base"
  mkdir -p "$base/evidence"

  head_ "$label — $version (${platforms[*]})"

  "$NOVAREL" plan --intent-dir "$intent_dir" --version "$version" --out "$base/plan.json" \
    || { bad "plan"; return 1; }
  ok "plan derived from the intent's claims"

  synth_descriptors "$base/descriptors" "$version" "${platforms[@]}"

  # Every pre-publication gate in the plan produces a statement. The workflow
  # does exactly this; if a gate the plan schedules produced nothing, the lock
  # below would refuse.
  local gate
  while read -r gate; do
    "$NOVAREL" evidence --plan "$base/plan.json" --gate "$gate" \
      --commit "$COMMIT" --artifacts "$base/descriptors" \
      --started "$BUILT_AT" --out "$base/evidence/$gate.json" >/dev/null \
      || { bad "evidence for $gate"; return 1; }
  done < <(python3 -c 'import json,sys
p = json.load(open(sys.argv[1]))
for g in p["pre_publication_gates"]:
    print(g["gate"])' "$base/plan.json")
  ok "one evidence statement per scheduled gate ($(ls "$base/evidence" | wc -l) of them)"

  "$NOVAREL" bundle --intent-dir "$intent_dir" --version "$version" \
    --out "$base/bundle" --evidence "$base/evidence" >/dev/null \
    || { bad "bundle"; return 1; }
  ok "bundle assembled"

  "$NOVAREL" lock --intent-dir "$intent_dir" --version "$version" \
    --commit "$COMMIT" --bundle "$base/bundle" --artifacts "$base/descriptors" \
    --evidence "$base/evidence" --built-at "$BUILT_AT" \
    --out "$base/bundle/lock.json" > "$base/lock.out" \
    || { cat "$base/lock.out"; bad "lock"; return 1; }
  local digest
  digest="$(awk '/^wrote /{print $3}' "$base/lock.out")"
  ok "lock built and the bundle verifies against it — $digest"

  # Deterministic: the same inputs and the same --built-at produce the same
  # bytes. Without this the lock digest a reviewer approves is not the digest an
  # operator downloads.
  "$NOVAREL" lock --intent-dir "$intent_dir" --version "$version" \
    --commit "$COMMIT" --bundle "$base/bundle" --artifacts "$base/descriptors" \
    --evidence "$base/evidence" --built-at "$BUILT_AT" \
    --out "$base/lock-again.json" > "$base/lock2.out"
  if [ "$(awk '/^wrote /{print $3}' "$base/lock2.out")" = "$digest" ]; then
    ok "rebuilding the lock from the same inputs produces the same digest"
  else
    bad "the lock is not deterministic; a reviewer cannot approve a digest that moves"
  fi

  "$NOVAREL" summary --intent-dir "$intent_dir" --version "$version" \
    --lock "$base/bundle/lock.json" --evidence "$base/evidence" \
    --out "$base/summary.md" || { bad "summary"; return 1; }
  if grep -q "$digest" "$base/summary.md" && grep -q "What approving does" "$base/summary.md"; then
    ok "the approval summary names the lock digest and says what approving does"
  else
    bad "the approval summary is missing the lock digest or the consequences"
  fi

  # The post-publication gate is scheduled and is NOT in the lock.
  local post
  post="$(python3 -c 'import json,sys
p = json.load(open(sys.argv[1]))
print(",".join(g["gate"] for g in p["post_publication_gates"]))' "$base/plan.json")"
  if [ -n "$post" ]; then
    ok "the plan schedules a post-publication gate ($post)"
  else
    bad "no post-publication gate; completion state 4 would be unreachable"
  fi
  if grep -q "$post" "$base/bundle/lock.json"; then
    bad "the post-publication gate reached the lock"
  else
    ok "and it does not appear in the lock, which was signed before it can run"
  fi

  printf '%s' "$digest" > "$base/digest"
}

rehearse "the release being cut" releases/intent v0.3.0 linux/amd64 || true
rehearse "a synthetic second release" internal/release/testdata/second-release v0.3.1 \
  linux/amd64 linux/arm64 || true

# ---------------------------------------------------------------------------
# The refusals. Each is a way a lock could come to say something untrue.
# ---------------------------------------------------------------------------

head_ "refusals"

A="$WORK/v0.3.0"
B="$WORK/v0.3.1"

# 1. Evidence gathered against the OTHER release's intent.
cp "$B/evidence/upgrade-candidate-e2e.json" "$WORK/foreign.json"
mkdir -p "$WORK/mixed-evidence"
cp "$A"/evidence/*.json "$WORK/mixed-evidence/"
cp "$WORK/foreign.json" "$WORK/mixed-evidence/upgrade-candidate-e2e.json"
if "$NOVAREL" lock --intent-dir releases/intent --version v0.3.0 --commit "$COMMIT" \
     --bundle "$A/bundle" --artifacts "$A/descriptors" --evidence "$WORK/mixed-evidence" \
     --built-at "$BUILT_AT" --out "$WORK/bad-lock.json" >/dev/null 2>&1; then
  bad "the lock accepted evidence gathered against a different reviewed decision"
else
  ok "the lock refuses evidence whose intent digest is another release's"
fi

# 2. A gate that SKIPPED cannot prove a claim.
mkdir -p "$WORK/skipped"
cp "$A"/evidence/*.json "$WORK/skipped/"
"$NOVAREL" evidence --plan "$A/plan.json" --gate upgrade-candidate-e2e \
  --commit "$COMMIT" --artifacts "$A/descriptors" --outcome skipped \
  --detail "no baseline deployment on this runner" \
  --out "$WORK/skipped/upgrade-candidate-e2e.json" >/dev/null
if "$NOVAREL" lock --intent-dir releases/intent --version v0.3.0 --commit "$COMMIT" \
     --bundle "$A/bundle" --artifacts "$A/descriptors" --evidence "$WORK/skipped" \
     --built-at "$BUILT_AT" --out "$WORK/bad-lock.json" >/dev/null 2>&1; then
  bad "a skipped gate proved a claim"
else
  ok "a skipped gate cannot prove a claim, so the lock refuses"
fi

# 3. Descriptors for the WRONG platform set.
synth_descriptors "$WORK/wrong-platform" v0.3.0 linux/arm64
if "$NOVAREL" lock --intent-dir releases/intent --version v0.3.0 --commit "$COMMIT" \
     --bundle "$A/bundle" --artifacts "$WORK/wrong-platform" --evidence "$A/evidence" \
     --built-at "$BUILT_AT" --out "$WORK/bad-lock.json" >/dev/null 2>&1; then
  bad "the lock accepted artifacts built for a platform the intent never declared"
else
  ok "the lock refuses a platform set the intent does not declare"
fi

# 4. A multi-platform intent with single-platform artifacts — the accidental
#    single-platform release, which is the failure that has no error message.
synth_descriptors "$WORK/one-platform" v0.3.1 linux/amd64
if "$NOVAREL" lock --intent-dir internal/release/testdata/second-release --version v0.3.1 \
     --commit "$COMMIT" --bundle "$B/bundle" --artifacts "$WORK/one-platform" \
     --evidence "$B/evidence" --built-at "$BUILT_AT" --out "$WORK/bad-lock.json" >/dev/null 2>&1; then
  bad "a release declaring two platforms shipped one, silently"
else
  ok "a release declaring two platforms cannot ship one"
fi

# 5. Evidence about a DIFFERENT build of the same release.
synth_descriptors "$WORK/other-build" v0.3.0-rebuilt linux/amd64
if "$NOVAREL" lock --intent-dir releases/intent --version v0.3.0 --commit "$COMMIT" \
     --bundle "$A/bundle" --artifacts "$WORK/other-build" --evidence "$A/evidence" \
     --built-at "$BUILT_AT" --out "$WORK/bad-lock.json" >/dev/null 2>&1; then
  bad "evidence about one build proved a different build"
else
  ok "evidence about one build cannot prove another"
fi

# 6. Dispatching for a version that has no reviewed intent.
if "$NOVAREL" plan --intent-dir releases/intent --version v9.9.9 >/dev/null 2>&1; then
  bad "planned a release with no checked-in intent"
else
  ok "a version with no reviewed intent cannot be planned"
fi

# 7. The two releases produce DIFFERENT locks. A pipeline that ignored --version
#    would pass everything above while cutting the same release twice.
if [ "$(cat "$A/digest")" != "$(cat "$B/digest")" ]; then
  ok "the two releases produce different locks"
else
  bad "both releases produced the same lock; --version is being ignored"
fi

# 8. The plans differ in the gates they schedule, which is the property that
#    makes deriving them from the intent worth anything.
gates_a="$(python3 -c 'import json,sys; print(",".join(g["gate"] for g in json.load(open(sys.argv[1]))["pre_publication_gates"]))' "$A/plan.json")"
gates_b="$(python3 -c 'import json,sys; print(",".join(g["gate"] for g in json.load(open(sys.argv[1]))["pre_publication_gates"]))' "$B/plan.json")"
if [ "$gates_a" != "$gates_b" ]; then
  ok "the two releases schedule different gates ($gates_a vs $gates_b)"
else
  bad "both releases scheduled the same gates; the plan is not derived from the intent"
fi

echo
printf 'release-pipeline-rehearsal: %d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
echo "OK: the release document pipeline completes for two different releases, and refuses"
echo "    every way a lock could come to say something untrue"
