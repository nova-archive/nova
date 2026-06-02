# Roadmap

## Phase 0 v3 — Specifications (complete)

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
`pkg/coordinator` as a public Go library package (with the
`pkg/coordinator/product` subpackage at v0.x.y until external adapter
authors are real consumers in Phase 4).

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

v3.1 amendment:
- `pkg/node` is **not exported in Phase 1**. The donor binary ships
  in Phase 2; freezing a public Go interface before any caller
  exercises it would produce immediate semver churn at Phase 2.
  Phase 1 keeps node-side types under `internal/node/` and promotes
  to `pkg/node` in Phase 2 alongside `cmd/node`. See
  `docs/REVIEW_2026_05_25.md`.
- The envelope codec ships v1 (single-shot XChaCha20-Poly1305), but
  the implementation uses a versioned `Codec` interface so v2
  (streaming-AEAD, see Phase 2) drops in without disturbing v1
  paths. Blob metadata exposes `envelope_version` so consumers can
  branch.

### Phase 1 — Progress (current)

Each milestone is a tagged annotated commit; the canonical
walking-skeleton breakdown lives in
`docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md`
§ "Walking-skeleton milestone breakdown".

- [x] **M1 foundation** (`m1-foundation`) — repo bones, schema +
  migrations (DATA_MODEL.sql v2 → `internal/db/migrations/0001..0005`),
  embedded Kubo skeleton, config loader, Makefile + CI.
- [x] **M2 envelope + IPFS** (`m2-envelope-ipfs`) — XChaCha20-Poly1305
  v1 codec, deterministic IPFS import per `IPFS_IMPORT_RULES.md`,
  master-key wrap/unwrap, job queue.
- [x] **M3 storage + read API** (`m3-storage-read-api`) — `Resolve`
  + `OpenBytes`, `/blob/{cid}` GET/HEAD + `.json`, in-process
  rate-limit middleware, `/health`.
- [x] **M4 upload pipeline** (`m4-upload-pipeline`) — tus + multipart,
  the `AnalyzeUpload`/`OnCommitted` product seam, encryption-at-rest
  path, `data_encryption_keys` lifecycle, T1.20 public-uploads floor.
- [x] **M5 image transforms** (`m5-image-transforms`) — `nova-image`
  `Product` impl, `govips` wrapper with megapixel + concurrency
  bounds, `/i/*` single-flight serve, derivative pre-warm, PDQ
  pass-through scanner.
- [x] **M6 auth** (`m6-auth`) — argon2id passwords + timing equalizer,
  EdDSA local issuer + JWKS, rotating refresh tokens with reuse
  detection, external-OIDC verify-only adapter with resilient
  discovery, bearer middleware, per-IP login limiter, T1.19 +
  signing-key floors, `novactl auth login|whoami|logout`.
- [x] **M6.1 keystore hardening** (`m6.1-keystore-hardening`) —
  env → `_FILE` → `/run/secrets/master-key-<label>` resolver chain
  with ACTIVE/FILE pseudo-label filtering; `THREAT_MODEL.md`
  boundary ③ amended.
- [x] **M6.2 audit remediation** (`m6.2-audit-remediation`) —
  spec-drift reconciliation across persistent docs; verified
  security hardening (rate-limiter LRU + sweep, trusted-proxy XFF
  enforcement, login-failure log unification, refresh-family
  revocation correctness, master-key source logging, ctx-aware
  Unwrap, multipart `LimitReader`); refresh-token GC partial-index
  alignment; `/readyz` with DB + Kubo + OIDC checks; structured
  coordinator startup log. See `docs/REVIEW_2026_05_31.md`.

### Phase 1 — Deferred / Future-milestone slots

These deliverables remain from the Phase 1 v3.1 commitment and are
assigned to the slots already specified in
`docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md`
§ "Walking-skeleton milestone breakdown". Implementation lands in
the named milestone; no work is in scope for M6.2 beyond naming
the slots here.

