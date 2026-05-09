# IPFS Daemon Hardening

Status: **Phase 0 — normative.** Donor pinning nodes refuse to start
unless the embedded Kubo daemon's configuration satisfies the rules
below. The coordinator's own embedded Kubo runs the same checks.

## Purpose

A default Kubo install joins the public IPFS DHT, broadcasts every
CID it provides, listens on mDNS, and exposes a Web UI. None of that
is acceptable for Nova's private federation, where:

- Donor nodes must be unable to leak the federation's CID set to the
  outside world.
- Network observers must not be able to identify Nova nodes from
  bootstrap traffic.
- The Kubo HTTP API and Gateway must never bind to a non-loopback
  interface.

This document specifies the exact configuration that the donor and
coordinator validators enforce on startup.

## Required configuration

The hardened `~/.ipfs/config` (or operator-specified equivalent) must
have at minimum the following keys set to the listed values. The
config validator reads the live config and refuses to start the
daemon if any rule is violated.

```jsonc
{
  // No public bootstrap peers. The operator supplies private peers
  // via the federation's own bootstrap mechanism (see below).
  "Bootstrap": [],

  // No public DHT. "none" disables DHT entirely; "dhtclient" can be
  // used inside a private swarm if the operator wants peer discovery
  // via private bootstrap nodes.
  "Routing": { "Type": "none" },

  // No mDNS broadcast on the LAN.
  "Discovery": {
    "MDNS": { "Enabled": false }
  },

  // Do not announce providership to anyone. The orchestrator
  // tracks who pins what; we do not need DHT advertisement.
  "Provider": { "Strategy": "" },
  "Reprovider": { "Strategy": "" },

  // Loopback-only API and Gateway. The federation HTTP traffic
  // does not go through these; they are local-management interfaces.
  "Addresses": {
    "API":     "/ip4/127.0.0.1/tcp/5001",
    "Gateway": "/ip4/127.0.0.1/tcp/8080"
  },

  // Don't punch through NAT for outsiders.
  "Swarm": {
    "DisableNatPortMap": true
  }
}
```

## Private swarm key

The donor and coordinator daemons must share the operator's
`IPFS_SWARM_KEY`. With the swarm key in place, libp2p refuses to
exchange data with peers that lack it.

The swarm key file at `~/.ipfs/swarm.key`:

```
/key/swarm/psk/1.0.0/
/base16/
<64-character hex-encoded 256-bit secret>
```

The donor node's container reads the key from the env var
`IPFS_SWARM_KEY` and writes it into the file at boot. If
`IPFS_SWARM_KEY` is unset and `public_ipfs_dht: false` (the default),
**the donor refuses to start.**

Generate the key via:

```sh
( echo "/key/swarm/psk/1.0.0/" \
  ; echo "/base16/" \
  ; tr -dc 'a-f0-9' </dev/urandom | head -c64 ; echo ) > swarm.key
```

The same swarm key is shared with every donor in the federation. It
is not a secret in the cryptographic sense — anyone with the key
can attempt to peer — but it is a coarse-grained access control
that keeps the federation invisible to default IPFS clients.

## Bootstrap peer requirements

`Bootstrap` is empty by default per the rules above, which means a
fresh Kubo daemon has no peers to connect to. The federation
substitutes operator-supplied private bootstrap entries injected at
boot time:

```jsonc
{
  "Bootstrap": [
    "/ip4/10.42.0.1/tcp/4001/p2p/<coordinator-peer-id>"
  ]
}
```

The startup validator accepts only entries whose multiaddr resolves
to **either**:

1. The loopback range (`127.0.0.0/8`, `::1`).
2. An RFC 1918 private range (`10/8`, `172.16/12`, `192.168/16`).
3. The operator-configured Nebula overlay subnet (set via
   `node.yaml: nebula_subnet`).

Any entry pointing to a public IP — `bootstrap.libp2p.io`,
`node.ipfs.io`, anything in the Kubo defaults — is rejected. The
node logs the offending entry and refuses to start.

## Validator algorithm

`internal/ipfs.ValidateConfig(cfg KuboConfig, mode Mode) error` is
called before the daemon starts. It walks the rules below and
returns the first failure (with a precise error message naming the
key) or `nil` for pass.

| Rule | Required value when `mode == private` (default) | Required value when `mode == public_archival_dht` |
|------|--------------------------------------------------|--------------------------------------------------|
| `Discovery.MDNS.Enabled`  | `false`                       | `false` (operator may override) |
| `Provider.Strategy`       | `""` (empty)                  | `"all"` allowed |
| `Reprovider.Strategy`     | `""` (empty)                  | `"all"` allowed |
| `Routing.Type`            | `"none"` or `"dhtclient"`     | `"dht"`, `"dhtclient"`, or `"auto"` |
| `Addresses.API`           | starts with `/ip4/127.0.0.1/` | starts with `/ip4/127.0.0.1/` |
| `Addresses.Gateway`       | starts with `/ip4/127.0.0.1/` | starts with `/ip4/127.0.0.1/` |
| `Swarm.DisableNatPortMap` | `true`                        | unconstrained |
| `Bootstrap`               | every entry resolves to loopback / RFC 1918 / operator overlay | unconstrained |
| Swarm key file            | exists and is non-empty       | unconstrained |

The validator runs before `ipfs daemon` is launched. It does not
hot-reload — the daemon must be restarted (or the container
recreated) to apply changes.

## Public IPFS DHT mode

For deployments where joining the public IPFS network is the
intended behaviour (a public-facing `nova-archive` instance hosting
open data, for example), the operator sets
`coordinator.public_ipfs_dht: true` in `operator.yaml`. The validator
relaxes to the right-hand column above.

Public DHT mode is **opt-in**, requires explicit configuration in two
places (operator config and the donor `node.yaml`), and is logged
prominently at startup. It is not appropriate for any product that
hosts personal or potentially-infringing content.

## Operational notes

- The Kubo HTTP API is loopback-only; this means `ipfs` CLI commands
  must be run on the same host as the daemon. Operators who want
  remote management should tunnel via SSH, not by binding the API
  to a public interface.
- The Kubo Gateway (`/ip4/127.0.0.1/tcp/8080`) is **not** the same as
  Nova's read gateway. The coordinator's read gateway speaks the
  HTTP API documented in `openapi.yaml` and reads from the local
  Kubo via its loopback API.
- IPFS garbage collection is the donor's local concern and runs on
  Kubo's normal schedule. The federation protocol does not direct GC.

## Out of scope

- WireGuard or alternative mesh VPNs. Nova standardizes on Nebula;
  the validator does not check anything about the mesh layer.
- IPFS Cluster configuration. Cluster pin coordination is a separate
  layer; its hardening is enforced by `cluster-config-validate` in
  Phase 2.
- Multi-tenant Kubo (one daemon, many federations). Each operator
  runs their own coordinator + Kubo + Cluster + swarm key.
