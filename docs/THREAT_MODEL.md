# Threat Model

Status: **Phase 0 — normative for what is in scope and out of scope.**
This document is engineering reasoning, not legal advice. Operators
should consult counsel in their jurisdiction before relying on any
of the legal mitigations described here.

## Purpose

Nova's security posture rests on a small number of load-bearing
architectural choices. This document enumerates which adversaries
those choices defend against, where the boundaries are, and which
classes of attack the project explicitly does not attempt to mitigate.
A reader who finishes this document should be able to predict the
project's response to any plausible attack scenario without needing
to read implementation code.

## Assets

The assets Nova protects, ranked by harm if exposed:

| Asset | Why it matters |
|---|---|
| **Operator master key** | Loss = permanent loss of every blob; compromise = total federation compromise |
| **User-uploaded plaintext** | Sensitive personal content; the system's primary protected asset |
| **Per-blob keys** (in `keys.wrapped_key`, when active) | Compromise lets an attacker decrypt corresponding blobs |
| **User identity** (email, source IP, upload history) | PII; minimization required by GDPR |
| **Federation membership** (the set of donor nodes and their IPs) | Knowing who hosts what enables targeted pressure |
| **Audit log** | Tampering hides administrator misconduct |
| **Donor disk contents** (ciphertext) | Volume / CID enumeration could leak federation activity |

## Trust boundaries

```
  PUBLIC INTERNET
        │
   ─────┴────  ① TLS (operator-controlled)
        │
       nginx
        │
   ─────┴────  ② loopback / unix socket
        │
   coordinator ─────────────────────────  ③ env-loaded master key
        │  │  │
        │  │  └────────── Postgres (local, trusted)
        │  └─────────── Kubo (local IPC, hardened)
        │
   ─────┴────  ④ Nebula mTLS + private swarm key
        │
   donor pinning nodes
        │
   ─────┴────  ⑤ libp2p inside private swarm
        │
   donor ↔ donor traffic
```

| # | Boundary | Trust direction |
|---|---|---|
| ① | Internet ↔ nginx | nginx terminates TLS; everything inside the box is trusted by the operator |
| ② | nginx ↔ coordinator | loopback / unix socket; trusted by colocation |
| ③ | coordinator ↔ master key | environment variable; never written to disk by Nova |
| ④ | coordinator ↔ donor | Nebula mTLS; cert fingerprint is durable identity |
| ⑤ | donor ↔ donor | private libp2p swarm; private swarm key gates participation |

The trust boundary the project most cares about is **④**. The
volunteer-blind storage argument lives there: across that boundary,
only ciphertext crosses, and a donor cannot decrypt the bytes it
holds.

## Threat actors and scenarios

Each scenario lists the attacker's modeled capabilities, what they
gain, and what stops them.

### A. Malicious donor node

**Capabilities.** Has a valid Nebula cert (registered legitimately or
via stolen credentials). Can poll `/fed/v1/pins/assigned`, ack pins,
fail pins, and receive ciphertext blobs through the libp2p swarm.

**Goal.** Read user content; identify other donors; serve modified
content; enumerate the federation's CIDs.

**Mitigations.**

- **Encryption envelope.** The donor holds only opaque ciphertext
  (see `docs/specs/ENCRYPTION_ENVELOPE.md`). They cannot decrypt;
  the per-blob keys exist only inside the coordinator's process.
- **CID self-validation.** Modifying a byte changes the CID. A
  donor that serves tampered bytes is detected immediately by the
  IPFS resolver, which checks the hash. The donor has no way to
  serve garbage and have it accepted.
- **Bounded enumeration.** A donor learns only the CIDs assigned to
  it. They cannot enumerate the full federation set without
  compromising the coordinator.
- **No upload privilege.** A donor's Nebula cert grants federation
  participation, not blob upload. Uploads require a bearer token
  from Authelia; donors do not have one.
- **Poison-pill revocation.** When a donor is identified as
  compromised, the operator's `novactl node revoke` revokes their
  Nebula cert immediately. Subsequent handshakes fail; the donor
  cannot rejoin. Held CIDs are re-replicated to other donors.

**Residual risk.** A donor can correlate the volume and timing of
pin requests against publicly-visible content (e.g., observing that
the federation pinned a particular CID shortly after a known
upload). For low-volume operators, this is a real but unmitigated
risk; for high-volume operators it is statistical noise.

### B. Compromised coordinator

**Capabilities.** Total: read/write database, hold master key in
process memory, sign URLs, mint Nebula certs.

**Goal.** Decrypt all blobs; identify all users; pivot to other
operators' federations.

