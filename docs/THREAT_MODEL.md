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
| ① | Internet ↔ nginx | nginx terminates TLS; everything inside the box is trusted by the operator. The public-content origin (`/blob/*`, `/i/*`, `/legal/*`, `/health`, `/api/v1/uploads\|blobs\|images`, the hermetic upload widget at `/widget/*`) and the admin origin (`/admin`, `/api/v1/admin/*`, `/api/v1/auth/*`, `/api/v1/users/me`) live on **distinct virtual hosts** so that an XSS or cache-poisoning at the public origin does not become administrative trust. The admin host can be IP-restricted or mTLS-fronted independently. v3.1 amendment; **implemented in M13** — the split is enforced by the wizard-rendered nginx config (`internal/setup/templates/nova.conf.tmpl`: each vhost closes its default location to 404), templated from the same source as the `nginx/nova.conf.example` reference. The coordinator keeps a single mux; the host split is enforced entirely at nginx. |
| ① a | First-run setup wizard | The **host-facing** wizard surface is loopback-only: nginx-setup binds `127.0.0.1:8444` and the coordinator's API port is never published (`docker compose --profile setup up`). The coordinator's in-container `/setup/*` is still reachable by colocated containers (boundary ②), so a **CSPRNG bootstrap token** — printed to the coordinator log at startup and required as the `X-Nova-Setup-Token` header on every `/setup/*` **API** request (the static wizard page loads token-free) — gates the surface regardless of network position. The seam self-disables permanently once the first successful bootstrap writes the `.bootstrap-complete` sentinel to the config volume (`/etc/nova`). v3.1 amendment; **implemented in M13**, **bootstrap token added in M13.1** — setup mode is a reduced boot of the coordinator (`coordinator.RunSetupServer`) that mounts only the sentinel-gated `/setup/*` seam; nginx serves `bootstrap.conf` (proxying `/setup/*` only) while the sentinel is absent and flips to the two-vhost `nova.conf` once it is present. |
| ② | nginx ↔ coordinator | loopback / unix socket; trusted by colocation |
| ③ | coordinator ↔ master key | environment variable OR file-mount (e.g., Docker secret at `/run/secrets/master-key-<label>`, per the `NOVA_MASTER_KEY_<LABEL> → _FILE → /run/secrets/master-key-<label>` resolver chain); never written to disk by Nova. v3.1 amendment broadens beyond env-var-only; implemented in M6.1. During a master-key rotation window (M10) both the old and new versions are process-resident simultaneously; this is consistent with the existing process-resident-master-key posture and is bounded to the rotation window. |
| ④ | coordinator ↔ donor | Nebula overlay + HTTPS/mTLS; the Nebula cert authorizes mesh membership, the federation client cert authorizes HTTP API calls |
| ⑤ | donor ↔ donor | HTTPS/mTLS inside Nebula; donor-to-donor fetches require a coordinator-issued, source-and-destination-pinned HMAC repair token *(**Phase 2 amendment**: the HMAC token is replaced by an **Ed25519** asymmetric token — donors hold only the public verify key; see § "Phase 2 amendment (donor federation)")*. The private IPFS swarm key gates Kubo's daemon-level peering as defense in depth, but no donor-to-donor data exchange occurs over Bitswap |

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
- **Hermetic admin SPA build (M11).** No third-party CDN assets at
  runtime — IBM Plex is self-hosted and the bundle declares no
  external origin; the `make hermetic-spa` CI gate fails the build on
  any external origin in `web/admin/dist`, and the coordinator serves
  it under a strict `default-src 'self'` CSP (connect-src adds the
  external-OIDC issuer only in external mode, for the browser PKCE
  token exchange). M11 serves `/admin/*` from the coordinator
  (`NOVA_ADMIN_DIST_DIR`); the hardened public/admin two-vhost split
  (boundary ①) is **implemented in M13** by the wizard-rendered nginx
  config — `/admin/*` and `/api/v1/admin/*` live on the admin vhost,
  closed to 404 on the public vhost.
- **Hermetic upload widget (M12).** The same hermetic guarantee covers
  the M12 upload widget (`web/widget/`): Uppy + tus are bundled, CSS
  is injected locally (the widget inlines its CSS into the JS bundle),
  there is no telemetry, and the `hermetic-widget` CI gate fails the
  build on any external origin in `web/widget/dist` — it scans both
  the HTML/CSS (via `hermetic-spa.sh`) and the inlined JS bundle for
  CSS asset-load patterns (`url(http…)`, `@import …http`), making the
  "fails on any external origin" claim precise. Phase-1 widget
  embedding is **same-origin** (the host page is served from the Nova
  origin); cross-origin embedding requires operator-managed CORS at
  the reverse proxy and is deferred. No first-class coordinator CORS
  surface ships in Phase 1. As of M13 the widget bundle is served on
  the **public_host** of the two-vhost split (it is the end-user
  uploader, and its API target `/api/v1/uploads/*` is a public-origin
  route) — never on the admin host.
- **No telemetry, no phone-home, no auto-update** in the
  coordinator or donor binaries. There is no mechanism for an
  attacker to push a malicious update through Nova's own update
  channel because there is no update channel.

**Residual risk.** A compromised pinned dependency that is updated
in a future release would still ship malicious code. Phase 5 ships
release signing (sigstore / cosign) for the coordinator; until then,
operators verify release artifacts against the project's published
checksums. **Phase 2 amendment:** because Phase 2 instructs volunteers
to `docker pull` and run a privileged network daemon (`nova-node`),
release signing + SBOM + provenance for the **donor** image is promoted
into Phase 2 — the donor walkthrough pins an image digest (never
`latest`) and verifies the signature. See § "Phase 2 amendment (donor
federation)".

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

