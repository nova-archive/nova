#!/bin/sh
# Kubo hardening for a Nova donor, applied BEFORE the daemon starts.
#
# Nova runs a PRIVATE swarm. A donor that reaches the public DHT or public
# bootstrap peers leaks the existence and shape of the federation's blockstore,
# so this is part of the shipped artifact rather than an instruction a volunteer
# is trusted to follow. See docs/specs/KUBO_HARDENING.md.
set -eu

export IPFS_PATH="${IPFS_PATH:-/data/ipfs}"

if [ ! -f "$IPFS_PATH/config" ]; then
  ipfs init --profile=server >/dev/null
fi

# No public bootstrap peers: this swarm is reached only via the overlay.
ipfs bootstrap rm --all >/dev/null 2>&1 || true

# No public routing and no local peer discovery.
ipfs config Routing.Type none
ipfs config --json Discovery.MDNS.Enabled false
ipfs config --json Swarm.DisableNatPortMap true
ipfs config --json Swarm.RelayClient.Enabled false
ipfs config --json Swarm.EnableHolePunching false

# API and Gateway on loopback only. nova-node reaches the API because it shares
# this container's network namespace; nothing outside it can.
ipfs config Addresses.API "/ip4/127.0.0.1/tcp/5001"
ipfs config Addresses.Gateway "/ip4/127.0.0.1/tcp/8080"

# Announce nothing publicly.
ipfs config --json Addresses.NoAnnounce '["/ip4/0.0.0.0/ipcidr/0", "/ip6/::/ipcidr/0"]'

# The swarm key is mounted read-only and must be present for a private swarm.
if [ ! -f "${IPFS_SWARM_KEY_FILE:-/run/secrets/ipfs_swarm_key}" ]; then
  echo "kubo-init: swarm key missing — refusing to start a donor that would join the public network" >&2
  exit 1
fi
cp "${IPFS_SWARM_KEY_FILE:-/run/secrets/ipfs_swarm_key}" "$IPFS_PATH/swarm.key"
chmod 0400 "$IPFS_PATH/swarm.key"

exec ipfs daemon --migrate=true --routing=none
