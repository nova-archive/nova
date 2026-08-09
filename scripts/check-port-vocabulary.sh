#!/usr/bin/env bash
# scripts/check-port-vocabulary.sh — the deployment vocabulary is fixed
# (P2-M7.2, D-M7.2-5):
#
#   4242/udp   Nebula lighthouse / rendezvous
#   9443/tcp   coordinator federation mTLS, overlay only
#   9555/tcp   donor read-source mTLS, overlay only
#
# 8443 means exactly one thing: the operator's PUBLIC nginx HTTPS port. Three
# artifacts previously disagreed — the operator README said the federation
# listener was on 8443, the donor node.yaml.example said 8443, and the
# templates said 9443 — which is especially confusing because 8443 was already
# taken. This gate stops that reoccurring.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

failures=0

# Federation/donor artifacts must never mention 8443.
donor_scope=(
  internal/deploy/templates
  deploy/donor
  deploy/operator
  docs/quickstart/donor.md
)

for target in "${donor_scope[@]}"; do
  [ -e "$target" ] || continue
  # ADDRESS form only (":8443"), so prose explaining what 8443 is for does not
  # trip the gate. `# port-vocabulary: ignore` opts a line out explicitly.
  if hits="$(grep -rn ":8443" "$target" 2>/dev/null | grep -v 'port-vocabulary: ignore')"; then
    echo "FAIL: :8443 used as an address in a federation/donor artifact (it is the PUBLIC nginx port):" >&2
    echo "$hits" | sed 's/^/      /' >&2
    failures=$((failures + 1))
  fi
done

# The federation endpoint must be 9443 wherever it is named.
if [ -f internal/deploy/templates/node.yaml.tmpl ]; then
  if ! grep -q '{{.FederationPort}}' internal/deploy/templates/node.yaml.tmpl; then
    echo "FAIL: node.yaml.tmpl should use {{.FederationPort}}, not a literal port" >&2
    failures=$((failures + 1))
  fi
fi

# The Go constants are the single source; assert they hold the canonical values.
if ! grep -q 'PortNebulaLighthouse = 4242' internal/deploy/templates.go ||
   ! grep -q 'PortFederationMTLS   = 9443' internal/deploy/templates.go ||
   ! grep -q 'PortReadSourceMTLS   = 9555' internal/deploy/templates.go; then
  echo "FAIL: internal/deploy port constants drifted from 4242/9443/9555" >&2
  failures=$((failures + 1))
fi

if [ "$failures" -gt 0 ]; then
  echo >&2
  echo "Port vocabulary: 4242 Nebula, 9443 federation, 9555 read-source." >&2
  echo "8443 is the public nginx HTTPS port and belongs nowhere else." >&2
  exit 1
fi

echo "OK: port vocabulary consistent (4242 nebula / 9443 federation / 9555 read-source)"