## Phase 2 amendment (donor federation)

Status: **design — pending P2-M0 ratification.** Phase 2 adds machines the
operator does not own (volunteer donor nodes), which grows the donor-facing
attack surface. This section enumerates the new adversaries and the mitigations
now specified, and records which earlier statements in this document it
**supersedes**. The canonical reasoning is in
`docs/superpowers/specs/phase2/2026-06-11-phase2-federation-design.md`
§ "Trust-model exploration"; the load-bearing spec edits land in P2-M0.

### Statements superseded for Phase 2

- **Boundary ⑤ — repair token.** The "HMAC repair token" is replaced by an
  **Ed25519** asymmetric token. HMAC is symmetric: a verify key handed to every
  donor would let any donor *mint* tokens. Donors now hold only the coordinator's
  Ed25519 public key; tokens are single/bounded-use (source-side `jti` replay
  cache), carry `max_bytes` + a short TTL, and bind `cid`, `source_node_id`,
  `dest_node_id`, `assignment_id`, and `generation`.
- **Actor A — CID self-validation.** "Re-hashes the envelope on receipt" is exact
  only for a single raw block. For any multi-block object the CID is a UnixFS /
  DAG-PB **root**, not `sha256(bytes)`; the destination donor verifies by
  **deterministic re-import (`IPFS_IMPORT_RULES`) and root-CID comparison**.
- **Actor G — release signing.** Promoted from Phase 5 to Phase 2 for the donor
  image (see the amended residual-risk note above).

### New adversaries

- **A′. Repair-token forgery / replay.** A donor minting tokens, or a malicious
  destination replaying a valid token to drain a source's daily budget.
  *Mitigation:* Ed25519 (public-key verification only at donors); single/bounded
  use; `max_bytes`; short TTL; source/dest/assignment/generation binding.
- **A″. Assignment replay.** A delayed `ack`/`fail` for a superseded assignment
  mutating a **reused** `(cid, node_id)` row — marking a stale copy "durable" or
  cancelling a live one. *Mitigation:* immutable `assignment_id` + `generation`;
  conditional state transitions (`… WHERE assignment_id=$ AND generation=$ AND
  state='pending'`).
- **H. Sybil / failure-domain forgery.** One donor-operator registers many
  nominal nodes (distinct *declared* geos) on a single host / provider / account
  to capture placement weight and defeat anti-affinity, or a brand-new node
  becomes a sole copy of critical content. *Mitigation:* placement uses
  **operator-verified `failure_domain_id` / `donor_principal_id`** (self-declared
  geo is informational only); an **orthogonal `trust_state`** (probationary /
  trusted / suspended) caps `placement_weight`; probationary nodes are **never**
  the sole or second copy of `important`-class data and are audited more often;
  they graduate on age + successful transfers + passed audits. This corrects the
  `VOLUNTEER_DEPLOYMENT_GUIDANCE.md` claim that same-owner nodes are
  auto-anti-affined — the schema had no owner identifier; Phase 2 adds one.
- **I. Audit collusion / backdating.** A lying donor satisfies a possession
  challenge via a co-located cache, a colluding fast peer, or by backdating its
  self-reported `completed_at`. *Mitigation:* the deadline uses **coordinator
  receive-time**, not donor-supplied time; the challenge is a **synchronous**
  single round-trip the coordinator times; the repair transport is Ed25519-token-
  gated and Bitswap-disabled, so a donor under audit cannot lawfully fetch the
  block in-window. Honest framing: audits prove **timely retrievability under the
  node identity**, *not* unique physical residency or independent failure domains.
- **J. Bandwidth exhaustion.** A coordinator bug or hostile schedule overshoots a
  donor's agreed budget, tripping its ISP/provider commercial-use heuristics — the
  exact harm boundary E already worries about. *Mitigation:* Tier-1 "no doomsday
  override" (`HEALING_PROTOCOL.md`) is enforced at the node that actually sends
  bytes: the **donor's local token-bucket is authoritative** and refuses
  over-budget work; coordinator scheduling is only a best-effort reservation;
  tokens carry `max_bytes`; actuals reconcile via heartbeat.

### Boundary extension

```
 ④ coordinator ↔ donor : Nebula + mTLS; identity from the VERIFIED cert (not
                          self-asserted JSON); capability-negotiated at register
 ⑤ donor ↔ donor       : mTLS + Ed25519 token (single-use, max_bytes, src/dst/gen)
 ⑥ donor budget        : donor-local token-bucket is the AUTHORITATIVE enforcer
 ⑦ possession audit    : coordinator-receive-time deadline; synchronous; Bitswap-
                          disabled; sampling weighted by stored bytes / age / risk
 Placement anti-affinity keys on operator-verified failure_domain_id /
 donor_principal_id (④), not self-declared geo. trust_state caps placement weight.
```

### New residual risks (Phase 2)

8. **Audits are retrievability sampling, not proof-of-replication.** A donor that
   can produce challenged bytes quickly passes, even if those bytes live on a
   shared backend or a fast colluding peer. This is acceptable for a
   coordinator-administered (non-permissionless) network; formal PDP/POR is Phase
   6+.
9. **Failure-domain truth depends on operator verification.** Placement safety is
   only as good as the operator's assignment of `failure_domain_id`; a lazy
   operator who trusts self-declared geo regains the Sybil exposure H targets.
10. **Donor metadata correlation persists.** Even metadata-minimal assignments let
    a donor correlate pin timing against publicly-visible uploads (boundary-A
    residual risk), now across a larger node set.

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
