# Architecture Decisions

Status: **Phase 0 v3 — normative.** Classifies every architectural
decision Nova has made into one of three tiers. Future contributors
proposing a change must identify which tier the change touches and
follow that tier's amendment rules.

## Purpose

Nova has accumulated specifications for the encryption envelope, the
federation protocol, the healing algorithm, integrity and possession
audits, signed URLs, IPFS import determinism, the threat model, and
several others. Each spec internally documents what it requires.
What has been missing is a single document that answers:

- Which choices are **protocol-enforced** (the same in every
  conforming deployment, not configurable, refused at startup if
  violated)?
- Which choices are **operator-tunable** (parameters with defaults,
  bounded by sane ranges, set in `operator.yaml`)?
- Which choices are **entirely the operator's** (outside the
  protocol's scope; Nova has no opinion)?

This document is the answer. It is also where the project records,
in one place, what it has deliberately chosen **not** to do, with
the rationale tied to design principles rather than scattered
across individual spec out-of-scope sections.

## Why a three-tier framework

The pull between protocol rigidity and deployment flexibility is the
defining tension of any federated system. A protocol that mandates
too much produces brittle deployments that cannot adapt to local
conditions; a protocol that mandates too little produces deployments
that fragment into incompatible silos. The three-tier framework
makes this tension explicit:

- **Tier 1 (Protocol-enforced)** is the layer where interoperability,
  cryptographic safety, and the trust model itself live. Conforming
  implementations must agree on every Tier 1 item or they are not
  speaking the same protocol.
- **Tier 2 (Operator-tunable)** is the layer where federations adapt
  to their own hardware, scale, and threat tolerance. Two
  federations that disagree on Tier 2 are still both running Nova.
- **Tier 3 (Operator freedoms)** is everything outside the protocol:
  hardware, hosting, funding, governance, social structure. Nova
  takes no position.

Amendment process scales with tier. Tier 1 changes require a v-bump
of the affected spec and a corresponding implementation gate (the
pattern Phase 0 v2 established). Tier 2 changes are normal
operator decisions documented in deployment notes. Tier 3 is
entirely outside the project.

## Tier 1: Protocol-enforced and immutable

These are the architectural commitments that conforming
implementations must honor. The coordinator and donor binaries
refuse to start when these are violated. Operator configuration
cannot opt out.

### Cryptographic substrate

| # | Decision | Where enforced |
|---|---|---|
| T1.1 | Per-blob symmetric encryption (XChaCha20-Poly1305) for all stored blobs except `public_archival` collections | `ENCRYPTION_ENVELOPE.md` § "Envelope wire format" |
| T1.2 | The envelope wire format starts with NOVE magic + 1-byte version + 1-byte algorithm; readers dispatch on `version` byte. v1 (Phase 1) is single-shot; v2 (Phase 2) is chunked streaming AEAD. Both formats remain decryptable forever. | `ENCRYPTION_ENVELOPE.md` § "Envelope wire format" + § "Planned v2: Streaming-AEAD" |
| T1.3 | Per-blob keys are 256-bit CSPRNG-generated; never derived from CIDs; never persisted plaintext | `ENCRYPTION_ENVELOPE.md` § "Per-blob key generation" |
| T1.4 | Master-key wrapping with XChaCha20-Poly1305; per-row `master_key_version_id` tracks which master key wrapped each entry | `ENCRYPTION_ENVELOPE.md` § "Master key versioning" |
| T1.5 | Crypto-shred refuses when `data_encryption_keys.legal_hold = true` | `ENCRYPTION_ENVELOPE.md` § "Pre-conditions" |
| T1.6 | The CID stored in `blobs.cid` is the CID of the entire envelope (header, ciphertext, tag) — true for both v1 and v2 | `ENCRYPTION_ENVELOPE.md` § "Envelope wire format" |
| T1.7 | Deterministic IPFS import (CID v1, sha2-256, base32, fixed 256 KiB chunker, balanced layout). **(P2-M0)** The v2 streaming-AEAD **record ↔ block mapping is authoritative in P2-M8**; the earlier "chunk N == block N" phrasing is superseded pending that milestone. v1 determinism is unchanged. | `IPFS_IMPORT_RULES.md` |
| T1.7a | (Phase 2) v2 streaming-AEAD envelope: per-chunk XChaCha20-Poly1305 with chunk-counter-derived nonces. **(P2-M0)** The per-chunk **AAD construction is authoritative in P2-M8** — it binds a canonical header commitment, not the final `cid` (which is circular); the earlier "AAD binds chunk_index/total_chunks/cid" phrasing is superseded pending that milestone. Range reads supported on v2 encrypted blobs. | `ENCRYPTION_ENVELOPE.md` § "Planned v2: Streaming-AEAD" |

