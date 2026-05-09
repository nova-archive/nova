# Volunteer Deployment Guidance

A short, practical guide for community members who want to run a
donor pinning node for a Nova federation. Aimed at volunteers, not
operators; this is the document a federation's invite email links to.

The architecture protects donors mathematically: the bytes you
store are encrypted ciphertext, and you cannot decrypt them. This
document is about the **second** line of defense — the network
posture your node exposes to your ISP, your hosting provider, and
the world.

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
day in service of pin assignments. The orchestrator never exceeds
it. Set it in `node.yaml`:

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

If you run multiple nodes for the same federation, the orchestrator
will avoid placing the same CID on two of your nodes (since that
defeats the redundancy goal). This is automatic; no extra config
needed.

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

3. **Receive your federation invite.** The operator emails or DMs
   you a small bundle:
   - A Nebula certificate (`donor.crt`)
   - The federation's CA cert (`ca.crt`)
   - The Nebula lighthouse address
   - The IPFS swarm key (`swarm.key`)
   - The coordinator's federation URL

4. **Create your node directory:**
   ```sh
   mkdir -p ~/nova-node
   cd ~/nova-node
   # place donor.crt, ca.crt, swarm.key here
   ```

5. **Write your `node.yaml`** with bandwidth limits, throttle
   window, and storage path.

6. **Run the container:**
   ```sh
   docker run -d \
       --name nova-node \
       --restart unless-stopped \
       -v $(pwd):/etc/nova-node:ro \
       -v $(pwd)/data:/var/lib/nova-node \
       --read-only \
       ghcr.io/nova-archive/nova-node:latest
   ```
   The `--read-only` flag is intentional; the node only writes to
   the data volume.

7. **Verify in your operator's admin dashboard** that your node
   appears with `status = 'active'` after the first heartbeat.

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
