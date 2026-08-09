# Operator configuration reference

Settings an operator can change, what they do, and the consequences.

For getting Nova running, see [the quickstart](../quickstart.md). For turning
on federation, see
[the federation quickstart](../quickstart/federation-operator.md).

The admin console's **Settings** screen exposes the commonly adjusted subset
with the same explanations, plus a read-only view of the full effective
configuration.

---

## Choosing a TLS mode

| Mode | What happens | Privacy note |
| --- | --- | --- |
| `dev-self-signed` | The wizard generates a throwaway CA + leaf. Browsers warn; `curl -k` accepts it. Dev and staging only. | Nothing leaves your machine. |
| `static` | You supply paths to your own fullchain + key PEMs (drop them in the config volume's TLS dir). You own renewal. | No third party is contacted; disclosure depends on where your cert came from. |
| `http-01` | Fully automated Let's Encrypt: the certbot sidecar writes a placeholder so nginx can start, obtains the real certificate on first boot, renews it on a 12-hour check loop, and nginx hot-reloads on every deploy. Zero manual certbot steps. Requires your hostname to resolve publicly and port 80 traffic to reach the stack (forward `:80 → :8442`). | Your hostname is published to public **Certificate Transparency logs** (crt.sh and friends). If that is a deanonymization concern, pick another mode. |
| `dns-01` / `onion` | The wizard renders the config and prints operator-handoff instructions: supply DNS-API credentials out of band (`dns-01`) or run Tor and supply the cert (`onion`). Not automated — see the per-mode guidance in [`legal/OPERATOR_CHECKLIST.md`](../legal/OPERATOR_CHECKLIST.md). | `dns-01` certs still land in CT logs but need no inbound port 80; `onion` keeps your service out of public DNS and CT entirely. |

---

## The `federation:` block

Written by `novactl federation init`. You should not need to hand-edit it.

```yaml
federation:
  listen_addr: "10.42.0.1:9443"
  nebula_interface: nebula1
  interface_wait_seconds: 120
  federation_ca_path:   /etc/nova/federation/federation-ca.crt
  federation_cert_path: /etc/nova/federation/coordinator-federation.crt
  federation_key_path:  /run/secrets/nova_coordinator_federation_key
  repair_signing_key_path: /run/secrets/nova_repair_signing_key
  federation_client_cert_path: /etc/nova/federation/federation-client.crt
  federation_client_key_path:  /run/secrets/nova_coordinator_client_key
```

| Field | Meaning |
|---|---|
| `listen_addr` | Overlay address for the federation API. Must be an address on `nebula_interface`; never `0.0.0.0`. |
| `nebula_interface` | The overlay interface. Empty disables the membership check entirely — not recommended. |
| `interface_wait_seconds` | How long to wait at boot for the interface. Default `120`. A **negative** value restores pre-M7.2 fail-fast behaviour. |
| `repair_signing_key_path` | Ed25519 seed for repair grants. Without it the coordinator still runs the control plane, but the source endpoint returns 503 and no grants are minted. |
| `federation_client_*` | The coordinator's own client identity, used to read from donors and to run possession audits. Omitting it disables donor-backed reads. |

### Startup behaviour

The coordinator does **not** fail to boot when the overlay interface is
missing. It starts its public and admin listeners, waits up to
`interface_wait_seconds`, then binds the federation listener. If the wait
elapses it stays alive and serving with federation *not ready*.

This matters because the Nebula sidecar shares the coordinator's network
namespace: it cannot create the interface until the coordinator is already
running. A coordinator that exited on a missing interface would deadlock its
own sidecar.

Readiness is on the operator-only metrics listener, never the public vhost:

```sh
curl -s http://127.0.0.1:2112/readyz
curl -s http://127.0.0.1:2112/metrics | grep nova_federation_listener_ready
```

`operator.yaml` is read once at boot. Changing this block requires recreating
the coordinator.

---

## Port vocabulary

| Port | Purpose |
|---|---|
| `4242/udp` | Nebula lighthouse / rendezvous |
| `9443/tcp` | Coordinator federation mTLS, overlay only |
| `9555/tcp` | Donor read-source mTLS, overlay only |
| `8442` / `8443` | Public HTTP / HTTPS (nginx) |
| `8445` | Admin console, loopback-published |
| `2112` | Prometheus metrics + `/readyz`, loopback by default |

---

## Low-level certificate commands

`novactl federation init` and `novactl node invite` are the documented path.
These primitives remain for unusual situations. They are pure local file
operations and need no `DATABASE_URL`.

```sh
novactl node ca-init --dir /etc/nova/federation \
  --coordinator-ip 10.42.0.1 --coordinator-dns nova.example.org

novactl node issue --dir /etc/nova/federation --name alice-desktop --out ./alice

novactl node issue-coordinator-client --dir /etc/nova/federation --out ./coordinator-client

novactl node nebula-template --name alice-desktop --nebula-ip 10.42.0.10/24 \
  --image ghcr.io/nova-archive/nova-node@sha256:REPLACE --out ./alice
```

`node issue` **generates** the node id. `nebula-template` requires a
digest-pinned `--image`.

Using these instead of `node invite` means you are responsible for assembling a
complete bundle yourself, and for not including anything you should not.
`node invite` refuses to write a bundle containing operator authority; these
primitives make no such promise.

---

## Recovery: destroying and recreating authority

**Read this whole section before running anything in it.**

There is deliberately no `--force`. Destroying a federation CA is not the same
kind of act as overwriting generated config, so it takes two flags:

```sh
novactl federation init --replace-authority --destroy-existing-federation ...
```

`--replace-authority` alone refuses whenever donors are registered, and names
how many would be orphaned. Every certificate you have issued becomes
untrusted, and every donor must re-enroll from scratch.

Even with both flags, an existing Kubo swarm key is **never** rotated. A new
swarm key would leave every current donor up, registered, and silently unable
to exchange data — a failure mode much worse than a loud one. If you genuinely
intend it, remove the key deliberately first.

**Before considering this:** if what you want is to adopt an existing hand-built
federation rather than replace it, use `--adopt-from` instead. See
[the federation quickstart](../quickstart/federation-operator.md#adopting-an-existing-setup).

---

## Backing up authority

The federation CA is the one thing Nova cannot rebuild.

```sh
docker run --rm -v nova_nova-fedpki:/pki:ro -v "$PWD":/out debian:bookworm-slim \
  tar czf /out/nova-fedpki-backup.tar.gz -C /pki .
```

Store it encrypted and off the machine. It contains issuance authority: anyone
holding it can mint donor identities for your federation.

---

## Headless / scripted first-run setup

Everything the wizard does is also available unattended via
`novactl setup --config-file`, which shares the same validation and crash-safe
commit ordering. Write an answers file:

```yaml
# answers.yaml — Nova first-run answers (see internal/setup/answers.go)
hostname: nova.example.org
contact_email: ops@example.org
display_name: Example Community Archive    # optional
admin_email: you@example.org
admin_password: use-a-long-passphrase      # 12 chars minimum
tls_mode: dev-self-signed                  # dev-self-signed|http-01|dns-01|static|onion
# cert_path: /etc/nova/tls/fullchain.pem   # static mode only
# key_path: /etc/nova/tls/privkey.pem      # static mode only
auth_mode: local                           # local|external
# issuer_url: https://idp.example.org      # external mode only
# client_id: nova-admin                    # external mode only
public_uploads: true
tos_url: https://nova.example.org/tos      # required when public_uploads: true
paranoid: false
```

Then run migrations + setup in a one-off coordinator container (compose starts
Postgres for you; the headless path needs no bootstrap token — it never opens
the network seam). This is an *alternative* first-run path: run it from a clean
slate, **instead of** the setup profile. If you already started the setup
profile, wipe it first with
`docker compose -f docker/docker-compose.yml --env-file docker/.env --profile setup down -v`
(pre-setup there is nothing to lose; the one-off container cannot write into a
secrets volume the setup boot has already claimed):

```sh
docker compose -f docker/docker-compose.yml --env-file docker/.env \
  run --rm -T --entrypoint /bin/sh \
  -v "$PWD/answers.yaml:/answers.yaml:ro" \
  coordinator -c "/usr/local/bin/migrate up && /usr/local/bin/novactl setup --config-file /answers.yaml"
```

Follow with the same prod-profile commands from the quickstart. The headless
path is also how you configure an **external OIDC** provider
(`auth_mode: external`), which the web wizard does not offer.

The same answers file drives CI: [`scripts/smoke.sh`](../../scripts/smoke.sh)
is a living end-to-end example of exactly this flow — headless setup, prod
profile, upload, read-back, transform, delete.

---

## See also

- [Operator checklist](../legal/OPERATOR_CHECKLIST.md) — the deep runbook
- [Donor lifecycle](../runbooks/donor-lifecycle.md) — drain, revoke, suspend
- [Failure drills](../runbooks/failure-drills.md)
- [Upgrading](../UPGRADING.md)
