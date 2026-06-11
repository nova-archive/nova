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

## Phase 1 — Single-node MVP (complete at `v0.1.0-rc1`)

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

### Phase 1 — Progress (complete)

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
| **M9** ✅ | DMCA quarantine + ≈1-minute in-process tombstone sweep + counter-notice; severe-content manual quarantine with `--legal-hold` + operator-only `clear-legal-hold` (enforced by `no_shred_under_legal_hold` CHECK); `novactl moderation quarantine/takedown/restore/clear-legal-hold/list`; operator-curated CID blocklist; `/api/v1/admin/moderation/*` + `/api/v1/admin/audit-log`; public `POST /legal/dmca` intake; M7 audit backfill; `audit_log` partition create-ahead. Implemented (tag `m9-moderation`). Design: `docs/superpowers/specs/2026-06-02-phase1-m9-moderation-design.md`. Plan: `docs/superpowers/plans/2026-06-02-phase1-m9-moderation.md`. Deferrals: perceptual/visual blocklist → Phase 3; NCMEC CyberTipline + legal-hold-clear admin SPA → Phase 4; repeat-infringer auto-suspension → later (no account-state column); Kubo-pinset/DB orphan reconciliation → Phase-5 hardening. |
| **M10** ✅ | Master-key rotation (`novactl keys rotate-master --from v1 --to v2`, `GET /api/v1/admin/keys/rotation-status`); parallel re-wrap worker (default 4 goroutines, 256-row batches, 50 ms pace); one atomic version-guarded UPDATE per DEK (no per-row `rotating` state; `rotating` marks the source `master_key_versions` row); signing keys re-wrapped (`state IN ('active','retired')`); stalled-rotation `/readyz` degradation + `novactl keys status`; `ResumeIfRotating` crash recovery; audit `master_key.rotation_started/completed/resumed`. Implemented (tag `m10-master-key-rotation`). Design: `docs/superpowers/specs/2026-06-03-phase1-m10-master-key-rotation-design.md`. Plan: `docs/superpowers/plans/2026-06-03-phase1-m10-master-key-rotation.md`. Deferrals: runtime/no-restart activation → not planned; master-key generator helper (`novactl keys gen-master`) → later; cross-node rotation propagation → Phase 2; `novactl keys rotate-signing` wrapper → optional. |
| **M11** ✅ | Admin SPA (`web/admin/`): hermetic React + Vite (self-hosted IBM Plex latin, no CDN; CI `hermetic-spa` gate on the bundle); two auth drivers behind one provider — local-issuer password→token with silent refresh, and external-OIDC authorization-code + PKCE (issuer added to the SPA CSP connect-src); operator screens for blob list/view/soft-delete, moderation queue + DMCA + blocklist, integrity-audit failures, key rotation (master + signing), read-only jobs view, audit log. Backend slice: a neutral `internal/lifecycle.TombstoneTree` primitive (extracted from M9 — crypto-shred lives in one place) + owner soft-delete + in-process grace sweep (`blob.soft_deleted`/`blob.tombstoned` audit, distinct from `dmca.*`); mounted `GET`/`DELETE /api/v1/blobs/{cid}` (the M6-deferred owner routes); `GET /api/v1/admin/blobs` + read-only `GET /api/v1/admin/jobs`; coordinator-served `/admin/*` static (strict CSP + SPA fallback) gated by `NOVA_ADMIN_DIST_DIR`; migration `0009` (`blobs.soft_deleted_at`). Implemented (tag `m11-admin-spa`). Design: `docs/superpowers/specs/2026-06-04-phase1-m11-admin-spa-design.md`. Plan: `docs/superpowers/plans/2026-06-04-phase1-m11-admin-spa.md`. Deferrals: jobs retry → fast-follow; blob PATCH / `/api/v1/images/{cid}` / collections / perceptual search → later; clear-legal-hold UI → Phase 4; upload widget → M12; production nginx two-vhost split + Docker → M13. |
| **M12** ✅ | Drag-and-drop upload widget (`web/widget/`): hermetic Vite **library-mode** IIFE bundle exposing the global `NovaUploadWidget` (single-`<script>` embed, stable entry filename, CSS injected at runtime); `@uppy/core`+`drag-drop`+`tus`+`status-bar` (3.x; the maintained `@uppy/status-bar`, **not** the deprecated `@uppy/progress-bar`); the Nova-aware finalize orchestrator (tus `upload-success` is transport-only → `POST .../finalize` → `UploadResult`); `getToken()` resolved **per request** (survives the M6 15-min access TTL; `null` ⇒ public-uploads floor); `mount`/`mountAll` + a `data-nova-upload-widget` auto-bootstrap with a `WeakMap` double-mount guard. Backend slice: a feature-gated coordinator `/widget/*` static seam (`internal/api/handlers/widget_static.go`, strict CSP, no SPA fallback) gated by `NOVA_WIDGET_DIST_DIR`; `web/widget` re-added to the root workspaces; a `hermetic-widget` CI gate that greps both the HTML/CSS and the inlined JS bundle for external-origin patterns. Implemented (tag `m12-upload-widget`). Design: `docs/superpowers/specs/2026-06-07-phase1-m12-upload-widget-design.md`. Plan: `docs/superpowers/plans/2026-06-07-phase1-m12-upload-widget.md`. Deferrals: cross-origin embedding + first-class CORS → operator nginx / later milestone; production nginx two-vhost split + Docker → M13; rich Uppy Dashboard / hosted upload app → later; tus-result preset URLs → later backend change. |
| **M13** ✅ | First-run setup wizard + Docker production. Shared UI-agnostic core (`internal/setup/`: answers + per-step validation reusing the `config.validate` floors, CSPRNG key material, `operator.yaml`/`nova.conf` render, per-mode TLS, atomic sentinel-last commit) drives both a hermetic React+Vite web wizard (`web/setup/`; `hermetic-spa` gate) and a headless `novactl setup --interactive | --config-file`. Setup mode is **folded into the coordinator boot path** (`coordinator.RunSetupServer`, sentinel-gated in `cmd/coordinator`) — a reduced boot mounting only the loopback-bound `/setup/*` seam (`internal/api/handlers/setup.go`) until `.bootstrap-complete` is written; `cmd/setup-wizard` is a thin alias. `operator.yaml` is now wired into `cmd/coordinator` as the canonical non-secret config source, with the existing `NOVA_*` env reads preserved as overrides. The two-vhost split is **nginx-only** (templated `nova.conf` from `internal/setup/templates/nova.conf.tmpl`: public_host serves `/blob`·`/i`·`/legal`·`/health`·`/api/v1/uploads\|blobs\|images`·`/widget`·ACL'd `/metrics`, `/fed`→404, default→404; admin_host serves `/admin`·`/api/v1/admin`·`/api/v1/auth`·`/api/v1/users/me`·`/health`, `/fed`→404, default→404); the coordinator keeps its single mux. TLS modes: `dev-self-signed` (auto CA+leaf), `static` (operator PEM), `http-01` (certbot, prod profile, best-effort renewal scaffold — initial issuance is operator-handoff); `dns-01`/`onion` render config + print operator-handoff instructions. Docker: multi-stage Debian-slim/glibc image (non-root via `gosu` drop in `docker/init/entrypoint.sh`), `docker/docker-compose.yml` with `setup` + `prod` profiles; published ports 8442:80, 8443:443, 127.0.0.1:8445:8445, wizard on 127.0.0.1:8444; secrets (master-key-v1, swarm.key, oidc-signing-key) generated by the wizard into the `nova-secrets` volume. The web wizard configures the **local issuer** (default); **external-OIDC is configured via the headless `novactl setup --config-file` / manual `operator.yaml`** path (`auth_mode: external` + `issuer_url`/`client_id`), not the web stepper. Integration test proves the two-vhost split + the setup→normal sentinel flip. Implemented (tag `m13-setup-wizard`). Design: `docs/superpowers/specs/2026-06-08-phase1-m13-setup-wizard-design.md`. Plan: `docs/superpowers/plans/2026-06-08-phase1-m13-setup-wizard.md`. Deferrals: exhaustive container hardening + release signing + CI e2e smoke + screenshot quickstart → M14 / Phase 5; full `dns-01`/`onion` automation → later; certbot full deploy-hook/reload + initial ACME issuance → M14; `operator.yaml` decode of the M7–M12 tuning knobs → later (those stay env-only); in-process uid-0 floor → later (non-root is enforced via the container today); web-wizard external-OIDC → the headless/manual path. |
| **M14** ✅ | Polish, security housecleaning, CI e2e smoke, release candidate. CI repairs: golangci-lint migrated to v2 (v2 config + `golangci-lint-action@v8` + Go version derived from `go.mod` via `go-version-file`); the dead schema-drift diff replaced by a migration-immutability check (`internal/db/migrations/MANIFEST.sha256` + `scripts/check-migrations-frozen.sh`, blocking CI job `migrations-frozen`; the `0001_init.sql` header corrected — `DATA_MODEL.sql` is the annotated living reference, the migrations are authoritative). Dependabot triage: all 25 alerts / 10 advisories assessed, **none enabling compromise of a production deployment** — the single runtime-reachable item (quic-go) is a memory-exhaustion DoS (full triage table in the design doc); the two runtime-reachable patches landed — quic-go v0.59.1 (CVE-2026-40898, DoS via the embedded Kubo QUIC stack) and otlptracehttp v1.43.0 (CVE-2026-39882) — and every npm advisory (all dev-toolchain-only) cleared by the toolchain jump: Vite 8.0.16 + Vitest 4.1.8 + plugin-react 6 + jsdom 29 across all three SPAs (the Node-16-era pins are gone), Node 22 in `.nvmrc`/engines/CI/`docker/Dockerfile`, a root npm `overrides` pinning @uppy/core's transitive nanoid ≥5.1.6. Ongoing currency: `.github/dependabot.yml` (gomod/npm/github-actions, weekly, grouped) + the CONTRIBUTING.md "Toolchain currency" policy. Full-stack e2e smoke (`scripts/smoke.sh`, wired as a blocking CI `smoke` job): image build → headless `novactl setup --config-file` → prod profile boot → anonymous upload → byte-identical `/blob` read → `/i/…/w320.png` transform → operator login + DELETE → 404/410. The M13 certbot deferral closed: http-01 **initial issuance is automated** (`docker/certbot/certbot-loop.sh` issues on first boot and deploys key-first/cert-atomic into `/etc/nova/tls`; a self-signed placeholder breaks the nginx⇄certbot bootstrap deadlock) and renewals hot-reload nginx (`docker/nginx/cert-watch.sh` watches the cert hash and SIGHUPs nginx); the new `nova-letsencrypt` volume persists the ACME account/lineage; `dns-01`/`onion` stay operator-handoff. Container hardening floors: healthchecks on all five compose services; read-only rootfs + tmpfs on coordinator/nginx/nginx-setup (postgres pre-existing; certbot exempted with comment); `no-new-privileges` + `cap_drop: [ALL]` + minimal commented `cap_add`s everywhere. `docs/quickstart.md` operator quickstart (screenshot capture is a pending human action — file list in `docs/images/quickstart/README.md`). Implemented (tags `m14-polish-release` + `v0.1.0-rc1` — **Phase 1 complete at release candidate**). Design: `docs/superpowers/specs/2026-06-09-phase1-m14-polish-release-design.md`. Plan: `docs/superpowers/plans/2026-06-09-phase1-m14-polish-release.md`. Deferrals: release signing (sigstore/cosign + `release.yml`) → Phase 5 (the master plan's original position; the M13-spec line assigning it to M14 was in error and is corrected); seccomp/AppArmor profiles + dropping nginx's `DAC_READ_SEARCH` via entrypoint group-perm rework → Phase 5; per-service log shipping + chaos testing → Phase 5. |

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
