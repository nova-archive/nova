# Roadmap

## Phase 0 v3 — Specifications (current)

Lock the protocol and contracts as documents before any production
code is written. The original Phase 0 was completed, then revised in
a v2 consistency pass after design audits identified contradictions
and gaps (see `docs/REVIEW_2026_05_09.md`), then revised again in v3
after a second round of architectural review identified document
drift and missing classification (see `docs/REVIEW_2026_05_19.md`).

Original Phase 0 deliverables (still complete, now updated to v2):
- [x] Repository skeleton
- [x] OpenAPI specification (`docs/specs/openapi.yaml`)
- [x] Signed URL format (`docs/specs/SIGNED_URL_FORMAT.md`) — v2 structured revocation
- [x] Data model (`docs/specs/DATA_MODEL.sql`) — v2 split keys, derivatives-as-blobs, 5-state liveness
- [x] Encryption envelope (`docs/specs/ENCRYPTION_ENVELOPE.md`) — v2 master-key versioning, narrowed GDPR claim
- [x] Federation protocol (`docs/specs/FEDERATION_PROTOCOL.md`) — v2 mTLS, donor-to-donor repair
- [x] IPFS daemon hardening (`docs/specs/KUBO_HARDENING.md`)
- [x] Product module interface (`docs/specs/PRODUCT_MODULE_INTERFACE.md`) — v2 split AnalyzeUpload + OnCommitted, format conversion
- [x] Healing protocol (`docs/specs/HEALING_PROTOCOL.md`) — v2 5-state liveness, configurable R, source-enforcement
- [x] Orchestrator resilience simulation (`simulations/orchestrator_resilience.py`)
- [x] Threat model (`docs/THREAT_MODEL.md`)
- [x] Privacy audit (`docs/PRIVACY_AUDIT.md`)
- [x] Operator checklist (`docs/legal/OPERATOR_CHECKLIST.md`) — v2 narrower [REQUIRED]
- [x] ToS template (`docs/legal/TOS_TEMPLATE.md`)
- [x] Takedown procedure (`docs/legal/DMCA_PROCEDURE.md`) — v2 quarantine-first default
- [x] Volunteer deployment guidance (`docs/VOLUNTEER_DEPLOYMENT_GUIDANCE.md`)
- [x] nginx reference config (`nginx/nova.conf.example`)
- [x] nginx walkthrough (`docs/recipes/NGINX_REFERENCE.md`)
- [x] Phase 0 dependency-only `docker-compose.yml`
- [x] Cloudflare recipe (`docs/recipes/CLOUDFLARE.md`) — v2 reframed as optional

v2 additions (new specs from the design audit):
- [x] IPFS import rules (`docs/specs/IPFS_IMPORT_RULES.md`) — deterministic CIDs for proof-readiness
- [x] Integrity audit (`docs/specs/INTEGRITY_AUDIT.md`) — Phase 1 local fixity
- [x] Possession audit (`docs/specs/POSSESSION_AUDIT.md`) — Phase 2 donor spot-checks
- [x] Severe content procedure (`docs/legal/SEVERE_CONTENT_PROCEDURE.md`) — Phase 4 implementation
- [x] Review summary (`docs/REVIEW_2026_05_09.md`) — what changed and why

v3 additions (architectural classification and slow-attrition handling):
- [x] Architecture decisions (`docs/specs/ARCHITECTURE_DECISIONS.md`) — three-tier classification (protocol-enforced / operator-tunable / operator freedom)
- [x] `THREAT_MODEL.md` boundary ⑤ corrected (HTTPS-over-Nebula, not libp2p); explicit out-of-scope rationale for threshold cryptography, end-to-end encryption, PSI moderation, S3 API, and multi-master HA; new residual-risk entry for slow attrition
- [x] `HEALING_PROTOCOL.md` slow-attrition detection (`federation.shrinking` webhook + `capacity_runway_floor_days`)
- [x] Operator recipes (`docs/recipes/AUTOMATED_ONBOARDING.md`, `KEY_ESCROW.md`, `COLD_STANDBY.md`) — patterns operators build on top of the protocol
- [x] Simulations: `sybil_concentration.py`, `long_tail_churn.py`, `key_rotation_load.py`
- [x] Review summary (`docs/REVIEW_2026_05_19.md`) — what changed and why

## Phase 1 — Single-node MVP

Standalone coordinator with embedded hardened IPFS daemon, Postgres,
nginx + certbot, signed-URL HMAC, per-blob encryption on by default,
on-the-fly image transforms, drag-and-drop upload widget. Exports
`pkg/coordinator` and `pkg/node` as semver-stable Go library packages.

v2 promotions into Phase 1:
- Master-key rotation tooling (`novactl keys rotate-master`),
  was previously deferred to Phase 5.
- Local integrity audits running in the background; admin UI
  surfaces recent failures.
- Deterministic IPFS import per `IPFS_IMPORT_RULES.md`; blob
  manifests + blob_blocks recorded for every upload.
- Quarantine-first DMCA flow with scheduled tombstone job.
- Manual operator path for severe-content quarantine + legal-hold
  via `novactl moderation quarantine ... --legal-hold`.

## Phase 2 — Federation

Split coordinator from pinning-node binary. Mesh-VPN-authenticated
federation, replication-factor enforcement, donor-operated nodes.

v2 additions:
- HTTPS+mTLS auth inside Nebula with separate federation client
  certs (Nebula cert authorizes overlay; federation cert authorizes
  HTTP API).
- Donor-to-donor controlled repair transport with HMAC-signed,
  source-and-destination-pinned repair tokens. No Bitswap-backed
  repair fetch.
- Five-state node liveness (active / suspect / unreachable /
  evicted / revoked) with separate timers; healing engages at
  unreachable (~1h), not at evicted (30d).
- Possession audits (per `POSSESSION_AUDIT.md`): challenge-response
  spot-checks, donor reputation tracking, audit-aware placement.
- Incremental change-log endpoint (`/fed/v1/pins/changes`) plus
  snapshot recovery path with snapshot_epoch consistency.

## Phase 3 — Dedup and moderation

Perceptual hash index, near-duplicate detection, content-moderation
pipeline.

## Phase 4 — Adapters, SDKs, and severe-content workflow

Adapter packages for fediverse and forum software (separate
repositories). Auto-generated client SDKs in TypeScript, Python,
Swift.

v2 addition: full severe-content workflow per
`SEVERE_CONTENT_PROCEDURE.md`:
- PDQ scan against StopNCII at upload (synchronous reject for
  clear matches, quarantine + legal-hold for ambiguous).
- NCMEC CyberTipline report generation.
- Admin SPA legal-hold clearance UI.
- Audit-log export for evidence packaging.

## Phase 5 — Hardening

Chaos testing, security audit, documentation polish, public 1.0.

## Phase 6+ — Research

Speculative directions: end-user client direct integration, browser-
resident pinning via WASM, FFI bindings for non-Go embedding,
additional product modules (`nova-video`, `nova-audio`, `nova-archive`,
`nova-document`), read-only secondary coordinator for read-availability
during primary failover, streaming-AEAD envelope variant for blobs
that exceed memory, formal Provable Data Possession / Proof of
Retrievability, hot-tier / cold-tier auto-migration, optional S3
read-only adapter product layer.
