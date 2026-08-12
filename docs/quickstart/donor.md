# Running a Nova donor node

You are lending some disk and some bandwidth to someone's Nova archive. Your
machine stores encrypted pieces of their data and hands them back when asked.

**You cannot read what you store.** It arrives encrypted and stays that way.
Nova is built so that donating storage does not mean being trusted with
content — see [what you are agreeing to](#what-you-are-agreeing-to).

Your operator sends you a folder. You check it, decide how much to lend, and
start it. About ten minutes.

## Before you start

- Linux with Docker and the `docker compose` plugin.
- Disk you can spare. 100 GB is a useful contribution; more is welcome.
- A machine that stays on. A laptop that sleeps is not a good donor —
  see [choosing a host](../VOLUNTEER_DEPLOYMENT_GUIDANCE.md).
- On Windows: [read the WSL2 guide first](../platforms/wsl2-donor.md).

---

## 1. Check what you were sent

Unpack the folder. You should see:

```
compose.yaml           node.yaml          nebula-config.yml
kubo-init.sh           README.md          invite-manifest.json
federation/            nebula/            secrets/
```

`secrets/` holds your private keys. Keep the folder to yourself.

---

## 2. Verify the image before running it

Your operator pinned an exact image. Confirm it is genuinely from the Nova
project and not something substituted along the way.

Get the digest from your bundle:

```sh
grep image: compose.yaml
```

Then, with `DIGEST` set to the `nova-node` digest you just saw:

```sh
DIGEST=sha256:<digest-from-compose.yaml>

cosign verify \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity https://github.com/nova-archive/nova/.github/workflows/release.yml@refs/heads/main \
  ghcr.io/nova-archive/nova-node@$DIGEST

cosign verify-attestation --type spdxjson \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity https://github.com/nova-archive/nova/.github/workflows/release.yml@refs/heads/main \
  ghcr.io/nova-archive/nova-node@$DIGEST

gh attestation verify oci://ghcr.io/nova-archive/nova-node@$DIGEST \
  --repo nova-archive/nova
```

All three must pass. **If any fails, stop and tell your operator.** Do not run
the image.

The identity above is an EXACT string, not a pattern. An identity regexp is one
typo away from admitting a workflow nobody meant to trust, and the point of
supplying the identity yourself is that cosign will not guess it for you. The
same string appears in `releases/verification-policy.txt` and in
`scripts/nova-release`, and a test fails if the three disagree: the signing
identity moves in ONE commit across every documented copy, because a volunteer
following a stale policy verifies nothing useful.

The identity above is an EXACT string, not a pattern. An identity regexp is one
typo away from admitting a workflow nobody meant to trust, and the whole point
of supplying the identity yourself is that cosign will not guess it for you. It
is the same string in `releases/verification-policy.txt` and in
`scripts/nova-release`, and a test fails if the three ever disagree — the
signing identity moves in ONE commit across every documented copy, because a
volunteer following a stale policy verifies nothing useful.

Images are published by digest and signed with cosign keyless via GitHub OIDC.
That is the only trust path; Nova publishes no local-key signing path.

---

## 3. Decide what you are lending

Open `node.yaml`. The top three settings are yours:

```yaml
# How much disk this node may fill. 0 = no limit.
storage_max_bytes: 536870912000          # 500 GiB

# How much traffic per day. Must be above zero.
bandwidth_budget_bytes_per_day: 53687091200   # 50 GiB/day

# Where the node keeps its own state.
storage_dir: /var/lib/nova-node/data
```

Change them to whatever you are comfortable with. Nova will not exceed them —
if a transfer would go over, it refuses rather than borrowing your capacity.

Some useful numbers:

| | bytes |
|---|---|
| 100 GiB | `107374182400` |
| 250 GiB | `268435456000` |
| 500 GiB | `536870912000` |
| 1 TiB | `1099511627776` |

Nothing else in `node.yaml` needs editing. Every other value points at a file
in the folder you were sent.

---

## 4. Start

```sh
docker compose up -d
```

---

## 5. Confirm it is working

```sh
docker compose logs -f nova-node
```

You want two lines, usually within a minute:

```
nova-node registered      node_id=19e8f7b9-...
node.source.started       listen=0.0.0.0:9555
```

`registered` means your operator can see you. `node.source.started` means you
are serving. **You should not need to restart anything** — if you were told
otherwise by an older guide, that was a bug and it is fixed.

Press Ctrl-C to stop watching; the node keeps running.

*If you see neither line:* check `docker compose logs nebula`. The overlay
network has to come up before anything else can.

*If you see `registered` but not `node.source.started`:* send your operator
the log output.

---

## That's it

Your operator will see you as `probationary` at first. That is normal — trust
rises on its own as you demonstrate you are holding what you said you would.

---

## Later

**Change how much you are lending:** edit `node.yaml`, then
`docker compose up -d`. Lowering a limit below what you already store is fine;
Nova moves the excess elsewhere.

**Stop for a while:** `docker compose stop`. Brief outages are expected and
handled. If you will be down for days, tell your operator so they can move your
data first.

**Stop for good:** tell your operator **before** you delete anything. They
drain you first — copying your pieces elsewhere — and confirm when it is safe.
Deleting the folder without draining loses redundancy.

**Update:** your operator tells you the digest. See
[`docs/UPGRADING.md`](../UPGRADING.md). Never `docker pull` a moving tag; the
whole point of the digest is that you and your operator run the same bytes.

---

## What you are agreeing to

- **You store encrypted data you cannot read.** No key that decrypts content
  ever reaches your machine.
- **You are not a public server.** Nothing is published to the open internet.
  Your node only talks to your operator's private network.
- **Your limits are yours.** Nova will not exceed the disk and bandwidth you
  set.
- **You can leave.** Tell your operator, let them drain you, delete the folder.

What you get in return is that someone's archive survives losing any single
machine, including theirs.

---

## More

- [Every `node.yaml` setting](../reference/donor-configuration.md)
- [Choosing a host, and why not your home connection](../VOLUNTEER_DEPLOYMENT_GUIDANCE.md)
- [Windows / WSL2](../platforms/wsl2-donor.md)
