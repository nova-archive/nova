#!/usr/bin/env bash
# scripts/release-publish.sh — every EXTERNAL WRITE a release makes, and the
# rules that make each one safe to retry (P2-M7.3, amendment to D-M7.3-16).
#
# ============================================================================
# Why this is not in the workflow YAML
# ============================================================================
#
# These four operations — promoting an OCI tag, creating a Git tag, drafting a
# release, attaching assets — are the only steps in the pipeline that write
# somewhere nobody can take it back from. They are also the steps most likely to
# be interrupted: a runner is evicted, a token expires, a network blips.
#
# In YAML they could not be tested. Here they run against a fake `docker`, `git`
# and `gh` (see scripts/release_publish_injection.sh), which interrupts the
# sequence after each external write and asserts that a retry CONVERGES rather
# than duplicating, overwriting, or rebuilding.
#
# ============================================================================
# The rule for every operation: no-overwrite, exact-match resume
# ============================================================================
#
# Each command below asks the same question in its own vocabulary:
#
#   Does this already exist?
#     no  → create it, then read it back and compare
#     yes → is it EXACTLY what this release requires?
#             yes → accept it and move on. This is the retry path.
#             no  → refuse. Never overwrite.
#
# The refusal matters more than the creation. A release is immutable; a tag that
# already points somewhere else is either a different release or an attack, and
# "make it point here instead" is the wrong answer to both.
#
# ============================================================================
# Nothing here rebuilds
# ============================================================================
#
# Promotion adds a tag to a digest that already exists. It never invokes a
# build, because a rebuild would produce a different digest than the one the
# lock committed to — stamping a version alters the image config, which alters
# the manifest digest. A resumed release finalizes the candidate it started
# with, or it fails.
set -euo pipefail

DOCKER="${NOVA_RELEASE_DOCKER:-docker}"
GIT="${NOVA_RELEASE_GIT:-git}"
GH="${NOVA_RELEASE_GH:-gh}"
PYTHON="${NOVA_RELEASE_PYTHON:-python3}"

die() { printf 'release-publish: %s\n' "$*" >&2; exit 1; }
note() { printf 'release-publish: %s\n' "$*" >&2; }

usage() {
  cat >&2 <<'EOF'
usage: release-publish.sh <command> [options]

  candidate-tag  --version V --sha SHA
      Print the OCI tag a candidate build pushes to. Carries BOTH the version
      and the commit: two versions cut from one commit would otherwise collide
      on a single candidate tag, and the second would silently promote the
      first's digests.

  promote-oci    --version V --descriptors DIR
      Add the version tag to each locked digest, then read it back and compare
      the FULL descriptor. Accepts an existing tag only if it already resolves
      to exactly that descriptor.

  git-tag        --version V --sha SHA
      Create and push the annotated product tag. Accepts an existing tag only
      if it already points at exactly that commit.

  publish        --version V --sha SHA --notes FILE --asset PATH [--asset PATH]...
      Draft the release if it does not exist, attach each asset (comparing the
      digest of any that is already there), then publish. Resumes a draft; never
      recreates one.

  verify-published --version V --descriptors DIR
      Re-resolve the published version tags and compare full descriptors. Safe
      to run repeatedly, writes nothing.
EOF
  exit 2
}

# ---------------------------------------------------------------------------
# Shared helpers
# ---------------------------------------------------------------------------

# descriptor_field reads one field out of a recorded descriptor.
descriptor_field() {
  "$PYTHON" -c 'import json,sys; d=json.load(open(sys.argv[1])); print(json.loads(json.dumps(d))["descriptor"].get(sys.argv[2], ""))' "$1" "$2"
}

repository_field() {
  "$PYTHON" -c 'import json,sys; print(json.load(open(sys.argv[1]))["repository"])' "$1"
}

