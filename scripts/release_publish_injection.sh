#!/usr/bin/env bash
# scripts/release_publish_injection.sh — interrupt the release after every
# external write and prove a retry converges (P2-M7.3, amendment, gate
# `release-publish-injection`).
#
# Runner tier: pr-static. No Docker, no network, no registry, no GitHub.
#
# ============================================================================
# The property under test
# ============================================================================
#
# A release makes writes to three systems nobody can roll back on request: a
# container registry, a Git remote, and a GitHub Release. Between any two of
# them the runner can be evicted, the token can expire, the network can blip.
#
# The property that has to hold is not "it does not fail". It is:
#
#   for every point at which the sequence can be interrupted, re-running the
#   WHOLE sequence from the beginning finishes it, and the end state is
#   identical to the state an uninterrupted run would have produced.
#
# That is a property about a partially-written world, and the only way to have
# it is to enumerate the interruption points and try them. So this gate stands
# up a fake registry, a fake Git remote and a fake GitHub, counts the external
# writes, and replays the sequence once per interruption point.
#
# ============================================================================
# What the fakes are allowed to be
# ============================================================================
#
# They are stubs, not simulators. They model exactly the four things the real
# systems do that the script depends on: a tag resolves to bytes or it does not;
# a remote tag names a commit or it does not; a release is absent, draft or
# published; an asset is absent, complete or incomplete.
#
# The fake `docker` REFUSES to build. If a resumed release ever rebuilt, the run
# would fail here rather than in production with a digest the lock never saw.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PUBLISH="$ROOT/scripts/release-publish.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

PASS=0
FAIL=0
ok()   { PASS=$((PASS + 1)); printf '  ok   %s\n' "$*"; }
bad()  { FAIL=$((FAIL + 1)); printf '  FAIL %s\n' "$*"; }
head_() { printf '\n== %s\n' "$*"; }

VERSION="v9.9.9"
SHA="1111111111111111111111111111111111111111"
OTHER_SHA="2222222222222222222222222222222222222222"

# ---------------------------------------------------------------------------
# The fakes
# ---------------------------------------------------------------------------

mkdir -p "$WORK/bin"

cat > "$WORK/bin/docker" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
. "$FAKE_STATE/lib.sh"

case "${1:-} ${2:-}" in
  "buildx imagetools")
    shift 2
    case "${1:-}" in
      inspect)
        shift
        raw=0; ref=""; format=""
        while [ $# -gt 0 ]; do
          case "$1" in
            --raw) raw=1; shift ;;
            --format) format="$2"; shift 2 ;;
            *) ref="$1"; shift ;;
          esac
        done
        digest="$(resolve_ref "$ref")" || { echo "fake docker: $ref not found" >&2; exit 1; }
        if [ -n "$format" ]; then
          # Only .Image is ever asked for, and only for a single manifest.
          echo '{"os":"linux","architecture":"amd64"}'
          exit 0
        fi
        [ "$raw" = 1 ] || { echo "fake docker: inspect without --raw" >&2; exit 1; }
        [ -f "$FAKE_STATE/blobs/${digest#sha256:}" ] \
          || { echo "fake docker: transient error reading $ref" >&2; exit 1; }
        if [ "${FAKE_LIE:-0}" = 1 ]; then
          # A registry that returns bytes other than the ones asked for. The
          # descriptor recorder hashes what it received rather than trusting
          # the digest it requested, so this has to be caught.
          printf '{"mediaType":"application/vnd.oci.image.manifest.v1+json","lie":true}'
          exit 0
        fi
        cat "$FAKE_STATE/blobs/${digest#sha256:}"
        ;;
      create)
        shift
        dst=""; src=""
        while [ $# -gt 0 ]; do
          case "$1" in
            --tag) dst="$2"; shift 2 ;;
            *) src="$1"; shift ;;
          esac
        done
        external_write "imagetools create $dst"
        digest="$(resolve_ref "$src")" || { echo "fake docker: $src does not exist" >&2; exit 1; }
        if [ "${FAKE_REWRAP:-0}" = 1 ]; then
          # Model the real failure mode: `imagetools create` wraps a
          # single-platform manifest in a NEW index, changing the digest.
          printf '{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}' \
            > "$FAKE_STATE/blobs/rewrapped"
          d="sha256:$(python3 -c 'import hashlib,sys;print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$FAKE_STATE/blobs/rewrapped")"
          mv "$FAKE_STATE/blobs/rewrapped" "$FAKE_STATE/blobs/${d#sha256:}"
          digest="$d"
        fi
        set_tag "$dst" "$digest"
        ;;
      *) echo "fake docker: unsupported imagetools verb ${1:-}" >&2; exit 1 ;;
    esac
    ;;
  "buildx build"|"build "*|"build")
    echo "fake docker: a release must NEVER rebuild. Promotion adds a tag to a digest that" >&2
    echo "already exists; a rebuild would produce a digest the lock never committed to." >&2
    exit 97
    ;;
  *) echo "fake docker: unsupported command $*" >&2; exit 1 ;;
