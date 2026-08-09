# Operator deployment artifacts

Operator-side deployment files. Donors use `deploy/donor/` instead — and note
that `deploy/donor/` is **generated** from `internal/deploy/templates/`; edit the
templates and run `make gen-deploy`.

This file is artifact notes only. It is deliberately not a walkthrough: the
federation bootstrap is one documented path, and duplicating it here is how the
two drifted apart in the first place.

**Turning on federation:**
[`docs/quickstart/federation-operator.md`](../../docs/quickstart/federation-operator.md).
**Every setting:**
[`docs/reference/operator-configuration.md`](../../docs/reference/operator-configuration.md).

## Federation block in `operator.yaml`

```yaml
federation:
  listen_addr: "10.42.0.1:9443"    # overlay IP:port; never 0.0.0.0
  nebula_interface: nebula1        # the overlay interface to bind
  interface_wait_seconds: 120      # bounded boot wait; negative = fail fast
  federation_ca_path:   /etc/nova/federation/federation-ca.crt
  federation_cert_path: /etc/nova/federation/coordinator-federation.crt
  federation_key_path:  /run/secrets/nova_coordinator_federation_key
  repair_signing_key_path: /run/secrets/nova_repair_signing_key
  federation_client_cert_path: /etc/nova/federation/federation-client.crt
  federation_client_key_path:  /run/secrets/nova_coordinator_client_key
```

`listen_addr` must be an address on `nebula_interface`.

**Startup behaviour (P2-M7.2).** The coordinator does **not** fail to boot when
the interface is absent. It starts its public and admin listeners, waits up to
`interface_wait_seconds` for the overlay, then binds. If the wait elapses it
stays alive and serving with federation **not ready** rather than exiting —
exiting would deadlock the Nebula sidecar, which shares the coordinator's
network namespace and so needs the coordinator running before it can create the
interface.

Readiness is reported on the coordinator-only metrics listener:

```sh
curl -s http://127.0.0.1:2112/readyz
```

`operator.yaml` is read once at boot. Changing the `federation:` block requires
recreating the coordinator; it is not hot-reloaded.

## Port vocabulary

| Port | Purpose |
|---|---|
| `4242/udp` | Nebula lighthouse / rendezvous |
| `9443/tcp` | coordinator federation mTLS, overlay only |
| `9555/tcp` | donor read-source mTLS, overlay only |

The public nginx HTTPS port is separate and means nothing else.

## Two trust roots

Nebula PKI and Nova federation mTLS are **separate** and not interchangeable:

- **Nebula** certificates authorize membership of the overlay *network*.
- **Nova federation** certificates authorize the Nova *HTTP API*.

Neither CA private key may leave operator custody, and neither belongs in a
donor bundle.

## Certificate primitives

These are low-level building blocks. They are pure local file operations and do
**not** need `DATABASE_URL`.

```sh
novactl node ca-init --dir /etc/nova/federation \
  --coordinator-ip 10.42.0.1 --coordinator-dns nova.example.org

novactl node issue --dir /etc/nova/federation --name alice-desktop --out ./alice

novactl node issue-coordinator-client --dir /etc/nova/federation --out ./coordinator-client

novactl node nebula-template --name alice-desktop --nebula-ip 10.42.0.10/24 \
  --image ghcr.io/nova-archive/nova-node@sha256:REPLACE --out ./alice
```

`node issue` **generates** the node id; it is not caller-supplied.
`node nebula-template` requires a digest-pinned `--image` — generated artifacts
must never carry a mutable tag.

## Registry commands

These are DB-direct and **do** require `DATABASE_URL`.

```sh
novactl node list

novactl node revoke --id <uuid>

novactl node rotate-cert --id <uuid> --dir /etc/nova/federation

novactl node set-domain --id <uuid> --provider example-vps --asn 64500 --region us-east

novactl node drain --id <uuid>
novactl node undrain --id <uuid>
```

Revocation is enforced at the node's next request. Cert rotation is a downtime
cutover: the old certificate is refused as soon as the new fingerprint is
stored, so the donor must restart with the replacement bundle.

For the drain-vs-revoke-vs-suspend decision and the safe-to-revoke conditions,
see `docs/runbooks/donor-lifecycle.md`.
