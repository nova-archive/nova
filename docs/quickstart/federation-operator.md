# Turning on federation

This is the whole path from a running Nova operator to a donor storing your
data. Eight steps, about fifteen minutes.

Every knob you *could* set is in
[`docs/reference/operator-configuration.md`](../reference/operator-configuration.md).
This page has only what you need.

## Before you start

- Nova is already running (you followed [the quickstart](../quickstart.md)).
- Your host has `/dev/net/tun`. Most Linux servers do. Check:
  ```sh
  ls -l /dev/net/tun
  ```
  Expected: `crw-rw-rw- 1 root root 10, 200 ... /dev/net/tun`
  *If it fails:* run `sudo modprobe tun`, then check again.
- You know your server's public IP, and UDP port 4242 can reach it.
- You picked a private network range for the overlay. Use `10.42.0.0/24`
  unless it clashes with something you already run.

Throughout, replace `nova.example.org` with your hostname and `203.0.113.7`
with your server's public IP.

---

## 1. Create the federation identity

```sh
docker compose -f docker/docker-compose.yml \
               -f deploy/operator/compose.federation.yaml \
               --profile federation run --rm nova-admin \
  federation init \
    --overlay-cidr 10.42.0.0/24 \
    --operator-overlay-ip 10.42.0.1 \
    --lighthouse-public 203.0.113.7:4242 \
    --hostname nova.example.org
```

Expected:

```
federation ready at /var/lib/nova/fedpki/active
  created:  12
  adopted:  0
  existing: 0

  listen_addr:  10.42.0.1:9443
  ca:           sha256:118fe2a4...
  lighthouse:   203.0.113.7:4242
```

*If it fails on `/dev/net/tun`:* see the prerequisite above.
*If it fails on a route conflict:* something already uses that range. Pick a
different `--overlay-cidr`.