**Mitigations.**

- **Federation isolation.** Each operator has their own master key,
  Nebula CA, and IPFS swarm key. A coordinator compromise is
  catastrophic for one operator but does not cascade.
- **Audit log.** Every privileged action is logged to the
  `audit_log` table. The compromise is forensically reconstructable
  if the audit log itself is intact and shipped off-box.
- **Master-key rotation.** The operator can rotate
  `NOVA_MASTER_KEY` on suspected compromise; `novactl keys
  rotate-master` re-wraps every active per-blob key with the new
  master key, invalidating the old one.

**Residual risk.** A coordinator compromise that is **silent** —
the attacker does not modify the audit log, but exfiltrates data
quietly — is undetectable by Nova alone. The operator must run
their own host-based intrusion detection. This is acknowledged as
out of scope for the project: Nova cannot defend the host that
runs Nova.

### C. Hostile crawler

**Capabilities.** HTTP client with arbitrary scripting; may rotate
IPs; may attempt to scrape every URL pattern.

**Goal.** Enumerate the public catalog; harvest signed URLs; cause
traffic and cost.

**Mitigations.**

- **Signed URLs for non-public content.** Without a valid signature,
  reads return `401`. The signature is bound to an audience origin
  and an expiry; a leaked URL is useless after expiry.
- **Audience check.** The signed URL's `aud` parameter must match
  the request's `Origin`/`Referer`. A crawler scraping signed URLs
  out of a leaked HTML page cannot dereference them from a
  different origin.
- **Rate limiting.** nginx applies per-IP, per-user, and per-CID
  token buckets at the edge; the coordinator runs a defense-in-
  depth check.
- **CDN absorption.** Public content is `Cache-Control: public,
  max-age=31536000, immutable`; a CDN absorbs ~95% of read traffic
  and the cost of crawl pressure.

**Residual risk.** A crawler with the embedding origin cookie or
referer can dereference signed URLs. Operators who need stronger
binding should set short TTLs (minutes) and rely on revocation.

### D. Network observer (public internet)

**Capabilities.** Passive eavesdropping between the user and `nginx`;
the user's ISP, a coffee-shop Wi-Fi sniffer, a transit AS.

**Goal.** Identify which content the user is uploading or viewing;
establish that the user uses Nova at all.

**Mitigations.**

- **HTTPS.** TLS 1.3 minimum; certificate pinning at the operator's
  discretion.
- **No third-party assets.** The widget and admin SPA are hermetic;
  there are no requests to `fonts.googleapis.com`, CDN libraries,
  or analytics endpoints. CI lint enforces this.
- **TLS mode selection.** ACME HTTP-01 leaks the hostname to CT
  logs **forever**; operators in privacy-sensitive deployments
  should choose DNS-01 wildcard, static certs, or a `.onion`
  hidden service. The first-run wizard surfaces the trade-off
  explicitly. See `docs/PRIVACY_AUDIT.md` § "TLS mode".

**Residual risk.** Traffic-volume analysis can fingerprint popular
content over time. Operators who care about this should serve all
content at uniform rate, which Nova does not implement; padding is
a Phase 6+ research direction.

### E. Network observer at the donor's ISP

**Capabilities.** Sees encrypted UDP traffic from the donor's host
to the coordinator's Nebula address.

**Goal.** Identify the donor as running peer-to-peer / commercial
infrastructure; cause the ISP to throttle or terminate the
connection.

**Mitigations.**

- **Nebula encryption.** Traffic is opaque to the ISP; only volume
  and destination IP are visible.
- **Bandwidth budget enforcement.** The donor's
  `bandwidth_budget_bytes_per_day` caps daily egress; the
  orchestrator never exceeds it (see `docs/specs/HEALING_PROTOCOL.md`).
  This is the architecture's primary defense against ISP volume
  heuristics.
- **VPS deployment guidance.** The operator's `VOLUNTEER_DEPLOYMENT_GUIDANCE.md`
  recommends running pinning nodes on a privacy-respecting VPS
  rather than residential connections, so the ISP's heuristics
  apply to a datacenter contract instead of the donor's home.

**Residual risk.** A donor running on a residential connection with
aggressive bandwidth use will be flagged. Nova mitigates by capping;
beyond the cap, the operational guidance is "use a VPS."

### F. Inducement / liability content uploader

**Capabilities.** A registered user with upload privileges submits
content the operator is legally liable for hosting.

**Goal.** Cause the operator to host material that triggers DMCA
notices, criminal exposure, or regulatory action.

**Mitigations.**

