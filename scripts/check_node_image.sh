#!/usr/bin/env bash
# Fails if the donor image contains operator-only artifacts. Exports the image
# rootfs and greps the file list for forbidden patterns. Run after the image is
# built (make node-image).
set -euo pipefail
IMAGE="${1:-nova-node:dev}"

forbidden='libvips|libvips-dev|/novactl|/migrate|/coordinator|node_modules|/usr/bin/curl|/usr/bin/wget|master-key'

cid="$(docker create "$IMAGE")"
trap 'docker rm -f "$cid" >/dev/null 2>&1 || true' EXIT
listing="$(docker export "$cid" | tar -t 2>/dev/null)"

if hits="$(printf '%s\n' "$listing" | grep -E "$forbidden" || true)"; [ -n "$hits" ]; then
  echo "FAIL: donor image contains forbidden artifact(s):" >&2
  printf '  %s\n' "$hits" >&2
  exit 1
fi
echo "OK: donor image inventory clean"