esac
FAKE

cat > "$WORK/bin/git" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
. "$FAKE_STATE/lib.sh"

case "${1:-}" in
  config) exit 0 ;;
  ls-remote)
    # git ls-remote origin refs/tags/X[^{}]
    ref="${3:-}"
    name="${ref#refs/tags/}"; name="${name%^\{\}}"
    if [ -f "$FAKE_STATE/remote_tags/$name" ]; then
      printf '%s\trefs/tags/%s\n' "$(cat "$FAKE_STATE/remote_tags/$name")" "$name"
    fi
    exit 0
    ;;
  rev-list)
    # git rev-list -n1 refs/tags/X
    ref="${3:-}"; name="${ref#refs/tags/}"
    [ -f "$FAKE_STATE/local_tags/$name" ] || exit 1
    cat "$FAKE_STATE/local_tags/$name"
    ;;
  tag)
    # git tag -a NAME SHA -m MSG
    name="${3:-}"; sha="${4:-}"
    [ -f "$FAKE_STATE/local_tags/$name" ] && { echo "fake git: tag exists" >&2; exit 1; }
    printf '%s' "$sha" > "$FAKE_STATE/local_tags/$name"
    ;;
  push)
    ref="${3:-}"; name="${ref#refs/tags/}"
    external_write "git push $name"
    [ -f "$FAKE_STATE/local_tags/$name" ] || { echo "fake git: no such local tag" >&2; exit 1; }
    if [ -f "$FAKE_STATE/remote_tags/$name" ]; then
      [ "$(cat "$FAKE_STATE/remote_tags/$name")" = "$(cat "$FAKE_STATE/local_tags/$name")" ] \
        || { echo "fake git: rejected, would move a tag" >&2; exit 1; }
      exit 0
    fi
    cp "$FAKE_STATE/local_tags/$name" "$FAKE_STATE/remote_tags/$name"
    ;;
  *) echo "fake git: unsupported command $*" >&2; exit 1 ;;
esac
FAKE

cat > "$WORK/bin/gh" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
. "$FAKE_STATE/lib.sh"

[ "${1:-}" = release ] || { echo "fake gh: unsupported $*" >&2; exit 1; }
verb="${2:-}"; version="${3:-}"; shift 3 || true
rel="$FAKE_STATE/releases/$version.json"

case "$verb" in
  view)
    [ -f "$rel" ] || exit 1
    cat "$rel"
    ;;
  create)
    external_write "gh release create $version"
    [ -f "$rel" ] && { echo "fake gh: release exists" >&2; exit 1; }
    target=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --target) target="$2"; shift 2 ;;
        --draft) shift ;;
        --title|--notes-file) shift 2 ;;
        *) shift ;;
      esac
    done
    mkdir -p "$FAKE_STATE/assets/$version"
    python3 -c '
import json, sys
json.dump({"isDraft": True, "tagName": sys.argv[2], "targetCommitish": sys.argv[3], "assets": []},
          open(sys.argv[1], "w"))
' "$rel" "$version" "$target"
    ;;
  upload)
    path="${1:-}"
    external_write "gh release upload $(basename "$path")"
    [ -f "$rel" ] || { echo "fake gh: no such release" >&2; exit 1; }
    name="$(basename "$path")"
    mkdir -p "$FAKE_STATE/assets/$version"
    cp "$path" "$FAKE_STATE/assets/$version/$name"
    python3 -c '