### Transport and federation

| # | Decision | Where enforced |
|---|---|---|
| T1.8 | All federation traffic transits a private Nebula overlay; donor inbound endpoints bind only to the Nebula interface | `FEDERATION_PROTOCOL.md` § "Authentication" |
| T1.9 | HTTPS + mTLS over Nebula with a separate federation client cert; Nebula cert authorizes overlay, federation cert authorizes HTTP API | `FEDERATION_PROTOCOL.md` § "Authentication (v2)" |
| T1.10 | Donor-to-donor repair fetches require a coordinator-issued, source-and-destination-pinned **Ed25519** repair token (asymmetric — donors hold only the public key; single/bounded-use via a `jti` replay cache). **(P2-M0: was HMAC; FED v3 is the implementation gate.)** | `FEDERATION_PROTOCOL.md` § "Repair transport" |
| T1.11 | Bitswap-backed repair fetch is explicitly disabled; the orchestrator dictates source designation | `FEDERATION_PROTOCOL.md` § "Repair transport"; `HEALING_PROTOCOL.md` |
| T1.12 | Bandwidth budgets are inviolable; no doomsday override | `HEALING_PROTOCOL.md` § "Why bandwidth budgets are inviolable" |
| T1.13 | Five-state liveness model (`active`/`suspect`/`unreachable`/`evicted`/`revoked`); healing engages at `unreachable`, not `evicted` | `FEDERATION_PROTOCOL.md` § "Liveness state machine" |
| T1.14 | Tier 1 healing (CIDs at one **acked** pin) takes strict priority over Tier 2; no interleaving. **(P2-M0)** Durability is **acked-only** — pending assignments are never durable and never lift a CID out of Tier 1. | `HEALING_PROTOCOL.md` § "Why Tier 1 is strict" |
| T1.29 | **(P2-M0)** Replica placement applies operator-verified **failure-domain anti-affinity** as a **soft preference (never a veto)**, and steady-state placement weight is **decoupled from donor bandwidth** (bandwidth governs repair-source selection only). Self-declared geo is informational. | `HEALING_PROTOCOL.md` § "Reputation and audit-aware placement"; phase6 resilience design |
| T1.30 | **(P2-M0)** Donor `trust_state` (`probationary`/`trusted`/`suspended`), orthogonal to liveness and reputation, caps `placement_weight`; a **probationary node is never the sole or second copy of `important`-class data**. | `HEALING_PROTOCOL.md` § "Reputation and audit-aware placement" |

### IPFS hardening

| # | Decision | Where enforced |
|---|---|---|
| T1.15 | In private mode (default), Kubo's public DHT, mDNS, providership announcement, and reprovider strategy are all disabled; daemon refuses to start otherwise | `KUBO_HARDENING.md` |
| T1.16 | Kubo API and Gateway bind to loopback only | `KUBO_HARDENING.md` |
| T1.17 | `IPFS_SWARM_KEY` is required in private mode; donor refuses to start without it | `KUBO_HARDENING.md` § "Private swarm key" |
| T1.18 | Bootstrap entries must resolve to loopback, RFC 1918, or the configured Nebula overlay subnet; public-internet entries are rejected | `KUBO_HARDENING.md` § "Bootstrap peer requirements" |

### Operator security floors