- **PDQ scan at upload.** Image uploads are scanned synchronously
  against the operator-configured blocklist (StopNCII et al.);
  matches reject the upload before any bytes are persisted.
- **DMCA workflow.** Public `/legal/dmca` endpoint receives notices
  into `dmca_cases`; operator reviews via admin UI; takedown
  executes crypto-shred + cluster-wide unpin broadcast.
- **Repeat-infringer tracking.** `takedown_repeat_infringers` rows
  accumulate; configurable strikes terminate accounts.
- **Anonymous-upload + moderation-off refusal.** The coordinator
  refuses to start with `auth: anonymous` and `moderation: off`
  simultaneously; this prevents the worst-case configuration that
  invites red-flag-knowledge problems.

**Residual risk.** PDQ has finite recall; novel content slips
through. The operator's responsiveness to DMCA notices is the
human-layer defense.

### G. Supply-chain attacker

**Capabilities.** Compromises a dependency, an adapter package, or
a build tool.

**Goal.** Inject backdoor; exfiltrate keys; tamper with the
encryption envelope.

**Mitigations.**

- **Adapters live in separate repositories.** The coordinator does
  not import their code; an adapter compromise affects only that
  adapter's host environment.
- **Pinned dependencies.** `go.sum` and `package-lock.json` lock
  every transitive version. CI rejects PRs that touch lock files
  without an accompanying explanation.
- **Hermetic admin SPA build.** No third-party CDN assets at
  runtime; CI lint blocks references to external origins in the
  built bundle.
- **No telemetry, no phone-home, no auto-update** in the
  coordinator or donor binaries. There is no mechanism for an
  attacker to push a malicious update through Nova's own update
  channel because there is no update channel.

**Residual risk.** A compromised pinned dependency that is updated
in a future release would still ship malicious code. Phase 5 ships
release signing (sigstore / cosign); until then, operators verify
release artifacts against the project's published checksums.

## Out of scope

These are not architectural oversights; the project explicitly
declines to defend against them.

| Threat | Why out of scope |
|---|---|
| State-level adversary | Outside the project's threat budget; defending against TAO requires architectures (compartmented hardware, key escrow, etc.) inconsistent with self-hostable FOSS. |
| Compromised root CA | If the public TLS chain is forged, every TLS-protected service is compromised; not specific to Nova. |
| Compromised host kernel | Nova runs in userspace; a malicious kernel sees plaintext as the coordinator decrypts it. Operators harden their host independently. |
| Hardware side channels (Spectre, etc.) | Mitigation belongs at the OS / microcode layer. |
| Operator who deliberately misconfigures security floors | The first-run wizard surfaces unsafe configurations and the coordinator refuses some combinations; bad-faith operators can still do harm. |
| Donor copying ciphertext to a hostile party | It remains ciphertext. The hostile party gains nothing the donor did not already have, and the donor cannot decrypt. |
| Multi-coordinator high availability | Single-coordinator deployments are the explicit target; HA is not a Phase 1–5 goal. Operators tolerate coordinator downtime by recovering from backups. |
| Cross-jurisdiction legal compulsion | Engineering cannot answer "what if a US court compels disclosure of a German-hosted operator's data?" Operators consult counsel. |

## Acknowledged residual risks

These are real risks the project does not fully eliminate, listed
prominently so operators can plan for them.

1. **A silently-compromised coordinator** can decrypt every blob
   without leaving a forensic trace. Host-level security is the
   operator's problem.
2. **Master-key loss** is permanent data loss for every blob.
   Backups must be off-box, off-cloud, and rehearsed.
3. **The coordinator is a single point of operational failure.**
   Nova does not run multi-master. Restoration from backup is the
   recovery procedure.
4. **Traffic-volume analysis** can fingerprint popular content
   over time. Operators worried about this should run a high-cache-
   hit-rate CDN in front and accept the residual risk.
5. **Donor IP exposure** if running on a residential connection.
   The architecture cannot hide the donor's IP from the
   coordinator or from a passive observer at their ISP. Operational
   guidance documents the VPS recommendation.
6. **PDQ false negatives.** Novel infringing content evades the
   blocklist until human review. The DMCA workflow is the backstop.

## Disclaimer

This document is a description of the project's engineering choices
and the reasoning behind them. It is not legal advice. Statements
about DMCA Section 512 safe harbor, GDPR Article 17 erasure, and
similar legal frameworks describe the engineering features that
support compliance; whether any given operator achieves compliance
depends on their jurisdiction, their conduct, and the advice of
their counsel. The contributors of Nova make no warranty that
deployment of this software will produce any particular legal
outcome.