import json, sys
rel, name = sys.argv[1], sys.argv[2]
d = json.load(open(rel))
d["assets"] = [a for a in d["assets"] if a["name"] != name] + [{"name": name, "state": "uploaded"}]
json.dump(d, open(rel, "w"))
' "$rel" "$name"
    ;;
  download)
    pattern=""; dir="."
    while [ $# -gt 0 ]; do
      case "$1" in
        --pattern) pattern="$2"; shift 2 ;;
        --dir) dir="$2"; shift 2 ;;
        --clobber) shift ;;
        *) shift ;;
      esac
    done
    [ -f "$FAKE_STATE/assets/$version/$pattern" ] || exit 1
    mkdir -p "$dir"
    cp "$FAKE_STATE/assets/$version/$pattern" "$dir/$pattern"
    ;;
  delete-asset)
    name="${1:-}"
    external_write "gh release delete-asset $name"
    rm -f "$FAKE_STATE/assets/$version/$name"
    python3 -c '
import json, sys
rel, name = sys.argv[1], sys.argv[2]
d = json.load(open(rel))
d["assets"] = [a for a in d["assets"] if a["name"] != name]
json.dump(d, open(rel, "w"))
' "$rel" "$name"
    ;;
  edit)
    external_write "gh release edit $version"
    [ -f "$rel" ] || { echo "fake gh: no such release" >&2; exit 1; }
    python3 -c '
import json, sys
d = json.load(open(sys.argv[1])); d["isDraft"] = False; json.dump(d, open(sys.argv[1], "w"))
' "$rel"
    ;;
  *) echo "fake gh: unsupported verb $verb" >&2; exit 1 ;;
esac
FAKE

chmod +x "$WORK/bin/docker" "$WORK/bin/git" "$WORK/bin/gh"

# ---------------------------------------------------------------------------
# Fake-world state and its little library
# ---------------------------------------------------------------------------

write_lib() {
  cat > "$1/lib.sh" <<'LIB'
tagfile() { printf '%s/tags/%s' "$FAKE_STATE" "$(printf '%s' "$1" | tr '/:' '__')"; }
set_tag() { printf '%s' "$2" > "$(tagfile "$1")"; }
resolve_ref() {
  case "$1" in
    *@sha256:*) printf '%s' "sha256:${1##*@sha256:}" ;;
    *)
      f="$(tagfile "$1")"
      [ -f "$f" ] || return 1
      cat "$f"
      ;;
  esac
}
external_write() {
  n=$(( $(cat "$FAKE_STATE/writes") + 1 ))
  if [ -n "${FAKE_FAIL_AFTER:-}" ] && [ "$n" -gt "$FAKE_FAIL_AFTER" ]; then
    echo "fake: interrupted before external write #$n ($1)" >&2
    exit 42
  fi
  printf '%s' "$n" > "$FAKE_STATE/writes"
  printf '%s\n' "$1" >> "$FAKE_STATE/writelog"
}
LIB
}