| # | Decision | Where enforced |
|---|---|---|
| T1.19 | Coordinator refuses to start with `auth: anonymous` AND `moderation: off` set simultaneously | `THREAT_MODEL.md` § F; `PRIVACY_AUDIT.md` |
| T1.20 | Coordinator refuses to start with public uploads enabled and no `tos_url` | `PRIVACY_AUDIT.md` |
| T1.21 | Container processes run as a non-root user; the validator refuses root | `PRIVACY_AUDIT.md` |
| T1.22 | No telemetry, no phone-home, no auto-update channel in coordinator or donor binaries | `THREAT_MODEL.md` § G; `PRIVACY_AUDIT.md` |
| T1.23 | Admin SPA bundle has no third-party CDN assets at runtime; CI lint enforces | `THREAT_MODEL.md` § D |
| T1.24 | `audit_log` is append-only at the application layer; every privileged action is logged | `THREAT_MODEL.md` § B |
| T1.31 | Admin-minted **upload tokens** are capped to the `uploader` role (never operator/moderator), are optionally scoped (collection / product / max file size) and one-click revocable; the `nova_ut_…` secret is shown once and stored only as a SHA-256 hash | `docs/superpowers/specs/2026-06-13-m0.3-offorigin-widget-design.md`; `internal/api/handlers/upload_tokens_admin.go`; `THREAT_MODEL.md` |

### Trust-model commitments

| # | Decision | Where enforced |
|---|---|---|
| T1.25 | Donor-blind storage: a conforming federation never asks donors to hold plaintext; donors hold envelope ciphertext only | `ENCRYPTION_ENVELOPE.md`; `THREAT_MODEL.md` |
| T1.26 | Not operator-blind: the coordinator decrypts on every read and on transform; this is intentional, not a defect | `ENCRYPTION_ENVELOPE.md` § "Trust-model note"; `THREAT_MODEL.md` |
| T1.27 | A federation has exactly **one logical authoritative state history**. Multiple coordinator replicas MAY serve concurrently when sharing that one strongly-consistent authority; global control-plane operations require a fenced leader term. Independent concurrent authoritative histories (multi-master with divergent writable databases) remain prohibited | `THREAT_MODEL.md` § "Out of scope"; 2026-06-12 resilience design |
| T1.28 | Federations remain independently governed and independently keyed. Optional **inter-federation peering** MAY replicate **opaque ciphertext** and encrypted recovery packages only — never user catalogs, moderation authority, legal holds, master keys, or assignment histories; every object has exactly one home federation, and peers never receive plaintext or active DEKs | `FEDERATION_PROTOCOL.md` § "Out of scope"; `HEALING_PROTOCOL.md` § "Out of scope"; 2026-06-12 resilience design |

A Tier 1 change requires a spec version bump, an implementation
gate, and a corresponding update to this table.

**P2-M0 (2026-06-13)** amended `T1.10` (HMAC → Ed25519), clarified `T1.14`
(acked-only durability), flagged `T1.7`/`T1.7a` as v2-layout/AAD-authoritative in
P2-M8, and added `T1.29` (failure-domain anti-affinity + bandwidth-decoupled
placement) and `T1.30` (donor trust/probation), with the `FEDERATION_PROTOCOL.md`
v3 and `HEALING_PROTOCOL.md` v3 spec bumps as the implementation gates. See
`docs/superpowers/specs/phase2/2026-06-13-phase2-m0-spec-reconciliation-design.md`.

**P2-M0.3 (2026-06-14)** added `T1.31` (the scoped, revocable, uploader-capped
upload-token credential) as part of the off-origin widget work, alongside the
first-class CORS layer and upload admission limits recorded in Tier 2. See
`docs/superpowers/specs/2026-06-13-m0.3-offorigin-widget-design.md`.

`T1.27` and `T1.28` were **reframed** (not relaxed) by the second-pass
resilience analysis in
`docs/superpowers/specs/phase6/2026-06-12-resilience-and-post-1.0-architecture-design.md`.
The single-coordinator deployment remains correct and sufficient for the entire
1.0 line; the reframing makes room for two deliberate **post-1.0** phases —
multi-coordinator single-authority HA (Phase 6) and opaque inter-federation
replica peering (Phase 7) — without ever permitting two authorities to diverge.
Until those phases land, every conforming deployment is single-coordinator and
single-federation.

## Tier 2: Operator-tunable parameters

These parameters have defaults that work for typical deployments
and bounded ranges that prevent unsafe configurations. Operators
set them in `operator.yaml` based on their hardware, scale, and
threat tolerance.

### Replication and healing

