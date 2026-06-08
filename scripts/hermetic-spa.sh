#!/usr/bin/env bash
# hermetic-spa.sh — fail if the built bundle declares any third-party
# asset load. Nova's threat model requires web bundles to make no third-party
# requests at runtime (no CDN fonts/scripts/analytics); this is the CI backstop
# alongside the coordinator's strict default-src 'self' CSP.
#
# Every external asset load (fonts, images, styles, scripts) is declared in the
# bundle's HTML (src/href/link) or CSS (url()/@import). Vite rewrites every
# bundled asset to a local /admin/assets/ path, so any external origin appearing
# in dist's HTML or CSS is a genuine third-party fetch and fails the build.
#
# JS string literals are intentionally NOT scanned: libraries embed
# documentation URLs (e.g. React's reactjs.org error links) that are not asset
# loads. A runtime fetch to a CDN would be blocked by connect-src 'self' anyway.
set -euo pipefail

dist="${1:-web/admin/dist}"
if [ ! -d "$dist" ]; then
  echo "hermetic-spa: '$dist' not found — build the bundle first" >&2
  exit 1
fi

# w3.org is the SVG/XML namespace URI (xmlns), never a network fetch.
hits="$(grep -rEnoI "https?://[A-Za-z0-9.-]+" \
  --include='*.html' --include='*.css' "$dist" 2>/dev/null \
  | grep -vE "https?://(www\.)?w3\.org" || true)"

if [ -n "$hits" ]; then
  echo "hermetic-spa: external origin(s) in bundle HTML/CSS:" >&2
  echo "$hits" >&2
  exit 1
fi

echo "hermetic-spa: clean ($dist)"
