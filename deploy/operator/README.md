# Nova Operator Deployment

This directory contains operator-side deployment artifacts (coordinator config,
compose overrides, etc.). Donors use `deploy/donor/` instead.

## Federation block in `operator.yaml`

```yaml
federation:
  listen_addr: "10.100.0.1:8443"   # Nebula overlay IP:port; never 0.0.0.0
  nebula_interface: nebula1         # guard: refuses to bind unless this interface exists
  federation_ca_path:   /etc/nova/federation/federation-ca.crt
  federation_cert_path: /etc/nova/federation/coordinator-federation.crt
  federation_key_path:  /run/secrets/nova_coordinator_federation_key
```

`listen_addr` must be an address on the `nebula_interface`. The coordinator
verifies this at startup and refuses to bind on a non-overlay address.

## Bootstrapping the CA and coordinator cert

Run once on the operator host (requires `DATABASE_URL` for the CA-seed record):

```sh
novactl node ca-init \
  --out-ca-cert   /etc/nova/federation/federation-ca.crt \
  --out-ca-key    /run/secrets/nova_ca_key \
  --out-coord-cert /etc/nova/federation/coordinator-federation.crt \
  --out-coord-key  /run/secrets/nova_coordinator_federation_key
```

This produces:
- `federation-ca.{crt,key}` — the operator CA (keep the key offline or in a
  secrets manager; only the `.crt` needs to be distributed to donors).
- `coordinator-federation.{crt,key}` — the coordinator's mTLS identity; point
  `operator.yaml` `federation_cert_path` / `federation_key_path` at these.

## Nebula sidecar

The coordinator runs a Nebula sidecar (same pattern as donors). The federation
listener binds the overlay address only. The `nebula_interface` guard enforces
this at boot — the process exits if the named interface is absent or the
`listen_addr` is not on it.

## Provisioning and revoking donors

Issue a new donor cert (requires `DATABASE_URL`):

```sh
novactl node issue --node-id <uuid> \
  --ca-cert /etc/nova/federation/federation-ca.crt \
  --ca-key  /run/secrets/nova_ca_key \
  --out-cert /tmp/donor-federation.crt \
  --out-key  /tmp/donor-federation.key
```

Generate a Nebula config template for the donor:

```sh
novactl node nebula-template --node-id <uuid> --out /tmp/donor-nebula-config.yaml
```

List registered nodes:

```sh
novactl node list
```

Revoke a compromised cert (marks the node revoked in the DB; coordinator
enforces at the next heartbeat):

```sh
novactl node revoke --node-id <uuid>
```

Rotate a donor's federation cert (issues replacement, marks old cert revoked):

```sh
novactl node rotate-cert --node-id <uuid> \
  --ca-cert /etc/nova/federation/federation-ca.crt \
  --ca-key  /run/secrets/nova_ca_key
```

All `novactl node` subcommands connect to the database via `DATABASE_URL`.