| Key | Default | Range | Where documented |
|---|---|---|---|
| `replication.factor.important` | 3 | 1..20 | `HEALING_PROTOCOL.md` |
| `replication.factor.normal` | 3 | 1..20 | `HEALING_PROTOCOL.md` |
| `replication.factor.cache` | 2 | 1..20 | `HEALING_PROTOCOL.md` |
| `tick_interval_seconds` | 60 | 5..600 | `HEALING_PROTOCOL.md` |
| `step_seconds` | 60 | 10..600 | `HEALING_PROTOCOL.md` |
| `mass_casualty_threshold_ratio` | 0.20 | 0.05..0.50 | `HEALING_PROTOCOL.md`; `FEDERATION_PROTOCOL.md` |
| `mass_casualty_window_seconds` | 3600 | 60..86400 | `HEALING_PROTOCOL.md`; `FEDERATION_PROTOCOL.md` |
| `capacity_runway_floor_days` | 7 | 1..90 | `HEALING_PROTOCOL.md` § "Slow-attrition detection" |
| `reputation_floor` | 0.5 | 0.0..1.0 | `HEALING_PROTOCOL.md`; `POSSESSION_AUDIT.md` |

### Diversity and concentration alerting (alert, not prevent)

Nova **alerts** on dangerous network homogeneity; it does **not** refuse to place
a replica purely for homogeneity. Placement gains *soft* failure-domain
anti-affinity (a preference, never a veto — a hard ceiling could block healing
into the only surviving capacity during a casualty). These webhooks parallel the
existing `federation.degraded` / `federation.shrinking` signals. Rationale and
simulation evidence: the 2026-06-12 resilience design.

| Key | Default | Range | Where documented |
|---|---|---|---|
| `concentration.largest_share_warn` | 0.30 | 0.10..0.90 | 2026-06-12 resilience design |
| `concentration.normalized_entropy_warn` | 0.50 | 0.0..1.0 | 2026-06-12 resilience design |
| `concentration.check_interval_seconds` | 3600 | 60..86400 | 2026-06-12 resilience design |

Webhooks: `federation.concentrated` (a failure-domain dimension —
provider / ASN / region / principal — exceeds `largest_share_warn`) and
`federation.homogeneous` (a dimension's normalized entropy falls below
`normalized_entropy_warn`). Dashboard metrics: pin-incidence Gini (per node) and
per-dimension largest-share / top-k share / normalized entropy.

### Federation and liveness

| Key | Default | Range | Where documented |
|---|---|---|---|
| `heartbeat_interval_seconds` | 300 | 60..3600 | `FEDERATION_PROTOCOL.md` |
| `pins_poll_interval_seconds` | 600 | 60..3600 | `FEDERATION_PROTOCOL.md` |
| `suspect_after_missed_heartbeats` | 3 | 2..10 | `FEDERATION_PROTOCOL.md` |
| `unreachable_after_seconds` | 3600 | 300..86400 | `FEDERATION_PROTOCOL.md` |
| `evicted_after_seconds` | 2_592_000 | 86400..7776000 | `FEDERATION_PROTOCOL.md` |
| `repair_token_ttl_seconds` | 300 | 60..1800 | `FEDERATION_PROTOCOL.md` |
| `max_pin_concurrency` | 16 | 1..256 | `FEDERATION_PROTOCOL.md` |

### Audits

| Key | Default | Range | Where documented |
|---|---|---|---|
| `integrity_audit.*.interval` | per audit kind | 0 disables in dev only | `INTEGRITY_AUDIT.md` |
| `integrity_audit.*.sample_size` | per audit kind | 1..10000 | `INTEGRITY_AUDIT.md` |
| `possession_audit.per_node_interval_seconds` | 3600 | 60..86400 | `POSSESSION_AUDIT.md` |
| `possession_audit.challenge_deadline_seconds` | 30 | 5..300 | `POSSESSION_AUDIT.md` |

### Moderation and legal

| Key | Default | Notes | Where documented |
|---|---|---|---|
| `dmca.default_action` | `quarantine_first` | also: `immediate_tombstone` | `DMCA_PROCEDURE.md` |
| `dmca.counter_notification_days` | 14 | 10..21 | `DMCA_PROCEDURE.md` |
| `moderation.pdq_threshold` | per operator policy | informed by PDQ recommendations | `SEVERE_CONTENT_PROCEDURE.md`; `PRODUCT_MODULE_INTERFACE.md` |
| `legal_hold.statutory_retention_days` | per jurisdiction | operator consults counsel | `SEVERE_CONTENT_PROCEDURE.md` |

