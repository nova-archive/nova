#!/usr/bin/env bash
# scripts/refresh-docker-digests.sh — re-resolve the digest pins in
# docker/*.Dockerfile AND docker/docker-compose.yml runtime images.
#
# Every base image is pinned as FROM image:tag@sha256:... (P2-M7.1, OSSF
# Scorecard PinnedDependenciesID). This script re-resolves each tag to its
# CURRENT manifest-list digest and rewrites the pin in place, updating the
# "resolved YYYY-MM-DD" date in the header comment. Dependabot's docker
# ecosystem watches the same pins; this is the local/manual path.
#
# Resolution order: `docker buildx imagetools inspect` if available, else the
# registry HTTP API (Docker Hub + gcr.io — the only registries we pull from).
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
today="$(date +%Y-%m-%d)"

resolve_digest() { # <image:tag> -> sha256:...
  local ref="$1"
  if docker buildx version >/dev/null 2>&1; then
    docker buildx imagetools inspect "$ref" | awk '/^Digest:/{print $2; exit}'
    return
  fi
  local registry repo tag token url
  tag="${ref##*:}"
  local name="${ref%:*}"
  case "$name" in
    gcr.io/*)
      registry="gcr.io"
      repo="${name#gcr.io/}"
      token=$(curl -fsS "https://gcr.io/v2/token?service=gcr.io&scope=repository:${repo}:pull" \
        | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')
      url="https://gcr.io/v2/${repo}/manifests/${tag}"
      ;;
    *)
      if [[ "${name%%/*}" == *.* || "${name%%/*}" == *:* ]]; then # other third-party registry
        echo "refresh-docker-digests: no registry-API fallback for ${ref}; install docker buildx" >&2
        return 1
      fi
      registry="registry-1.docker.io"
      repo="$name"
      [[ "$repo" == */* ]] || repo="library/${repo}"
      token=$(curl -fsS "https://auth.docker.io/token?service=registry.docker.io&scope=repository:${repo}:pull" \
        | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')
      url="https://${registry}/v2/${repo}/manifests/${tag}"
      ;;
  esac
  curl -fsSI -H "Authorization: Bearer ${token}" \
    -H "Accept: application/vnd.oci.image.index.v1+json" \
    -H "Accept: application/vnd.docker.distribution.manifest.list.v2+json" \
    "$url" | tr -d '\r' | awk 'tolower($1)=="docker-content-digest:"{print $2; exit}'
}

any_changed=0
for df in "$repo_root"/docker/*.Dockerfile; do
  changed=0
  while IFS= read -r pinned; do
    ref="${pinned%@sha256:*}"                 # image:tag
    old="sha256:${pinned##*@sha256:}"         # sha256:...
    new="$(resolve_digest "$ref")"
    [[ "$new" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "bad digest for ${ref}: '${new}'" >&2; exit 1; }
    if [[ "$new" != "$old" ]]; then
      sed -i "s|${ref}@${old}|${ref}@${new}|" "$df"
      echo "$(basename "$df"): ${ref} ${old} -> ${new}"
      changed=1
      any_changed=1
    else
      echo "$(basename "$df"): ${ref} unchanged (${old})"
    fi
  done < <(grep -oE '^FROM [^ ]+@sha256:[0-9a-f]{64}' "$df" | cut -d' ' -f2 | sort -u)
  if [[ "$changed" == 1 ]]; then
    sed -i -E "s/(digest-pinned \(P2-M7\.1\); resolved )[0-9]{4}-[0-9]{2}-[0-9]{2}/\1${today}/" "$df"
  fi
done

# docker-compose.yml runtime images carry the same PinnedDependencies posture
# (P2-M7.1 security review). Format: `image: name:tag@sha256:...`. Duplicate
# refs (both nginx services) dedupe via sort -u and rewrite together.
compose="$repo_root/docker/docker-compose.yml"
if [[ -f "$compose" ]]; then
  while IFS= read -r ref; do
    old="$(grep -oE "${ref}@sha256:[0-9a-f]{64}" "$compose" | head -1 | sed -E 's/.*@(sha256:[0-9a-f]{64})/\1/')"
    new="$(resolve_digest "$ref")"
    [[ "$new" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "bad digest for ${ref}: '${new}'" >&2; exit 1; }
    if [[ "$new" != "$old" ]]; then
      sed -i "s|${ref}@${old}|${ref}@${new}|g" "$compose"
      echo "docker-compose.yml: ${ref} ${old} -> ${new}"
      any_changed=1
    else
      echo "docker-compose.yml: ${ref} unchanged (${old})"
    fi
  done < <(grep -oE 'image: [^ ]+@sha256:[0-9a-f]{64}' "$compose" | sed -E 's/image: ([^@]+)@.*/\1/' | sort -u)
fi

[[ "$any_changed" == 1 ]] && echo "Digests updated — rebuild and commit." || echo "All digests current."
