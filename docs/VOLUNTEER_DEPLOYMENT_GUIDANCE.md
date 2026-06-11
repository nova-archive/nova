# Volunteer Deployment Guidance

A short, practical guide for community members who want to run a
donor pinning node for a Nova federation. Aimed at volunteers, not
operators; this is the document a federation's invite email links to.

The architecture protects donors mathematically: the bytes you
store are encrypted ciphertext, and you cannot decrypt them. This
document is about the **second** line of defense — the network
posture your node exposes to your ISP, your hosting provider, and
the world.

## Two setup pathways

Nova has two distinct deployment roles; do not confuse them:

- **Operator** — runs the single coordinator (the authoritative node with the
  master key, moderation, and the public read gateway). Operators follow
  [`quickstart.md`](quickstart.md), not this guide.
- **Donor** — runs a `nova-node` that pins **opaque ciphertext** over the
  operator's mesh. **That is you.** This guide is the donor pathway. The exact,
  signed, tested install steps are formalized in `quickstart/donor.md` as part of
  the Phase 2 donor release (P2-M7); the walkthrough below is the current
  reference.

A donor never holds keys, never sees plaintext, and exposes no public ports.

## Why this matters

Running a Nova node is, technically, a small commercial-style
network workload: a long-running daemon that receives and serves
data over a mesh VPN. Even though Nova's bandwidth budget caps keep
volume modest, residential ISPs and some VPS providers apply
heuristics to flag long-running upload-heavy workloads. A flag
doesn't mean you've done anything wrong; it can still mean a
throttled connection, an annoying email from your ISP, or in
extreme cases account termination from a strict provider.

The architecture cannot hide your node's IP from your ISP or your
provider. What it can do is give you knobs to keep the workload
small and unremarkable. The rest of this document is how to use
those knobs and where to deploy your node so the workload is in an
environment that expects this kind of traffic.

## TL;DR

- **Run your node on a privacy-respecting VPS, not your home
  connection.** A small-tier VPS at a provider with reasonable
  logging policies and a generous Terms of Service is the right
  default.
- **Set your bandwidth budget to fit your provider's plan.** Use
  the table below as a starting point.
- **Don't all use the same provider.** Diversification across
  providers is the federation's resilience story.
- **Read your provider's Acceptable Use Policy** before signing up.
  If they prohibit "running servers" or "P2P traffic," they're not
  the right home for your node.

## Choosing a host

Run your node on a host that:

- Has a clear Acceptable Use Policy that **does not** prohibit
  long-running daemons or peer-to-peer traffic.
- Provides root or sudo access (Nova's container needs to run as a
  non-root user, but you'll need root to install Docker or your
  preferred container runtime).
- Has bandwidth allowances appropriate to your chosen budget.
  Track 1 TB/month or more is a reasonable floor.
- Has a privacy policy describing log retention. Shorter is better;
  30–90 days is common, < 7 days is excellent.
- Is in a jurisdiction whose data-protection regime you trust.

Avoid hosts that:

- Are known to terminate accounts for opaque "ToS violations" with
  no appeal.
- Run aggressive automated abuse heuristics (some major commodity
  cloud providers do; their detection systems are tuned for
  hyperscale workloads, not federated storage).
- Require KYC ("know-your-customer") that exceeds your comfort
  level, unless you have a specific reason.

## Why not run on your home connection?

You can — Nova will let you. The question is whether you want to.

Things to consider:

- Your home IP is exposed to anyone who can observe Nebula traffic
  destination addresses. The mesh encrypts payload, not transport
  metadata.
- Residential ISPs almost universally prohibit "running servers"
  in the fine print. Most ignore it; some don't.
- Sustained upload at residential rates is a heuristic flag.
- Your ISP's logs are the first thing subpoenaed in any
  investigation, however unrelated to you.
- The architecture's mathematical defenses (encryption envelope,
  signed-URL audience binding, etc.) work the same regardless of
  where you host. This is purely about reducing the size of your
  exposure surface.

If you choose to run at home, lean conservative on the bandwidth
budget — start at 20–50 GB/day, not 500 GB/day.

## Bandwidth budget configuration

The bandwidth budget caps how much data your node will upload per
day in service of pin assignments. The coordinator schedules within
it — and, as the authoritative backstop, **your node enforces the cap
locally**: it runs a token-bucket that refuses any transfer which would
exceed your configured daily budget, regardless of what the coordinator
asks. The budget is yours, enforced on your machine. Set it in
`node.yaml`:

```yaml
node:
  bandwidth_budget_bytes_per_day: 53687091200   # 50 GB/day, residential default
  upload_link_mbps: 50
  throttle_window:
    start: "09:00"
    end:   "23:00"
    multiplier: 0.30                             # cap at 30% during business hours
```

