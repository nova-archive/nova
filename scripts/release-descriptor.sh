#!/usr/bin/env bash
# scripts/release-descriptor.sh — record a pushed image as an OCI DESCRIPTOR
# (P2-M7.3, amendment to D-M7.3-2e).
#
# ============================================================================
# Why this is a script and not six lines of YAML
# ============================================================================
#
# It was six lines of YAML, and they recorded digest, mediaType and size. That
# is not a complete descriptor, and the two fields it omitted are the two that
# matter to a volunteer donor:
#
#   - PLATFORM. Nothing compared what was built against what the intent
#     declares, so the release was single-platform because `platforms:` said so,
#     not because anybody decided it. A donor on arm64 gets an exec-format error
#     and no way to tell "amd64 only, on purpose" from "the release forgot".
#
#   - CHILD DESCRIPTORS. An index IS its list of manifests. One recorded
#     without them cannot say which platforms it serves, and the read-back after
#     promotion has nothing to compare against.
#
# As a script it can be run against a fake `docker` by the failure-injection
# gate, which is how the refusals below are known to work rather than believed
# to.
#
# ============================================================================
# The digest is taken over the bytes, not read from the registry
# ============================================================================
#
# `imagetools inspect --raw` returns the exact manifest bytes. Hashing them
# locally means the descriptor is a statement about bytes this machine saw, not
# a value a registry asserted — and the whole promotion read-back exists because
# a registry can hand back something different from what was pushed.
set -euo pipefail

DOCKER="${NOVA_RELEASE_DOCKER:-docker}"
PYTHON="${NOVA_RELEASE_PYTHON:-python3}"

die() { printf 'release-descriptor: %s\n' "$*" >&2; exit 1; }

usage() {
  cat >&2 <<'EOF'
usage: release-descriptor.sh --repository REPO --digest sha256:... --out FILE

  --repository  the repository WITHOUT a tag or digest, e.g.
                ghcr.io/nova-archive/nova-coordinator
  --digest      the digest the build pushed
  --out         where to write the LockedArtifact JSON
EOF
  exit 2
}

REPOSITORY=""
DIGEST=""
OUT=""
while [ $# -gt 0 ]; do
  case "$1" in
    --repository) REPOSITORY="${2:-}"; shift 2 ;;
    --digest)     DIGEST="${2:-}"; shift 2 ;;
    --out)        OUT="${2:-}"; shift 2 ;;
    -h|--help)    usage ;;
    *)            die "unknown argument $1" ;;
  esac
done
[ -n "$REPOSITORY" ] && [ -n "$DIGEST" ] && [ -n "$OUT" ] || usage

case "$REPOSITORY" in
  *@*|*:*/*) die "--repository must carry neither a tag nor a digest: $REPOSITORY" ;;
esac
case "$DIGEST" in
  sha256:*) ;;
  *) die "--digest must be a sha256: digest, got $DIGEST" ;;
esac

REF="${REPOSITORY}@${DIGEST}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

"$DOCKER" buildx imagetools inspect --raw "$REF" > "$tmp/raw.json" \
  || die "cannot inspect $REF"

# The image CONFIG carries os/architecture for a single-platform manifest. An
# index carries platforms on its children instead, and asking for .Image there
# returns one arbitrary child's config — so it is only consulted below when the
# manifest is not an index.
"$DOCKER" buildx imagetools inspect "$REF" --format '{{json .Image}}' > "$tmp/image.json" 2>/dev/null \
  || echo 'null' > "$tmp/image.json"

"$PYTHON" - "$tmp/raw.json" "$tmp/image.json" "$REPOSITORY" "$DIGEST" > "$OUT" <<'PY'
import hashlib, json, sys

raw_path, image_path, repository, want_digest = sys.argv[1:5]
raw = open(raw_path, "rb").read()
got = "sha256:" + hashlib.sha256(raw).hexdigest()
if got != want_digest:
    sys.exit("release-descriptor: %s returned bytes hashing to %s, not the pushed %s. The "
             "registry handed back something other than what was pushed; nothing further may "
             "reference this digest." % (repository, got, want_digest))

doc = json.loads(raw)
media = doc.get("mediaType", "")
if not media:
    sys.exit("release-descriptor: the manifest carries no mediaType. A bare digest cannot say "
             "whether it names an index or a single manifest, and promotion treats the two "
             "differently.")

INDEX = ("application/vnd.oci.image.index.v1+json",
         "application/vnd.docker.distribution.manifest.list.v2+json")

artifact = {
    "repository": repository,
    "descriptor": {"mediaType": media, "digest": got, "size": len(raw)},
}

if media in INDEX:
    children = doc.get("manifests") or []
    if not children:
        sys.exit("release-descriptor: %s is an index with no manifests. An index IS its list of "
                 "children; one without them serves no platform." % repository)
    out = []
    for i, c in enumerate(children):
        for field in ("mediaType", "digest", "size"):
            if field not in c:
                sys.exit("release-descriptor: %s child %d has no %s" % (repository, i, field))
        child = {"mediaType": c["mediaType"], "digest": c["digest"], "size": c["size"]}
        if "platform" in c:
            child["platform"] = c["platform"]
        out.append(child)
    artifact["manifests"] = out
else:
    # A single manifest does not carry its own platform; the image config does.
    # Recording it is what lets the lock refuse a platform set nobody declared.
    image = json.load(open(image_path))
    if not isinstance(image, dict) or not image.get("os") or not image.get("architecture"):
        sys.exit("release-descriptor: %s is a single manifest and its image config declares no "
                 "os/architecture, so the descriptor cannot say what it runs on." % repository)
    platform = {"architecture": image["architecture"], "os": image["os"]}
    if image.get("variant"):
        platform["variant"] = image["variant"]
    if image.get("os.version"):
        platform["os.version"] = image["os.version"]
    artifact["descriptor"]["platform"] = platform

print(json.dumps(artifact, indent=2))
PY

echo "release-descriptor: wrote $OUT for $REF"
