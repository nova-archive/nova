# Upgrading Nova

How to move an existing deployment to a new release without breaking it.

> **Scope note.** This document becomes normative and machine-generated in
> **P2-M7.3**, which adds versioned signed releases, a per-release
> compatibility matrix tested by CI, `novactl upgrade check|verify`, and an
> explicit migration-rollback contract. Until then this is the honest minimum:
> what changed, what you run, and how to go back. Every milestone adds its own
> entry, and the `live-upgrade-e2e` gate proves an existing federation can
> cross it using only this page.

## The rule

**Every step you need is on this page.** If an upgrade requires something not
written here, that is a bug — report it rather than improvising. Manual
out-of-band repair is not an acceptable upgrade path.

## Before any upgrade

1. **Read the entry for your target release below.** Do not skip versions
   unless an entry says you may.
2. **Record what you are running:**
   ```sh
   git -C /srv/nova describe --tags --always
   docker compose --env-file docker/.env -f docker/docker-compose.yml ps --format '{{.Service}} {{.Image}}'
   ```
3. **Back up Postgres and the `nova-secrets` volume.**

   **If federation is already initialised**, also back up the federation
   authority — the CA cannot be rebuilt:
   ```sh
   docker run --rm -v nova_nova-fedpki:/pki:ro -v "$PWD":/out debian:bookworm-slim \
     tar czf /out/nova-fedpki-backup.tar.gz -C /pki .
   ```
   `nova_nova-fedpki` exists only once `federation init` has run. A
   non-federated operator, or one adopting a pre-productization federation for
   the first time, will not have it yet — in the latter case back up wherever
   the existing authority currently lives instead.

   **A backup you have never restored does not count as a backup.**
4. **Do not upgrade donors at the same time as the coordinator.** Coordinator
   first, donors afterwards, in small batches.

---

## P2-M7.2 — Federation productization

**What changed.** Federation went from a set of manual steps to one supported
path: `federation init`, `federation doctor`, `node invite`. Existing
federations are **adopted**, not replaced.

**No database migration.** Schema is unchanged.

### If you do not use federation

Nothing to do. Pull the release and recreate as usual — the base Compose file
gained one volume declaration and no service changes.

### If you have a hand-built federation

Your CA, your donors and your swarm key are all preserved. Nothing re-enrolls.

**1. Collect your existing PKI** into `deploy/operator/import/`:

| File | What it is |
|---|---|
| `federation-ca.crt` / `federation-ca.key` | your federation CA |
| `coordinator-federation.crt` / `.key` | the coordinator's identity |
| `nebula-ca.crt` / `nebula-ca.key` | your Nebula CA |
| `swarm.key` | your Kubo swarm key |

Copy them — do not move them. Nova reads from this directory and never writes
to it; it is mounted read-only.

Your existing `swarm.key` is never regenerated. A new one would silently cut
every current donor off from your storage network while leaving them looking
perfectly healthy — registered, up, and unable to exchange anything.

If your hand-built setup genuinely never had some of these (a Nebula CA, for
instance), leave them out rather than inventing them. Adoption creates what is
missing and preserves what is not.

**2. Adopt:**

```sh
docker compose --env-file docker/.env \
               -f docker/docker-compose.yml \
               -f deploy/operator/compose.federation.yaml \
               --profile federation run --rm nova-admin \
  federation init \
    --adopt-from /import \
    --overlay-cidr 10.42.0.0/24 \
    --operator-overlay-ip 10.42.0.1 \
    --lighthouse-public 203.0.113.7:4242 \
    --hostname nova.example.org
```

Use the values your current federation already uses. If they disagree with your
existing certificates, the command **stops and shows the difference** rather
than overwriting anything.

Expected: a non-zero `adopted:` count, and `created:` covering only what your
hand-built setup lacked — typically the coordinator client identity and the
repair signing key, which earlier documentation never mentioned.

**3. Recreate with the overlay:**

```sh
docker compose --env-file docker/.env \
               -f docker/docker-compose.yml \
               -f deploy/operator/compose.federation.yaml \
               --profile prod --profile federation up -d
```

**4. Verify:**

```sh
docker compose --env-file docker/.env \
               -f docker/docker-compose.yml \
               -f deploy/operator/compose.federation.yaml \
               --profile federation run --rm nova-doctor \
  federation doctor --live
```

All checks should pass. Then confirm your donors are still there and unchanged:

```sh
docker compose exec coordinator novactl node list
```

Their `NODE_ID` values must be the same as before. If any donor has a new id,
stop — something re-enrolled rather than adopting, and that should not happen.

### Port change

The federation listener is **9443**. Some older notes said `8443`, which is the
public HTTPS port. If you set `federation.listen_addr` by hand to an `:8443`
address, `federation init` rewrites it; verify with `federation doctor`.

### Behaviour change: the coordinator waits instead of failing

Previously, a missing overlay interface stopped the coordinator from booting.
It now starts, serves normally, and waits up to `interface_wait_seconds`
(default 120) for the interface before binding the federation listener. On
timeout it stays up with federation not ready.

If you prefer the old behaviour, set `interface_wait_seconds: -1`.

You will now see this line at startup, which is normal:

```
federation.listener.waiting  iface=nebula1 listen=10.42.0.1:9443
```

### Behaviour change: donors no longer need a restart

A first-boot donor used to register but not serve until restarted. That is
fixed. Donors on the new image start serving immediately after registering.
**No action needed** — existing donors keep working on the old image and pick
this up whenever they next update.

### Rollback

Safe. No migration ran, so the previous release can be redeployed directly.
The federation material `init` created is additive: an older coordinator
ignores what it does not know about. Your CA and donor identities are untouched
either way.

---

## Upgrading donors

Donors update **after** the coordinator, in small batches, never all at once.

For each donor:

1. Confirm the federation stays above its replication floor if this donor
   disappears (`novactl node list`).
2. If the update will take more than a moment, drain first:
   ```sh
   novactl node drain --id <node-id>
   ```
3. Give the donor the exact **digest** to run. Never a moving tag.
4. The donor verifies and restarts — see
   [the donor quickstart](quickstart/donor.md).
5. Confirm they return: registration, immediate heartbeat, and
   `node.source.started`.
6. `novactl node undrain --id <node-id>`.
7. Wait for repair debt to settle before the next batch.

An update must never look like a new enrollment. The donor keeps their
federation certificate, Nebula certificate, node id, Kubo repository, swarm key
and stored data. If a donor comes back with a new `NODE_ID`, something is
wrong.

## Do not auto-update

Do not point Watchtower or similar at Nova images. A broken or compromised
release reaching every donor simultaneously defeats the diversity that makes
the federation survivable in the first place. Update a few, watch, continue.
