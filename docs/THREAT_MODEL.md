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
| **Per-blob keys** (in `data_encryption_keys.wrapped_key`, when active) | Compromise lets an attacker decrypt corresponding blobs |
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
   ─────┴────  ④ Nebula overlay + HTTPS/mTLS (federation cert)
        │
   donor pinning nodes
        │
   ─────┴────  ⑤ controlled HTTPS over Nebula + repair tokens
        │
   donor ↔ donor traffic
```

| # | Boundary | Trust direction |
|---|---|---|
| ① | Internet ↔ nginx | nginx terminates TLS; everything inside the box is trusted by the operator. The public-content origin (`/blob/*`, `/i/*`, `/legal/*`, `/health`, `/api/v1/uploads/*`) and the admin/setup origin (`/admin`, `/api/v1/admin/*`, `/api/v1/auth/*`, ephemeral `/setup`) live on **distinct virtual hosts** so that an XSS or cache-poisoning at the public origin does not become administrative trust. The admin host can be IP-restricted or mTLS-fronted independently. v3.1 amendment. |
| ① a | First-run setup wizard | Binds loopback-only inside the coordinator container; reachable from the host only when the operator publishes the setup port (`docker compose --profile setup up`). Self-disables permanently after the first successful bootstrap writes `.bootstrap-complete` to the secrets volume. v3.1 amendment. |
| ② | nginx ↔ coordinator | loopback / unix socket; trusted by colocation |
| ③ | coordinator ↔ master key | environment variable OR file-mount (e.g., Docker secret at `/run/secrets/master-key-<label>`, per the `NOVA_MASTER_KEY_<LABEL> → _FILE → /run/secrets/master-key-<label>` resolver chain); never written to disk by Nova. v3.1 amendment broadens beyond env-var-only; implemented in M6.1. |
| ④ | coordinator ↔ donor | Nebula overlay + HTTPS/mTLS; the Nebula cert authorizes mesh membership, the federation client cert authorizes HTTP API calls |
| ⑤ | donor ↔ donor | HTTPS/mTLS inside Nebula; donor-to-donor fetches require a coordinator-issued, source-and-destination-pinned HMAC repair token. The private IPFS swarm key gates Kubo's daemon-level peering as defense in depth, but no donor-to-donor data exchange occurs over Bitswap |

The trust boundary the project most cares about is **④**. The
volunteer-blind storage argument lives there: across that boundary,
only ciphertext crosses, and a donor cannot decrypt the bytes it
holds.

## Threat actors and scenarios

Each scenario lists the attacker's modeled capabilities, what they
gain, and what stops them.

### A. Malicious donor node

**Capabilities.** Has a valid Nebula cert and a valid federation
client cert (registered legitimately or via stolen credentials).
Can poll `/fed/v1/pins/changes`, ack pins, fail pins, and fetch
ciphertext blobs over the controlled HTTPS-over-Nebula repair
transport when issued a valid repair token. Cannot mint repair
tokens of its own and cannot ad-hoc fetch blocks from other donors
without a coordinator-signed grant.

**Goal.** Read user content; identify other donors; serve modified
content; enumerate the federation's CIDs.

**Mitigations.**

- **Encryption envelope.** The donor holds only opaque ciphertext
  (see `docs/specs/ENCRYPTION_ENVELOPE.md`). They cannot decrypt;
  the per-blob keys exist only inside the coordinator's process.
- **CID self-validation.** Modifying a byte changes the CID. A
  donor that serves tampered bytes is detected immediately when the
  destination donor (or the coordinator) re-hashes the envelope on
  receipt. The donor has no way to serve garbage and have it accepted.
- **Bounded enumeration.** A donor learns only the CIDs assigned to
  it. They cannot enumerate the full federation set without
  compromising the coordinator.
- **No ad-hoc peering.** Donor-to-donor fetches are gated by
  coordinator-issued repair tokens bound to a specific source, a
  specific destination, a specific CID, and a short TTL. A donor
  cannot fetch a block of its choosing from a peer of its choosing;
  the orchestrator dictates the route.
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
| Multi-master high availability (consensus-replicated coordinators accepting concurrent writes) | Forever out of scope. Requires Raft/Paxos, leader election, split-brain prevention, and reintroduces the consensus complexity the project deliberately avoided. Operators needing this trust model should use a different product. |
| Cold-standby failover for the coordinator | Not shipped, but compatible with the architecture. An operator may run a hot-spare host with Postgres streaming replication and the same `NOVA_MASTER_KEY` versions loaded; failover is manual. See `docs/recipes/COLD_STANDBY.md`. |
| Cross-jurisdiction legal compulsion | Engineering cannot answer "what if a US court compels disclosure of a German-hosted operator's data?" Operators consult counsel. |

## Trust-model choices not implemented

The threats above are things Nova *cannot* defend against. The
items below are things Nova *could* implement but deliberately
does not, because doing so would change the trust model into a
different product. Each is recorded with explicit rationale so the
question "have you considered X?" has a written answer.

### Threshold cryptography for the master key (Shamir / DKG / TKMS)

Some federated systems shard the master decryption key across an
N-of-M committee of trusted parties so that no single host's
compromise exposes the corpus. Tahoe-LAFS approximates this with
its provider-blind grid model.

Nova's design principle is **operator sovereignty**: "you run the
coordinator on your own infrastructure; the project author cannot
turn it off, observe it, or coerce its behavior." Threshold
cryptography replaces that principle with "you trust N of M
committee members," which is fundamentally a different trust model
than "I trust this one operator (or I run my own)."

The mitigations Nova does ship for the silent-compromise residual
risk are master-key rotation (`ENCRYPTION_ENVELOPE.md` § "Master
key versioning"), audit-log shipping (host-level operator
responsibility), and off-box rehearsed backups. Operators whose
threat model genuinely requires N-of-M consensus over key access
should use Tahoe-LAFS or a similar grid-trust storage system rather
than asking Nova to become one.

A future opt-in mode that wraps `NOVA_MASTER_KEY` at backup time
under a Shamir scheme (so the operator's recovery committee shares
the secret, but the running coordinator still holds the unwrapped
key) is compatible with the architecture and documented as an
operator-side pattern in `docs/recipes/KEY_ESCROW.md`. That pattern
preserves operator sovereignty for the read path while reducing
the blast radius of catastrophic key loss; it does not change the
running trust model.

### End-to-end encryption / operator-blindness

Nova is donor-blind, not operator-blind. The coordinator decrypts
plaintext on every read and on every transform; the operator's
master key is process-resident. This is intentional, not a defect.

End-to-end encryption would require client-side keys, would
prevent the on-the-fly image transforms that make `nova-image`
useful at all, and would make content moderation impossible for
the operator. The audiences Nova targets — federated social
servers, FOSS forums, archival projects, ML dataset hosts — need
the operator to be able to inspect content for moderation,
generate derivatives at read time, and serve transformed bytes to
the gateway. These are read-path features, not bugs.

Deployments whose threat model requires operator-blindness should
use a fully end-to-end-encrypted file-sharing system. Nova is the
right tool for "pick an operator you trust, or run your own"; it
is not the right tool for "even the operator can't see my files."

### Private Set Intersection / Zero-Knowledge moderation

Some end-to-end-encrypted platforms moderate against known-bad
hash lists by running PSI between the client's perceptual hash and
the operator's encrypted blocklist, so the operator learns only a
match/no-match boolean and never the client's hash. This is the
right design for E2EE systems.

It is not the right design for Nova. The coordinator already sees
plaintext (per the E2EE rationale above) and runs PDQ directly on
the plaintext at upload (see `PRODUCT_MODULE_INTERFACE.md` step 4,
`AnalyzeUpload`). Adding PSI on top would impose massive
computational overhead on every upload, would add a hash-list
distribution problem (clients must hold the full blocklist), and
would buy nothing the existing pipeline does not already provide.
PSI exists to bridge the gap between privacy and moderation in
systems that cannot decrypt; Nova decrypts.

### S3 API compatibility

Nova exposes HTTP URLs and content-addressed CIDs; S3 exposes
buckets, keys, IAM policies, presigned URLs, object versioning,
and lifecycle policies. The naming model (hierarchical mutable
keys) and the authorization model (pre-shared SigV4 credentials
with bucket-level scope) do not translate cleanly to Nova's
per-blob HMAC token model.

A full S3 API is forever out of scope. A read-only adapter that
maps `GET /s3/bucket/key` → operator-maintained key→CID lookup →
`GET /blob/{cid}` is conceptually possible as a Phase 6+
adapter product layer, but low priority: deployments wanting an
S3 interface are better served by Garage or MinIO. Nova is the
right tool when the integration target is "anything that takes an
HTTP image URL"; it is not the right tool when the target is "any
S3 SDK call I have already written."

Operators may still use S3 alongside Nova — as a ciphertext backup
destination, as an origin for public-archival collection bytes, or
as a Kubo blockstore backend. None of these patterns require Nova
to speak S3 itself.

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
7. **Federation slow-attrition.** A federation that gradually loses
   donors over months can cross the mathematical threshold at which
   the surviving network's aggregate daily budget is insufficient to
   maintain target replication, even if no single failure event
   triggers mass-casualty detection. The `federation.shrinking`
   webhook (see `HEALING_PROTOCOL.md` § "Slow-attrition detection")
   is the early-warning signal. The recovery action — recruiting more
   donors — is a social action the project cannot take on the
   operator's behalf. Operators whose federation crosses this
   threshold should treat it with the same seriousness as a
   mass-casualty event, even though it accumulates quietly.

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