# artifact_names lists the descriptors present, in a stable order.
artifact_names() {
  local dir="$1" f
  for f in "$dir"/*.json; do
    [ -e "$f" ] || die "no descriptors under $dir; the candidate job writes these"
    basename "$f" .json
  done | sort
}

sha256_of() { "$PYTHON" -c 'import hashlib,sys; print("sha256:"+hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$1"; }

# resolve_tag prints "digest mediaType size" for an OCI reference.
#
# Three outcomes, not two:
#
#   0  it resolves, and the triple is on stdout
#   1  it is genuinely ABSENT
#   2  it could not be read, and whether it exists is UNKNOWN
#
# The third is why this is not a boolean. "Could not resolve" has two causes
# with opposite correct responses: a tag that does not exist should be created,
# and a tag that exists but could not be read — a transient registry error, an
# expired token — must stop the release. Collapsing them creates the tag anyway,
# which is exactly the overwrite this script exists to refuse.
#
# It cannot signal the third by calling `die`, either: every caller reads it
# through a command substitution, and an exit inside one only kills the
# subshell. A refusal that the caller can swallow is not a refusal.
resolve_tag() {
  local ref="$1" raw err msg
  err="$(mktemp)"
  if raw="$("$DOCKER" buildx imagetools inspect --raw "$ref" 2>"$err")"; then
    rm -f "$err"
    printf '%s' "$raw" | "$PYTHON" -c '
import hashlib, json, sys
raw = sys.stdin.buffer.read()
doc = json.loads(raw)
print("sha256:" + hashlib.sha256(raw).hexdigest(), doc.get("mediaType", ""), len(raw))
'
    return 0
  fi
  msg="$(tr '\n' ' ' < "$err")"
  rm -f "$err"
  case "$msg" in
    *"not found"*|*MANIFEST_UNKNOWN*|*"404"*|*"no such"*|*"does not exist"*)
      return 1
      ;;
    *)
      note "cannot resolve $ref, and the registry did not say it is absent: $msg"
      return 2
      ;;
  esac
}

# resolve_present is how callers ask. It returns 0 when the reference resolves —
# leaving the triple in $resolved_triple — 1 when it is absent, and EXITS when
# the answer is unknown.
#
# It is called directly, never through `$( )`. Both halves of that matter: a
# command substitution would put the assignment to $resolved_triple in a
# subshell, where the caller never sees it, and it would put the `die` there
# too, where the caller could swallow it. A refusal the caller can ignore is not
# a refusal, and this script is mostly refusals.
resolved_triple=""
resolve_present() {
  local ref="$1" rc=0
  resolved_triple="$(resolve_tag "$ref")" || rc=$?
  case "$rc" in
    0) return 0 ;;
    1) resolved_triple=""; return 1 ;;
    *) die "refusing to continue against $ref: an unreadable tag is not a missing tag, and
  creating it anyway is how a retry becomes an overwrite." ;;
  esac
}

require_version() {
  case "$1" in
    v[0-9]*.[0-9]*.[0-9]*) ;;
    *) die "$1 is not a canonical vMAJOR.MINOR.PATCH version" ;;
  esac
}

require_sha() {
  case "$1" in
    [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;;
    *) die "$1 is not a full 40-character lowercase commit SHA" ;;
  esac
}

# ---------------------------------------------------------------------------
# candidate-tag
# ---------------------------------------------------------------------------

cmd_candidate_tag() {
  local version="" sha=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --version) version="${2:-}"; shift 2 ;;
      --sha)     sha="${2:-}"; shift 2 ;;
      *) die "candidate-tag: unknown argument $1" ;;
    esac
  done
  [ -n "$version" ] && [ -n "$sha" ] || usage
  require_version "$version"
  require_sha "$sha"
  # `.` is legal in an OCI tag; `+` is not, which is why the intent refuses a
  # version carrying build metadata.
  printf 'candidate-%s-%s\n' "$version" "$sha"
}

# ---------------------------------------------------------------------------
# promote-oci
# ---------------------------------------------------------------------------
#
# `docker buildx imagetools create` wraps a single-platform manifest in a NEW
# index unless told otherwise, which changes the digest the lock committed to.
# The read-back compares digest, mediaType AND size, because two of the three
# can match while the third does not: an index built around a manifest has a
# different mediaType and a different size, and a truncated response has the
# same mediaType and a different size.

promote_one() {
  local version="$1" desc="$2" name="$3"
  local repo want_digest want_media want_size dst got

  repo="$(repository_field "$desc")"
  want_digest="$(descriptor_field "$desc" digest)"
  want_media="$(descriptor_field "$desc" mediaType)"
  want_size="$(descriptor_field "$desc" size)"
  [ -n "$repo" ] && [ -n "$want_digest" ] && [ -n "$want_media" ] && [ -n "$want_size" ] \
    || die "$name: the recorded descriptor is incomplete"

  dst="${repo}:${version}"

  if resolve_present "$dst"; then
    got="$resolved_triple"
    if [ "$got" = "$want_digest $want_media $want_size" ]; then
      note "$dst already resolves to the locked descriptor; nothing to do"
      return 0
    fi
    die "$dst already exists and resolves to [$got], but this release requires
  [$want_digest $want_media $want_size].

  A release is immutable and this command never overwrites. Either that tag
  belongs to a different build of the same version — in which case cut a new
  version rather than moving the tag — or somebody else is publishing."
  fi

  note "tagging $dst -> $want_digest"
  "$DOCKER" buildx imagetools create --tag "$dst" "${repo}@${want_digest}" \
    || die "$name: could not create $dst"

  resolve_present "$dst" || die "$name: $dst does not resolve after being created"
  got="$resolved_triple"
  [ "$got" = "$want_digest $want_media $want_size" ] || die "$dst resolves to [$got], not the
  signed [$want_digest $want_media $want_size].

  Promotion rewrapped the manifest. Nothing is tagged in Git and no release
  exists; the release stops here."
  note "promoted and verified $dst"
}

cmd_promote_oci() {
  local version="" dir="" name
  while [ $# -gt 0 ]; do
    case "$1" in
      --version)     version="${2:-}"; shift 2 ;;
      --descriptors) dir="${2:-}"; shift 2 ;;
      *) die "promote-oci: unknown argument $1" ;;
    esac
  done
  [ -n "$version" ] && [ -n "$dir" ] || usage
  require_version "$version"
  [ -d "$dir" ] || die "$dir is not a directory"

  while read -r name; do
    promote_one "$version" "$dir/$name.json" "$name"
  done < <(artifact_names "$dir")
  echo "OK: every artifact is tagged $version and reads back as the locked descriptor"
}

cmd_verify_published() {
  local version="" dir="" name fail=0
  while [ $# -gt 0 ]; do
    case "$1" in
      --version)     version="${2:-}"; shift 2 ;;
      --descriptors) dir="${2:-}"; shift 2 ;;
      *) die "verify-published: unknown argument $1" ;;
    esac
  done
  [ -n "$version" ] && [ -n "$dir" ] || usage
  require_version "$version"

  while read -r name; do
    local desc repo want got
    desc="$dir/$name.json"
    repo="$(repository_field "$desc")"
    want="$(descriptor_field "$desc" digest) $(descriptor_field "$desc" mediaType) $(descriptor_field "$desc" size)"
    if ! resolve_present "${repo}:${version}"; then
      echo "FAIL: ${repo}:${version} does not resolve" >&2; fail=1; continue
    fi
    got="$resolved_triple"
    if [ "$got" != "$want" ]; then
      echo "FAIL: ${repo}:${version} resolves to [$got], the lock says [$want]" >&2; fail=1; continue
    fi
    echo "ok  ${repo}:${version}"
  done < <(artifact_names "$dir")
  [ "$fail" -eq 0 ] || die "the published tags do not match the lock"
  echo "OK: every published version tag resolves to the locked descriptor"
}

# ---------------------------------------------------------------------------
# git-tag
# ---------------------------------------------------------------------------
#
# The Git tag is the one write here that is expensive to retract in practice: it
# is fetched by everyone, referenced by URL, and a repository ruleset should
# forbid moving it. So it comes AFTER every OCI write, and it refuses an
# existing tag that points anywhere else.
#
# Both sides are checked. A local tag proves nothing about the remote — a rerun
# starts from a fresh checkout with no tags at all — and a remote tag that
# already matches must not cause the push to fail the release.

cmd_git_tag() {
  local version="" sha="" local_commit="" remote_commit=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --version) version="${2:-}"; shift 2 ;;
      --sha)     sha="${2:-}"; shift 2 ;;
      *) die "git-tag: unknown argument $1" ;;
    esac
  done
  [ -n "$version" ] && [ -n "$sha" ] || usage
  require_version "$version"
  require_sha "$sha"

  # `^{}` dereferences an annotated tag to the commit it names. Without it an
  # annotated tag compares as its own object id and never matches.
  remote_commit="$("$GIT" ls-remote origin "refs/tags/${version}^{}" 2>/dev/null | awk '{print $1}' | head -n1)"
  if [ -z "$remote_commit" ]; then
    remote_commit="$("$GIT" ls-remote origin "refs/tags/${version}" 2>/dev/null | awk '{print $1}' | head -n1)"
  fi
  if [ -n "$remote_commit" ]; then
    if [ "$remote_commit" = "$sha" ]; then
      note "origin already carries $version at $sha; nothing to do"
      echo "OK: $version is tagged at $sha"
      return 0
    fi
    die "origin already carries $version at $remote_commit, not $sha.

  A published tag is never moved. If that tag is a previous attempt at this
  release, the release it names is the one that exists; if it is a different
  release, cut a new version."
  fi

  local_commit="$("$GIT" rev-list -n1 "refs/tags/$version" 2>/dev/null || true)"
  if [ -n "$local_commit" ] && [ "$local_commit" != "$sha" ]; then
    die "a LOCAL tag $version points at $local_commit, not $sha. Refusing to push it."
  fi
  if [ -z "$local_commit" ]; then
    "$GIT" config user.name  "nova-release"
    "$GIT" config user.email "release@nova-archive.invalid"
    "$GIT" tag -a "$version" "$sha" -m "Nova $version" || die "could not create the tag"
  fi

  "$GIT" push origin "refs/tags/$version" || die "could not push $version"

  remote_commit="$("$GIT" ls-remote origin "refs/tags/${version}^{}" 2>/dev/null | awk '{print $1}' | head -n1)"
  [ "$remote_commit" = "$sha" ] || die "after pushing, origin resolves $version to
  ${remote_commit:-nothing}, not $sha"
  echo "OK: $version is tagged at $sha"
}

# ---------------------------------------------------------------------------
# publish
# ---------------------------------------------------------------------------
#
# Draft FIRST, attach, then publish. A release that exists before its lock is
# attached is one an operator can download without the document that
# authenticates it.
#
# Every step distinguishes three states, not two: absent, present-and-correct,
# present-and-different. Collapsing the last two into "present" is what turns a
# retry into an overwrite.

release_state() {
  # Prints "absent", or "draft"/"published" followed by the target commit.
  local version="$1" out
  out="$("$GH" release view "$version" --json isDraft,targetCommitish,tagName 2>/dev/null)" || {
    echo absent; return 0
  }
  printf '%s' "$out" | "$PYTHON" -c '
import json, sys
d = json.load(sys.stdin)
print("draft" if d.get("isDraft") else "published", d.get("targetCommitish", ""))
'
}

asset_state() {
  # Prints "absent", or "<state> <name>" for an asset already on the release.
  local version="$1" name="$2" out
  out="$("$GH" release view "$version" --json assets 2>/dev/null)" || { echo absent; return 0; }
  printf '%s' "$out" | "$PYTHON" -c '
import json, sys
name = sys.argv[1]
for a in json.load(sys.stdin).get("assets", []):
    if a.get("name") == name:
        print(a.get("state", "unknown"))
        break
else:
    print("absent")
' "$name"
}

attach_asset() {
  local version="$1" path="$2" name state tmp remote_digest local_digest
  name="$(basename "$path")"
  [ -f "$path" ] || die "asset $path does not exist"
  local_digest="$(sha256_of "$path")"

  state="$(asset_state "$version" "$name")"
  case "$state" in
    absent)
      note "uploading $name"
      "$GH" release upload "$version" "$path" || die "could not upload $name"
      ;;
    uploaded)
      # An asset that is already there must be BYTE IDENTICAL. Comparing the
      # digest rather than replacing is the difference between resuming a
      # release and quietly changing one somebody may already have downloaded.
      tmp="$(mktemp -d)"
      if "$GH" release download "$version" --pattern "$name" --dir "$tmp" --clobber 2>/dev/null; then
        remote_digest="$(sha256_of "$tmp/$name")"
        rm -rf "$tmp"
        if [ "$remote_digest" = "$local_digest" ]; then
          note "$name is already attached and identical"
          return 0
        fi
        die "$name is already attached to $version and differs:
  attached $remote_digest
  local    $local_digest
  A published asset is never replaced. This release was built from different
  bytes than the one already there."
      fi
      rm -rf "$tmp"
      die "$name is attached to $version but could not be downloaded for comparison;
  refusing to replace an asset whose contents are unknown"
      ;;
    *)
      # GitHub marks a partially uploaded asset with a state other than
      # "uploaded". That one IS safe to remove, because nobody can have
      # downloaded it: it was never complete.
      note "$name is in state '$state' — an incomplete upload; replacing it"
      "$GH" release delete-asset "$version" "$name" --yes || die "could not delete the incomplete $name"
      "$GH" release upload "$version" "$path" || die "could not upload $name"
      ;;
  esac
}

cmd_publish() {
  local version="" sha="" notes="" assets=() state target
  while [ $# -gt 0 ]; do
    case "$1" in
      --version) version="${2:-}"; shift 2 ;;
      --sha)     sha="${2:-}"; shift 2 ;;
      --notes)   notes="${2:-}"; shift 2 ;;
      --asset)   assets+=("${2:-}"); shift 2 ;;
      *) die "publish: unknown argument $1" ;;
    esac
  done
  [ -n "$version" ] && [ -n "$sha" ] && [ -n "$notes" ] || usage
  [ "${#assets[@]}" -gt 0 ] || die "a release with no assets is a release with no lock"
  require_version "$version"
  require_sha "$sha"
  [ -f "$notes" ] || die "$notes does not exist"

  read -r state target <<<"$(release_state "$version")"
  case "$state" in
    absent)
      note "drafting $version at $sha"
      "$GH" release create "$version" --draft --target "$sha" \
        --title "Nova $version" --notes-file "$notes" \
        || die "could not draft $version"
      ;;
    draft)
      note "resuming the existing draft for $version"
      ;;
    published)
      # Already published. Assets are still compared below, which is how a
      # retry that died between the last upload and the flip converges.
      note "$version is already published; verifying its assets and stopping"
      ;;
    *)
      die "cannot determine the state of $version"
      ;;
  esac

  if [ "$state" != absent ] && [ -n "$target" ] && [ "$target" != "$sha" ]; then
    die "$version already exists targeting $target, not $sha. Refusing to retarget a release."
  fi

  local a
  for a in "${assets[@]}"; do
    attach_asset "$version" "$a"
  done

  if [ "$state" != published ]; then
    note "publishing $version"
    "$GH" release edit "$version" --draft=false || die "could not publish $version"
  fi

  read -r state target <<<"$(release_state "$version")"
  [ "$state" = published ] || die "$version is still $state after publishing"
  echo "OK: $version is published with ${#assets[@]} asset(s)"
}

# ---------------------------------------------------------------------------

[ $# -gt 0 ] || usage
command="$1"; shift
case "$command" in
  candidate-tag)    cmd_candidate_tag "$@" ;;
  promote-oci)      cmd_promote_oci "$@" ;;
  verify-published) cmd_verify_published "$@" ;;
  git-tag)          cmd_git_tag "$@" ;;
  publish)          cmd_publish "$@" ;;
  -h|--help)        usage ;;
  *)                die "unknown command $command" ;;
esac