Recommended starting points:

| Deployment type | `bandwidth_budget_bytes_per_day` | `upload_link_mbps` | Notes |
|---|---|---|---|
| Residential, conservative | 21474836480  (20 GB/day)  | 50  | Stay well under most ISP heuristics. |
| Residential, generous     | 53687091200  (50 GB/day)  | 50  | Default. Most home plans tolerate this. |
| Mid-tier VPS              | 536870912000 (500 GB/day) | 100 | Common for $5–$15/mo VPS plans. |
| High-bandwidth VPS        | 2199023255552 (2 TB/day)  | 1000| For donors who can afford a 1 Gbps tier. |

The orchestrator does not penalize donors with smaller budgets; it
simply distributes work proportionally. Your contribution scales
with your budget.

You can change these values later. Restart your node container after
editing `node.yaml`.

## Running multiple nodes

Some donors run more than one node — for example, one residential
and one VPS — to contribute redundantly. Nova supports this; each
node has its own Nebula cert and `node_id`. The orchestrator treats
them as independent donors.

If you run multiple nodes for the same federation, you want the
orchestrator to avoid placing the same CID on two of *your* nodes —
otherwise a single host or account failure takes out both copies and
defeats the redundancy. Nova does this through **failure-domain
anti-affinity**, but it is **not magic and not purely automatic**: the
orchestrator can only avoid co-placing copies it knows share a failure
domain. **Tell your operator** which nodes you run (and on which host /
provider / account) so they can record a shared *donor principal* /
*failure domain* for them. Until the operator records that linkage, the
orchestrator treats your nodes as independent and may place the same CID
on two of them. (Self-declared geography alone is *not* used for this —
it is too easy to spoof — so the operator-verified linkage is what makes
the anti-affinity real.)

## Provider diversification

The simulation in `simulations/orchestrator_resilience.py` shows
that the worst-case mass-casualty event is a single provider
abruptly terminating every donor's account on it. Recovery from
that event takes 5–10× longer than from an equivalent uniform
random failure.

The architectural mitigation is **donor diversification across
providers**. As a donor, you can help by:

- Choosing a provider that is *not* the dominant one in your
  federation. The federation's supporters page (if the operator
  runs one) usually shows the distribution.
- Running multiple nodes on different providers, if you have
  capacity.
- Recommending less-represented providers when new donors join
  the community.

This is collective rather than enforced. The federation's
operator monitors the diversification ratio and announces in the
community if any provider exceeds 40 % of capacity.

## Setup walkthrough

1. **Provision a host.** Use the criteria above. Have an SSH
   keypair ready.

2. **Install a container runtime.** Docker Engine on Linux is the
   recommended path:
   ```sh
   curl -fsSL https://get.docker.com | sh
   ```
   Avoid Docker Desktop on Mac/Windows (it sends usage telemetry).

3. **Receive your federation invite.** Nova authenticates donors at **two
   layers**, so the bundle carries **two certificate + key pairs** — do not
   conflate them:
   - **Nebula** cert + key (`nebula.crt`, `nebula.key`) — authorizes mesh
     (overlay) membership.
   - **Federation client** cert + key (`federation.crt`, `federation.key`) —
     authorizes your node's HTTP calls to `/fed/v1` (mTLS, distinct from the
     Nebula identity).
   - The federation's CA cert (`ca.crt`).
   - The IPFS swarm key (`swarm.key`).
   - The Nebula lighthouse address and the coordinator's federation URL.

   **Private-key handling.** The two `.key` files are secrets. Receive them over
   a private channel, store them `chmod 600`, mount them read-only, and never
   share or commit them. If a key is ever exposed, ask the operator to **revoke**
   it immediately (a `novactl node revoke` on their side fails your next mTLS
   handshake) and issue a fresh bundle.

4. **Create your node directory:**
   ```sh
   mkdir -p ~/nova-node
   cd ~/nova-node
   # place donor.crt, ca.crt, swarm.key here
   ```

5. **Write your `node.yaml`** with bandwidth limits, throttle
   window, and storage path.

6. **Bring up Nebula.** Nova runs Nebula as a **separate host/sidecar process**,
   not inside `nova-node` — so the node container needs no `NET_ADMIN` capability.
   Start Nebula with your `nebula.crt`/`nebula.key`/`ca.crt` and lighthouse first;
   note the overlay interface address it creates (e.g. `10.42.0.x`). `nova-node`
   binds its inbound HTTPS server to **that Nebula address only** — never
   `0.0.0.0` — which is why the run command below publishes **no ports**.