| Slot | Deliverable |
|---|---|
| **M7** ✅ | Signed-URL HMAC verifier (`internal/auth/signedurl`) gating `/blob` + `/i/*`; `signing_keys` rotation via `/api/v1/admin/keys/rotate-signing` with grace window; structured `(kind, value)` revocation via `/api/v1/admin/signed-urls/revoke`; server-side minting via `/api/v1/admin/signed-urls/sign` + `novactl signed-url sign`. Implemented (tag `m7-signed-urls`). |
| **M8** ✅ | In-process integrity-audit scheduler (`internal/audit/integrity`) running the seven audit kinds on per-kind cadences (no `jobs.Queue`; resumes from natural cadence on restart); `/api/v1/admin/audits/integrity` paginated listing; failure surfacing via warn logs + `integrity_audits` rows + a `FailureSink` seam (`nova_integrity_audit_failures_total` metric and `integrity.audit_failed` webhook deferred); monthly-partition create-ahead + retention pruning. Implemented (tag `m8-integrity-audit`). Design: `docs/superpowers/specs/2026-06-02-phase1-m8-integrity-audit-scheduler-design.md`. |
| **M9** | DMCA quarantine + scheduled tombstone job + counter-notice; severe-content manual quarantine with `--legal-hold`; `novactl moderation quarantine`; operator-curated blocklist; `/api/v1/admin/moderation/*`. |
| **M10** | Master-key rotation (`novactl keys rotate-master`, `/api/v1/admin/keys/rotate-master`); parallel re-wrap worker; reads work against either MK version during rotation. |
| **M11** | Admin SPA: hermetic React + Vite build; login (PKCE-style); blob list/view/soft-delete; moderation queue + DMCA cases; integrity-audit failures view; key-rotation UI; jobs view. (`web/admin/`) |
| **M12** | Drag-and-drop widget: Uppy + tus.io; embeds in any HTML page; bearer-token auth; hermetic build, no external CDN. (`web/widget/`) |
| **M13** | Setup wizard (web UI + headless `novactl setup`); `entrypoint.sh` `.bootstrap-complete` sentinel; templated nginx config with two-vhost split per `THREAT_MODEL.md` boundary ①; TLS modes + certbot integration (prod profile). (`cmd/setup-wizard/`, `web/setup/`, `internal/setup/`, `nginx/`) |
| **M14** | Polish + end-to-end smoke test in CI; `docs/quickstart.md` operator guide; Phase 1 release-candidate tag. |

## Phase 2 — Federation + streaming-AEAD envelope

Split coordinator from pinning-node binary. Mesh-VPN-authenticated
federation, replication-factor enforcement, donor-operated nodes.
Streaming-AEAD envelope (v2 wire format) so encrypted blobs support
HTTP Range requests, CDN partial-object caching, and modern web
media playback expectations.

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
- `pkg/node` graduates to a public, semver-stable Go library
  alongside `cmd/node`.

v3.1 promotions into Phase 2:
- Streaming-AEAD envelope (v2 wire format). Chunk size aligned to
  the existing 256 KiB IPFS block boundary so chunk N == block N;
  per-chunk XChaCha20-Poly1305 with chunk-counter-derived nonces;
  AAD binds `chunk_index || total_chunks || cid` to defeat
  reordering and substitution. Encrypted blobs become Range-
  serveable. See `docs/specs/ENCRYPTION_ENVELOPE.md` § "Planned v2:
  Streaming-AEAD".
  This was previously listed as Phase 6+ research; pulled forward
  because single-shot AEAD restricts `nova-video`, `nova-audio`,
  large `nova-archive` objects, and modern web media patterns to
  full-object fetch. Federation is the right pairing because the
  per-block crypto semantics share infrastructure with possession
  audits and donor-to-donor repair.

## Phase 3 — Dedup and moderation

Go-native 256-bit perceptual hash (pHash, goimagehash
`ExtPerceptionHash`) index and BK-tree for near-duplicate detection
and dedup. Content-moderation pipeline scaffolding.

## Phase 4 — Adapters, SDKs, and severe-content workflow

Adapter packages for fediverse and forum software (separate
repositories). Auto-generated client SDKs in TypeScript, Python,
Swift.

v2 addition: full severe-content workflow per
`SEVERE_CONTENT_PROCEDURE.md`:
- PDQ hash computation and scan against the StopNCII/NCMEC external
  blocklist at upload (synchronous reject for clear matches,
  quarantine + legal-hold for ambiguous). Note: PDQ is distinct from
  the Phase-3 Go-native pHash — PDQ is for external blocklist
  matching only, not dedup.
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
during primary failover, formal Provable Data Possession / Proof of
Retrievability, hot-tier / cold-tier auto-migration, optional S3
read-only adapter product layer.

(v3.1: streaming-AEAD envelope was promoted from Phase 6+ research
to a Phase 2 deliverable. See above.)