**Already have a hand-built federation?** See
[*Adopting an existing setup*](#adopting-an-existing-setup) below before
running this.

This is safe to run twice. A second run changes nothing and says
`created: 0`.

---

## 2. Restart with federation enabled

```sh
docker compose -f docker/docker-compose.yml \
               -f deploy/operator/compose.federation.yaml \
               --profile prod --profile federation up -d
```

Nova reads its configuration once at startup, so this restart is what picks up
the federation settings step 1 wrote.

---

## 3. Wait for the overlay

```sh
docker compose logs -f coordinator
```

You are looking for, in order:

```
federation.listener.waiting  iface=nebula1 listen=10.42.0.1:9443
federation listener bound    listen=10.42.0.1:9443
```

The first line is normal — the overlay takes a few seconds to come up, and
Nova waits for it rather than giving up. Your site keeps serving the whole
time.

*If you only ever see the first line:* the overlay did not start. Check
`docker compose logs nebula`.

---

## 4. Check everything before inviting anyone

```sh
docker compose -f docker/docker-compose.yml \
               -f deploy/operator/compose.federation.yaml \
               --profile federation run --rm nova-doctor \
  federation doctor --live
```

Expected: every line starts `ok`.

```
ok    [A] pki.parse       all certificate/key pairs parse and match
ok    [A] pki.sans        coordinator certificate names the configured hostname and overlay IP
ok    [A] pki.seed        repair signing key parses
ok    [A] pki.perms       no key material is group- or world-readable
ok    [A] cfg.manifest    operator.yaml federation block agrees with the manifest
ok    [A] swarm.key       swarm key present and matches the manifest
ok    [B] net.iface       coordinator sees overlay interface nebula1
ok    [B] net.bind        federation listener bound overlay-only on 10.42.0.1:9443
ok    [B] net.ready       coordinator reports federation ready
ok    [B] net.mtls        mTLS handshake with 10.42.0.1:9443 succeeded
```

Each failure names what is wrong and where. Fix them before step 5 — an invite
issued against a broken federation produces a bundle your donor cannot run, and
they are the one who finds out.

---

## 5. Create an invite

One command produces everything your donor needs.

```sh
docker compose -f docker/docker-compose.yml \
               -f deploy/operator/compose.federation.yaml \
               --profile federation run --rm nova-admin \
  node invite \
    --name alice-desktop \
    --nebula-ip 10.42.0.10/24 \
    --image ghcr.io/nova-archive/nova-node@sha256:<digest> \
    --out /invites/alice-desktop
```

Give each donor their own `--nebula-ip`: `.10`, `.11`, `.12`, and so on.

Get `<digest>` from the release you want them to run — see
[`docs/UPGRADING.md`](../UPGRADING.md). A version tag will be rejected: your
donor must run exactly the bytes you verified.

Expected:

```
invite for alice-desktop written to /invites/alice-desktop
  node_id:     19e8f7b9-2ddc-4d12-8260-14dea6d39edf
  fingerprint: sha256:70bcae01...
  files:       11
```

---

## 6. Send it

The bundle is at `deploy/operator/invites/alice-desktop/` on your server. Send
the whole directory — a zip over any channel you trust.

It contains your donor's own keys, so treat it as private. It contains **none**
of your CA or coordinator keys; Nova refuses to write the bundle at all if any
of them would end up inside.

Point your donor at [the donor quickstart](donor.md). They run
`docker compose up -d` in the folder, unchanged.

---

## 7. Confirm they arrived

```sh
docker compose exec coordinator novactl node list
```

Expected:

```
NODE_ID                               DISPLAY          STATUS       TRUST          LAST_SEEN
19e8f7b9-2ddc-4d12-8260-14dea6d39edf  alice-desktop    active       probationary   2026-08-09 12:04:11
```

`probationary` is normal for a new donor. Trust rises automatically as they
prove they are holding what they claim.

*If nothing appears:* the donor's node has not registered. Ask them for
`docker compose logs nova-node`.

---

## 8. Done

Nova starts placing data with them on its own. Nothing further is required.

---

## Day-to-day

**Add another donor:** repeat steps 5 and 6 with a new `--name` and
`--nebula-ip`.

**Someone wants to stop:** drain first, so their data is copied elsewhere
before they go.

```sh
docker compose exec coordinator novactl node drain --id <node-id>
```

Watch until their outstanding work reaches zero, then:

```sh
docker compose exec coordinator novactl node revoke --id <node-id>
```

Changed your mind: `novactl node undrain --id <node-id>`.

**A donor's machine is compromised:** revoke immediately, without draining.
Their certificate stops working at their next request.

**Rotate a donor's certificate:**

```sh
docker compose exec coordinator novactl node rotate-cert --id <node-id> \
  --dir /var/lib/nova/fedpki/active
```

The old certificate stops working straight away, so send the replacement before
you run this.

The full decision guide is in
[`docs/runbooks/donor-lifecycle.md`](../runbooks/donor-lifecycle.md).

---

## Back up your CA

If you lose the federation CA, every donor has to re-enroll from scratch.

```sh
docker run --rm -v nova_nova-fedpki:/pki:ro -v "$PWD":/out debian:bookworm-slim \
  tar czf /out/nova-fedpki-backup.tar.gz -C /pki .
```

Store it somewhere encrypted and off this machine. Everything else Nova can
rebuild; this it cannot.

---

## Adopting an existing setup

If you already built a federation CA by hand, `federation init` **adopts** it
rather than replacing it. Your donors keep working.

Put your existing files in `deploy/operator/import/`:

| File | What it is |
|---|---|
| `federation-ca.crt` / `federation-ca.key` | your federation CA |
| `coordinator-federation.crt` / `.key` | the coordinator's identity |
| `nebula-ca.crt` / `nebula-ca.key` | your Nebula CA |
| `swarm.key` | your Kubo swarm key |

Then add `--adopt-from /import` to the step 1 command.

Nova copies them in, fills in only what is missing, and never writes to
`/import`. Anything that disagrees with what you asked for stops the command
with an explanation rather than overwriting it.

Your existing `swarm.key` is never regenerated — a new one would silently cut
every current donor off from your storage network while leaving them looking
healthy.

---

## Two kinds of identity

Nova uses two separate certificate authorities, and they are not
interchangeable:

- **Nebula** decides who may join your private network.
- **Nova federation** decides who may talk to Nova's API.

A donor gets one of each. Neither CA key ever leaves your server — not in an
invite, not in a backup you send anyone, not in a support bundle.

---

## What next

- [Every operator setting](../reference/operator-configuration.md)
- [Donor lifecycle: drain, revoke, suspend](../runbooks/donor-lifecycle.md)
- [Upgrading](../UPGRADING.md)
- [Failure drills](../runbooks/failure-drills.md)