7. **Verify the image signature and pin a digest.** The donor image is
   cosign-signed with SBOM + provenance. Do **not** run `:latest` in production:
   ```sh
   cosign verify ghcr.io/nova-archive/nova-node:v0.2.0   # confirm signature + identity
   docker pull   ghcr.io/nova-archive/nova-node:v0.2.0
   docker inspect --format '{{index .RepoDigests 0}}' ghcr.io/nova-archive/nova-node:v0.2.0
   # record the @sha256:… digest and run that, so the bytes can never change under you
   ```

8. **Run the container** (digest-pinned, no published ports, hardened):
   ```sh
   docker run -d \
       --name nova-node \
       --restart unless-stopped \
       -v $(pwd):/etc/nova-node:ro \
       -v $(pwd)/data:/var/lib/nova-node \
       --read-only \
       --cap-drop ALL \
       --security-opt no-new-privileges \
       ghcr.io/nova-archive/nova-node@sha256:<digest-from-step-7>
   ```
   The config volume (certs, keys, `node.yaml`, `swarm.key`) is mounted
   read-only; the node only writes to the data volume.

9. **Verify in your operator's admin dashboard** that your node appears and
   heartbeats. New nodes start in a **probationary** trust state and receive a
   capped amount of data — and are never made the sole copy of critical content —
   until they graduate on age plus successfully-passed possession audits. This is
   expected; your contribution ramps up as your node proves itself.

## What to back up (and what not to)

Your node holds two very different kinds of data:

- **Your node identity and certs are precious — back them up.** The Nebula and
  federation certs + keys, the `swarm.key`, your `node.yaml`, and the node's
  small local state (its `node_id`, sync cursor, and replay cache). Losing these
  means re-registering from scratch and re-fetching everything you held. Keep an
  off-box copy of the cert/key bundle and `node.yaml`; the local state directory
  is recreated on re-register if lost.
- **The ciphertext is replaceable — don't bother backing it up.** The blocks in
  your data volume are opaque, re-fetchable from the federation, and meaningless
  without keys you do not have. If your disk dies, the orchestrator re-replicates
  what you held to other donors; you just re-join and re-fill.

## Decommissioning gracefully

When you want to stop donating — or move to a new host — don't just `docker rm`
the node and disappear. A clean exit lets the federation re-replicate your CIDs
before they become under-replicated:

1. **Tell the operator** you intend to leave, and when.
2. Let the operator **drain** your node (stop assigning new pins and re-replicate
   your held CIDs elsewhere). On the operator side this is part of
   `novactl node` lifecycle management; on yours it is just keeping the node
   running and heartbeating until they confirm the drain is complete.
3. Once drained, stop and remove the container, and **delete the cert/key
   material** so a lost disk cannot leak it. Ask the operator to **revoke** the
   certs as a belt-and-suspenders step.

If you instead vanish without draining, you are treated as an outage: your node
goes `unreachable` (~1 h) and the orchestrator heals around you, then `evicted`
(~30 d) at which point your assignments are dropped. Graceful is kinder to the
federation's bandwidth budget.

## Troubleshooting

**The node won't start.** Check container logs:
```sh
docker logs nova-node
```
Common issues:
- `IPFS_SWARM_KEY missing`: place `swarm.key` in the config volume
  with the right path.
- `Kubo hardening validator failed`: an operator-supplied
  bootstrap entry resolves to a public IP. Confirm the bootstrap
  list points only at the federation's Nebula overlay.
- `Nebula handshake refused`: your cert may be expired or revoked;
  contact the operator.

**My ISP sent a notice.** Open a ticket with the operator describing
what was sent. The operator may provide a redacted copy of the
notice for the federation's "providers in good standing" list.
Reduce your bandwidth budget if you want to continue donating from
the same connection.

**I'm hitting my bandwidth budget every day.** Either your budget
is too small or the federation is concentrated on you (you may be
one of the few donors with high-capacity link). Talk to the
operator; they may want to recruit additional donors to spread load.

## What Nova cannot protect you from

- **Your hosting provider's policies.** If your provider terminates
  you for ToS violations, no architectural feature can prevent it.
- **Your ISP's heuristics for residential connections.** Bandwidth
  caps mitigate but cannot eliminate.
- **Subpoenas served on you directly.** Your provider has your
  payment information; that is independent of Nova. The bytes you
  hold remain mathematically opaque, but your hosting relationship
  is between you and your provider.
- **Your own operational mistakes.** Misconfigured firewalls,
  exposed Kubo APIs, leaked Nebula certs — Nova's hardening
  validators catch many of these but cannot catch all of them.

The math protects the bytes. The deployment guidance in this
document protects the donor's network posture. Together they
amount to a reasonable defense for ordinary community-scale
operation. For threats beyond that, this is not the right
infrastructure.