### Authentication

| Key | Default | Notes | Where documented |
|---|---|---|---|
| `auth.issuer_url` | empty (built-in local issuer) | non-empty ⇒ verify-only external OIDC; local issuer endpoints 404 `external_oidc_active` | M6 design `docs/superpowers/specs/phase1/2026-05-30-phase1-m6-auth-design.md` |
| `auth.role_claim` | `groups` | external-OIDC claim read for role mapping | M6 design |
| `auth.role_mapping` | operator-supplied | maps IdP group/scope strings → Nova roles; unmapped ⇒ `viewer` (safe default) | M6 design |
| `uploads.public_uploads` | `false` | `true` allows anonymous uploads; refuse-to-start without `tos_url` (T1.20) | M6 design; `PRIVACY_AUDIT.md` |
| upload tokens | n/a (minted on demand) | scoped, revocable `nova_ut_…` bearer credentials for off-origin widgets; minted via `POST /api/v1/admin/upload-tokens` or `novactl upload-token create`, always `uploader`-capped (T1.31) | M0.3 design |

The coordinator is a resource server: it accepts bearer tokens from the local
issuer and/or an operator-chosen external IdP (operator freedom; see Tier 3),
but never mediates the interactive login flow. Role mapping is operator policy —
a misconfiguration can over- or under-grant, so `viewer` is the unmapped default.

### Network mode

| Key | Default | Notes | Where documented |
|---|---|---|---|
| `coordinator.public_ipfs_dht` | `false` | opt-in for `nova-archive`-style public data | `KUBO_HARDENING.md` § "Public IPFS DHT mode" |
| `coordinator.record_source_ip` | unset ⇒ record (the `paranoid` preset defaults it off) | tri-state `*bool`; an explicit value wins over the preset, decoupling source-IP recording from full paranoid (P2-M0.2) | `PRIVACY_AUDIT.md` § "paranoid: true mode" |
| `tls.mode` | operator-prompted at first run | `http-01` / `dns-01` / `static` / `.onion` | `PRIVACY_AUDIT.md` § "TLS mode" |
| `paranoid` | `false` | **preset, not force** (P2-M0.2): sets protective privacy defaults; explicit operator values win and relaxing one only *warns* — it never refuses to start. Legal floors (T1.20, `auth.anonymous`) stay hard. | `PRIVACY_AUDIT.md` § "paranoid: true mode" |

### Cross-origin uploads and admission limits

First-class CORS (default off; same-origin only) lets an operator authorize an
upload widget hosted on their own site. When enabled, an allowlisted `Origin` is
**echoed** with `Vary: Origin` (never `*`), preflight `OPTIONS` short-circuits to
204 before the auth guard, and the tus method/header lists default to sane values
(P2-M0.3). The admission limits bound a bulk-upload burst so the client queues
rather than triggering a 503/429 storm; over-limit returns **429** with
`Retry-After` (distinct from the storage-saturation **503** `server_busy`).

| Key | Default | Notes | Where documented |
|---|---|---|---|
| `uploads.cors.enabled` | `false` | `true` activates the CORS layer on the upload routes; requires `allowed_origins` | M0.3 design |
| `uploads.cors.allowed_origins` | empty | exact-match origin allowlist (echoed, never `*`) | M0.3 design |
| `uploads.cors.allow_credentials` | `false` | Bearer auth, no cookies; keep `false` | M0.3 design |
| `uploads.limits.max_concurrent_global` | 16 | global in-flight upload ceiling; over-limit ⇒ 429 `too_many_concurrent` | M0.3 design |
| `uploads.limits.max_concurrent_per_session` | 4 | per-credential in-flight ceiling; mirrors the widget tus `limit` | M0.3 design |
| `uploads.limits.max_files_per_session` | 100 | per-credential active in-progress session ceiling; over-limit ⇒ 429 `too_many_files` | M0.3 design |

Tier 2 changes are normal operator decisions. They produce different
deployments, not different protocol versions.

## Tier 3: Operator freedoms

These are the choices Nova takes no position on. They are listed
here so contributors know not to propose protocol changes that would
constrain them, and so operators know they are not protocol
violations.