# new_world resets the fake registry/remote/GitHub and seeds the candidate
# digests a build would have pushed.
new_world() {
  local state="$WORK/state"
  rm -rf "$state"
  mkdir -p "$state"/{blobs,tags,local_tags,remote_tags,releases,assets}
  printf '0' > "$state/writes"
  : > "$state/writelog"
  write_lib "$state"
  export FAKE_STATE="$state"

  # Three candidate manifests, one per artifact, each a distinct single-platform
  # manifest so a mix-up between them is visible.
  mkdir -p "$WORK/descriptors"
  rm -f "$WORK/descriptors"/*.json
  local name repo body digest
  for name in nova-coordinator nova-node nova-admin; do
    repo="ghcr.io/nova-archive/$name"
    body="{\"mediaType\":\"application/vnd.oci.image.manifest.v1+json\",\"artifact\":\"$name\"}"
    printf '%s' "$body" > "$state/blobs/tmp"
    digest="sha256:$(python3 -c 'import hashlib,sys;print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$state/blobs/tmp")"
    mv "$state/blobs/tmp" "$state/blobs/${digest#sha256:}"
    python3 - "$WORK/descriptors/$name.json" "$repo" "$digest" "${#body}" <<'PY'
import json, sys
path, repo, digest, size = sys.argv[1:5]
json.dump({
    "repository": repo,
    "descriptor": {
        "mediaType": "application/vnd.oci.image.manifest.v1+json",
        "digest": digest,
        "size": int(size),
        "platform": {"architecture": "amd64", "os": "linux"},
    },
}, open(path, "w"), indent=2)
PY
  done

  mkdir -p "$WORK/bundle"
  printf 'lock for %s\n' "$VERSION" > "$WORK/bundle/lock.json"
  printf 'notes\n' > "$WORK/bundle/UPGRADING.md"
}

# run_sequence performs the whole publication, exactly as release.yml does.
run_sequence() {
  "$PUBLISH" promote-oci --version "$VERSION" --descriptors "$WORK/descriptors" >/dev/null &&
  "$PUBLISH" git-tag --version "$VERSION" --sha "$SHA" >/dev/null &&
  "$PUBLISH" publish --version "$VERSION" --sha "$SHA" \
    --notes "$WORK/bundle/UPGRADING.md" \
    --asset "$WORK/bundle/lock.json" >/dev/null
}

# world_fingerprint is everything an observer outside the release can see.
world_fingerprint() {
  {
    echo "--- tags"
    for f in "$FAKE_STATE"/tags/*; do
      [ -e "$f" ] || continue
      printf '%s %s\n' "$(basename "$f")" "$(cat "$f")"
    done | sort
    echo "--- remote tags"
    for f in "$FAKE_STATE"/remote_tags/*; do
      [ -e "$f" ] || continue
      printf '%s %s\n' "$(basename "$f")" "$(cat "$f")"
    done | sort
    echo "--- releases"
    for f in "$FAKE_STATE"/releases/*; do
      [ -e "$f" ] || continue
      python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(d["tagName"], d["isDraft"], d["targetCommitish"], sorted(a["name"] for a in d["assets"]))' "$f"
    done | sort
    echo "--- assets"
    find "$FAKE_STATE/assets" -type f -exec sh -c 'printf "%s %s\n" "$(basename "$1")" "$(sha256sum < "$1" | cut -d" " -f1)"' _ {} \; | sort
  } 2>/dev/null
}

export PATH="$WORK/bin:$PATH"
export NOVA_RELEASE_DOCKER="$WORK/bin/docker"
export NOVA_RELEASE_GIT="$WORK/bin/git"
export NOVA_RELEASE_GH="$WORK/bin/gh"

# ---------------------------------------------------------------------------
# 1. The uninterrupted run, which is the reference state
# ---------------------------------------------------------------------------

head_ "an uninterrupted release"
new_world
unset FAKE_FAIL_AFTER
if run_sequence; then ok "the full sequence succeeds"; else bad "the full sequence failed"; fi
REFERENCE="$(world_fingerprint)"
TOTAL_WRITES="$(cat "$FAKE_STATE/writes")"
printf '     %s external writes: %s\n' "$TOTAL_WRITES" "$(tr '\n' ',' < "$FAKE_STATE/writelog")"

if "$PUBLISH" verify-published --version "$VERSION" --descriptors "$WORK/descriptors" >/dev/null; then
  ok "the published tags read back as the locked descriptors"
else
  bad "verify-published rejected an uninterrupted release"
fi

# ---------------------------------------------------------------------------
# 2. Idempotence: the same sequence, again, changes nothing
# ---------------------------------------------------------------------------

head_ "re-running a finished release"
if run_sequence; then ok "a completed release re-runs cleanly"; else bad "re-running a completed release failed"; fi
if [ "$(world_fingerprint)" = "$REFERENCE" ]; then
  ok "and changes nothing"
else
  bad "re-running a completed release changed the world"
fi

# ---------------------------------------------------------------------------
# 3. Interruption after every external write
# ---------------------------------------------------------------------------

# n is the number of external writes that SUCCEED before the interruption, so it
# runs from 0 — died before touching anything — to one short of the total, which
# is dying immediately before the final write. Interrupting after the last write
# is not an interruption; it is the uninterrupted run, already covered above.
head_ "interrupted at each of the $TOTAL_WRITES points before an external write"
n=0
while [ "$n" -lt "$TOTAL_WRITES" ]; do
  new_world
  export FAKE_FAIL_AFTER="$n"
  if run_sequence 2>/dev/null; then
    bad "interruption point $n: the sequence did not stop"
  fi
  interrupted_at="$(tail -n1 "$FAKE_STATE/writelog" 2>/dev/null || true)"
  interrupted_at="after ${interrupted_at:-nothing}"

  # The retry is a FRESH invocation with no memory of the first: same inputs,
  # no local tags carried over, nothing rebuilt.
  unset FAKE_FAIL_AFTER
  rm -rf "$FAKE_STATE/local_tags"; mkdir -p "$FAKE_STATE/local_tags"
  if run_sequence; then
    if [ "$(world_fingerprint)" = "$REFERENCE" ]; then
      ok "$n write(s) done ($interrupted_at) → retry converges to the reference state"
    else
      bad "$n write(s) done ($interrupted_at) → retry produced a DIFFERENT world"
      diff <(printf '%s\n' "$REFERENCE") <(world_fingerprint) | head -20
    fi
  else
    bad "$n write(s) done ($interrupted_at) → retry failed"
  fi
  n=$((n + 1))
done

# ---------------------------------------------------------------------------
# 4. The refusals. Each is a way a retry could quietly become an overwrite.
# ---------------------------------------------------------------------------

head_ "refusals"

new_world
# An OCI version tag that already resolves to a real, different manifest —
# somebody else's release, or a second build of the same version.
foreign='{"mediaType":"application/vnd.oci.image.manifest.v1+json","artifact":"someone-else"}'
printf '%s' "$foreign" > "$FAKE_STATE/blobs/tmp"
foreign_digest="$(python3 -c 'import hashlib,sys;print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$FAKE_STATE/blobs/tmp")"
mv "$FAKE_STATE/blobs/tmp" "$FAKE_STATE/blobs/$foreign_digest"
printf 'sha256:%s' "$foreign_digest" \
  > "$FAKE_STATE/tags/ghcr.io_nova-archive_nova-coordinator_$VERSION"
if "$PUBLISH" promote-oci --version "$VERSION" --descriptors "$WORK/descriptors" >/dev/null 2>&1; then
  bad "promote-oci overwrote an OCI tag pointing at a different digest"
else
  ok "promote-oci refuses an OCI tag that already resolves elsewhere"
fi

# A tag that EXISTS but cannot be read is not a tag that is absent. Creating it
# anyway would be the same overwrite, arrived at through a transient error.
new_world
printf 'sha256:%s' "$(printf 'unreadable' | sha256sum | cut -d' ' -f1)" \
  > "$FAKE_STATE/tags/ghcr.io_nova-archive_nova-coordinator_$VERSION"
if "$PUBLISH" promote-oci --version "$VERSION" --descriptors "$WORK/descriptors" >/dev/null 2>&1; then
  bad "promote-oci treated an unreadable tag as a missing one and overwrote it"
else
  ok "promote-oci refuses a tag it cannot read rather than assuming it is absent"
fi

new_world
printf '%s' "$OTHER_SHA" > "$FAKE_STATE/remote_tags/$VERSION"
if "$PUBLISH" git-tag --version "$VERSION" --sha "$SHA" >/dev/null 2>&1; then
  bad "git-tag moved a tag that already existed at another commit"
else
  ok "git-tag refuses a remote tag pointing at another commit"
fi

new_world
FAKE_REWRAP=1 "$PUBLISH" promote-oci --version "$VERSION" --descriptors "$WORK/descriptors" \
  >/dev/null 2>&1 && bad "promote-oci accepted a rewrapped manifest" \
  || ok "promote-oci refuses when the read-back digest differs (the imagetools rewrap)"

new_world
"$PUBLISH" promote-oci --version "$VERSION" --descriptors "$WORK/descriptors" >/dev/null
"$PUBLISH" git-tag --version "$VERSION" --sha "$SHA" >/dev/null
"$PUBLISH" publish --version "$VERSION" --sha "$SHA" --notes "$WORK/bundle/UPGRADING.md" \
  --asset "$WORK/bundle/lock.json" >/dev/null
printf 'a DIFFERENT lock\n' > "$WORK/bundle/lock.json"
if "$PUBLISH" publish --version "$VERSION" --sha "$SHA" --notes "$WORK/bundle/UPGRADING.md" \
     --asset "$WORK/bundle/lock.json" >/dev/null 2>&1; then
  bad "publish replaced an attached asset with different bytes"
else
  ok "publish refuses to replace an attached asset whose digest differs"
fi
printf 'lock for %s\n' "$VERSION" > "$WORK/bundle/lock.json"

new_world
"$PUBLISH" publish --version "$VERSION" --sha "$SHA" --notes "$WORK/bundle/UPGRADING.md" \
  --asset "$WORK/bundle/lock.json" >/dev/null
if "$PUBLISH" publish --version "$VERSION" --sha "$OTHER_SHA" --notes "$WORK/bundle/UPGRADING.md" \
     --asset "$WORK/bundle/lock.json" >/dev/null 2>&1; then
  bad "publish retargeted an existing release to another commit"
else
  ok "publish refuses to retarget an existing release"
fi

# An incomplete upload IS replaced — nobody can have downloaded it.
new_world
"$PUBLISH" publish --version "$VERSION" --sha "$SHA" --notes "$WORK/bundle/UPGRADING.md" \
  --asset "$WORK/bundle/lock.json" >/dev/null
python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
d["assets"] = [{"name": a["name"], "state": "starter"} for a in d["assets"]]
json.dump(d, open(sys.argv[1], "w"))
' "$FAKE_STATE/releases/$VERSION.json"
if "$PUBLISH" publish --version "$VERSION" --sha "$SHA" --notes "$WORK/bundle/UPGRADING.md" \
     --asset "$WORK/bundle/lock.json" >/dev/null 2>&1; then
  ok "publish replaces an asset whose upload never completed"
else
  bad "publish would not repair an incomplete upload"
fi

# ---------------------------------------------------------------------------

# ---------------------------------------------------------------------------
# 5. The descriptor recorder. It runs once per image in the release and its
#    output is what every later step compares against, so a mistake here is a
#    lock that describes something other than what was pushed.
# ---------------------------------------------------------------------------

head_ "recording descriptors"

new_world
DESCRIPTOR="$ROOT/scripts/release-descriptor.sh"
want_digest="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["descriptor"]["digest"])' \
  "$WORK/descriptors/nova-coordinator.json")"

if "$DESCRIPTOR" --repository ghcr.io/nova-archive/nova-coordinator \
     --digest "$want_digest" --out "$WORK/recorded.json" >/dev/null 2>&1 &&
   python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
desc = d["descriptor"]
assert d["repository"] == "ghcr.io/nova-archive/nova-coordinator", d
assert desc["digest"] == sys.argv[2], desc
assert desc["mediaType"], desc
assert desc["size"] > 0, desc
assert desc["platform"]["os"] == "linux", desc
assert desc["platform"]["architecture"] == "amd64", desc
' "$WORK/recorded.json" "$want_digest"; then
  ok "a single manifest is recorded with digest, mediaType, size AND platform"
else
  bad "the recorded descriptor is missing a field the lock requires"
fi

# A repository carrying a tag or a digest is refused: the reference is built
# from the repository and the digest, and one already carrying either would
# produce a reference nothing can resolve.
"$DESCRIPTOR" --repository ghcr.io/nova-archive/nova-coordinator@sha256:x \
  --digest "$want_digest" --out /dev/null >/dev/null 2>&1 \
  && bad "a repository carrying a digest was accepted" \
  || ok "a repository carrying a tag or digest is refused"

# The recorder hashes the bytes it RECEIVED. A registry that returns something
# else must not end up described in a signed lock.
FAKE_LIE=1 "$DESCRIPTOR" --repository ghcr.io/nova-archive/nova-coordinator \
  --digest "$want_digest" --out /dev/null >/dev/null 2>&1 \
  && bad "the recorder trusted the digest it asked for over the bytes it got" \
  || ok "the recorder refuses when the returned bytes hash to something else"

echo
printf 'release-publish-injection: %d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
echo "OK: every external write is safe to interrupt, and no retry ever rebuilds or overwrites"