### Physical and economic deployment

- Hardware selection and topology — bare metal, VPS, containers, bare-metal-at-residential, hot-spare server, cold-standby, etc.
- Backend storage for the Kubo blockstore — local NVMe, network-attached storage, S3-mounted filesystem, ZFS, btrfs, anything Kubo can read.
- Geographic distribution of donor nodes.
- Bandwidth allowance the operator negotiates with their hosting provider.
- Funding model — donations, grants, sponsorship, internal organization budget, subscription, sponsorship-tiered access. Nova does not ship a payment surface.
- Whether to colocate Postgres, Kubo, and the coordinator on one host or split them across multiple hosts.

### Donor lifecycle and onboarding

- How donor candidates are vetted prior to receiving a Nebula cert + `swarm.key` bundle. The protocol provides the cryptographic gates (cert issuance, revocation, reputation) the operator uses to enforce whatever policy they choose.
- Whether to issue invites manually, through a closed application form, through an automated portal with proof-of-work, or any other mechanism the operator builds on top of the cert-issuance API. A reference pattern is documented in `docs/recipes/AUTOMATED_ONBOARDING.md` but is not part of the protocol.
- Identity verification thresholds — none, email confirmation, OIDC binding, KYC, deposit, character references. Operator policy.
- Whether to require diversity (geographic, ISP, provider, ASN) in the donor pool. Operator policy informed by `HEALING_PROTOCOL.md` empirical thresholds and `OPERATOR_CHECKLIST.md` recommendations. Nova *alerts* on dangerous homogeneity (Tier 2 § "Diversity and concentration alerting") but never enforces a diversity floor — acting on the alert is the operator's freedom.

### Governance and community

- How content-policy decisions get made — operator decree, council vote, community consensus, appeals process, none of the above. Nova provides audit logs, possession audits, and configurable thresholds; the social structure on top is the operator's.
- Whether to publish a supporters page, a transparency report, an annual statement, or any other community-facing artifact.
- How moderation appeals are handled — staff-only, community-elected appeals board, external arbitration, none.
- Specific moderation hash lists — Nova ships PDQ scanning per `SEVERE_CONTENT_PROCEDURE.md`; which lists (StopNCII, NCMEC, operator-curated, regional) is operator policy.

### Integration patterns

- Whether the operator runs adapter packages for specific application stacks (Mastodon plugin, forum integration, etc.). These adapters live in separate repositories and are not part of the protocol.
- Whether to front the coordinator with a CDN (and accept the plaintext-cache implication documented in `CLOUDFLARE.md`).
- Whether to integrate with external SSO (Authelia, generic OIDC, SAML). Nova accepts bearer tokens; the issuer is operator-chosen.
- Whether to run a hot-spare coordinator on cold standby for read-availability after primary failure. The architecture is compatible (see `docs/recipes/COLD_STANDBY.md`) but Nova does not ship the failover orchestration. (Automating and hardening this pattern into multi-coordinator single-authority HA is the proposed post-1.0 Phase 6 — see `ROADMAP.md` and the 2026-06-12 resilience design.)

## What this document deliberately excludes

This is not an exhaustive parameter reference; for that, see each
spec's "Configuration parameters" section. This document is the
classification layer: which tier governs each decision, and what
the amendment rules are.

This is also not a roadmap; for that, see `ROADMAP.md`. The tier
classification stays stable across roadmap phases.

## Cross-references

- `ENCRYPTION_ENVELOPE.md` — Tier 1 cryptographic substrate
- `FEDERATION_PROTOCOL.md` — Tier 1 transport and federation
- `HEALING_PROTOCOL.md` — Tier 1 healing invariants and Tier 2 tunables
- `KUBO_HARDENING.md` — Tier 1 IPFS hardening
- `IPFS_IMPORT_RULES.md` — Tier 1 CID determinism
- `INTEGRITY_AUDIT.md`, `POSSESSION_AUDIT.md` — Tier 2 audit cadences
- `THREAT_MODEL.md` — explicit out-of-scope rationale (the boundary between Tier 1 and what Nova will never do)
- `OPERATOR_CHECKLIST.md` — operator-facing summary of what to actually set
- `docs/recipes/` — Tier 3 patterns the operator builds on top
