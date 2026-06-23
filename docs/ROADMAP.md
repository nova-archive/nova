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
`docs/superpowers/specs/phase1/2026-05-25-phase1-single-node-mvp-design.md`
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
`docs/superpowers/specs/phase1/2026-05-25-phase1-single-node-mvp-design.md`
§ "Walking-skeleton milestone breakdown". Implementation lands in
the named milestone; no work is in scope for M6.2 beyond naming
the slots here.

| Slot | Deliverable |
|---|---|
| **M7** ✅ | Signed-URL HMAC verifier (`internal/auth/signedurl`) gating `/blob` + `/i/*`; `signing_keys` rotation via `/api/v1/admin/keys/rotate-signing` with grace window; structured `(kind, value)` revocation via `/api/v1/admin/signed-urls/revoke`; server-side minting via `/api/v1/admin/signed-urls/sign` + `novactl signed-url sign`. Implemented (tag `m7-signed-urls`). |
| **M8** ✅ | In-process integrity-audit scheduler (`internal/audit/integrity`) running the seven audit kinds on per-kind cadences (no `jobs.Queue`; resumes from natural cadence on restart); `/api/v1/admin/audits/integrity` paginated listing; failure surfacing via warn logs + `integrity_audits` rows + a `FailureSink` seam (`nova_integrity_audit_failures_total` metric and `integrity.audit_failed` webhook deferred); monthly-partition create-ahead + retention pruning. Implemented (tag `m8-integrity-audit`). Design: `docs/superpowers/specs/phase1/2026-06-02-phase1-m8-integrity-audit-scheduler-design.md`. |
| **M9** ✅ | DMCA quarantine + ≈1-minute in-process tombstone sweep + counter-notice; severe-content manual quarantine with `--legal-hold` + operator-only `clear-legal-hold` (enforced by `no_shred_under_legal_hold` CHECK); `novactl moderation quarantine/takedown/restore/clear-legal-hold/list`; operator-curated CID blocklist; `/api/v1/admin/moderation/*` + `/api/v1/admin/audit-log`; public `POST /legal/dmca` intake; M7 audit backfill; `audit_log` partition create-ahead. Implemented (tag `m9-moderation`). Design: `docs/superpowers/specs/phase1/2026-06-02-phase1-m9-moderation-design.md`. Plan: `docs/superpowers/plans/phase1/2026-06-02-phase1-m9-moderation.md`. Deferrals: perceptual/visual blocklist → Phase 3; NCMEC CyberTipline + legal-hold-clear admin SPA → Phase 4; repeat-infringer auto-suspension → later (no account-state column); Kubo-pinset/DB orphan reconciliation → Phase-5 hardening. |
| **M10** ✅ | Master-key rotation (`novactl keys rotate-master --from v1 --to v2`, `GET /api/v1/admin/keys/rotation-status`); parallel re-wrap worker (default 4 goroutines, 256-row batches, 50 ms pace); one atomic version-guarded UPDATE per DEK (no per-row `rotating` state; `rotating` marks the source `master_key_versions` row); signing keys re-wrapped (`state IN ('active','retired')`); stalled-rotation `/readyz` degradation + `novactl keys status`; `ResumeIfRotating` crash recovery; audit `master_key.rotation_started/completed/resumed`. Implemented (tag `m10-master-key-rotation`). Design: `docs/superpowers/specs/phase1/2026-06-03-phase1-m10-master-key-rotation-design.md`. Plan: `docs/superpowers/plans/phase1/2026-06-03-phase1-m10-master-key-rotation.md`. Deferrals: runtime/no-restart activation → not planned; master-key generator helper (`novactl keys gen-master`) → later; cross-node rotation propagation → Phase 2; `novactl keys rotate-signing` wrapper → optional. |
| **M11** ✅ | Admin SPA (`web/admin/`): hermetic React + Vite (self-hosted IBM Plex latin, no CDN; CI `hermetic-spa` gate on the bundle); two auth drivers behind one provider — local-issuer password→token with silent refresh, and external-OIDC authorization-code + PKCE (issuer added to the SPA CSP connect-src); operator screens for blob list/view/soft-delete, moderation queue + DMCA + blocklist, integrity-audit failures, key rotation (master + signing), read-only jobs view, audit log. Backend slice: a neutral `internal/lifecycle.TombstoneTree` primitive (extracted from M9 — crypto-shred lives in one place) + owner soft-delete + in-process grace sweep (`blob.soft_deleted`/`blob.tombstoned` audit, distinct from `dmca.*`); mounted `GET`/`DELETE /api/v1/blobs/{cid}` (the M6-deferred owner routes); `GET /api/v1/admin/blobs` + read-only `GET /api/v1/admin/jobs`; coordinator-served `/admin/*` static (strict CSP + SPA fallback) gated by `NOVA_ADMIN_DIST_DIR`; migration `0009` (`blobs.soft_deleted_at`). Implemented (tag `m11-admin-spa`). Design: `docs/superpowers/specs/phase1/2026-06-04-phase1-m11-admin-spa-design.md`. Plan: `docs/superpowers/plans/phase1/2026-06-04-phase1-m11-admin-spa.md`. Deferrals: jobs retry → fast-follow; blob PATCH / `/api/v1/images/{cid}` / collections / perceptual search → later; clear-legal-hold UI → Phase 4; upload widget → M12; production nginx two-vhost split + Docker → M13. |
| **M12** ✅ | Drag-and-drop upload widget (`web/widget/`): hermetic Vite **library-mode** IIFE bundle exposing the global `NovaUploadWidget` (single-`<script>` embed, stable entry filename, CSS injected at runtime); `@uppy/core`+`drag-drop`+`tus`+`status-bar` (3.x; the maintained `@uppy/status-bar`, **not** the deprecated `@uppy/progress-bar`); the Nova-aware finalize orchestrator (tus `upload-success` is transport-only → `POST .../finalize` → `UploadResult`); `getToken()` resolved **per request** (survives the M6 15-min access TTL; `null` ⇒ public-uploads floor); `mount`/`mountAll` + a `data-nova-upload-widget` auto-bootstrap with a `WeakMap` double-mount guard. Backend slice: a feature-gated coordinator `/widget/*` static seam (`internal/api/handlers/widget_static.go`, strict CSP, no SPA fallback) gated by `NOVA_WIDGET_DIST_DIR`; `web/widget` re-added to the root workspaces; a `hermetic-widget` CI gate that greps both the HTML/CSS and the inlined JS bundle for external-origin patterns. Implemented (tag `m12-upload-widget`). Design: `docs/superpowers/specs/phase1/2026-06-07-phase1-m12-upload-widget-design.md`. Plan: `docs/superpowers/plans/phase1/2026-06-07-phase1-m12-upload-widget.md`. Deferrals: cross-origin embedding + first-class CORS → operator nginx / later milestone; production nginx two-vhost split + Docker → M13; rich Uppy Dashboard / hosted upload app → later; tus-result preset URLs → later backend change. |
| **M13** ✅ | First-run setup wizard + Docker production. Shared UI-agnostic core (`internal/setup/`: answers + per-step validation reusing the `config.validate` floors, CSPRNG key material, `operator.yaml`/`nova.conf` render, per-mode TLS, atomic sentinel-last commit) drives both a hermetic React+Vite web wizard (`web/setup/`; `hermetic-spa` gate) and a headless `novactl setup --interactive | --config-file`. Setup mode is **folded into the coordinator boot path** (`coordinator.RunSetupServer`, sentinel-gated in `cmd/coordinator`) — a reduced boot mounting only the loopback-bound `/setup/*` seam (`internal/api/handlers/setup.go`) until `.bootstrap-complete` is written; `cmd/setup-wizard` is a thin alias. `operator.yaml` is now wired into `cmd/coordinator` as the canonical non-secret config source, with the existing `NOVA_*` env reads preserved as overrides. The two-vhost split is **nginx-only** (templated `nova.conf` from `internal/setup/templates/nova.conf.tmpl`: public_host serves `/blob`·`/i`·`/legal`·`/health`·`/api/v1/uploads\|blobs\|images`·`/widget`·ACL'd `/metrics`, `/fed`→404, default→404; admin_host serves `/admin`·`/api/v1/admin`·`/api/v1/auth`·`/api/v1/users/me`·`/health`, `/fed`→404, default→404); the coordinator keeps its single mux. TLS modes: `dev-self-signed` (auto CA+leaf), `static` (operator PEM), `http-01` (certbot, prod profile, best-effort renewal scaffold — initial issuance is operator-handoff); `dns-01`/`onion` render config + print operator-handoff instructions. Docker: multi-stage Debian-slim/glibc image (non-root via `gosu` drop in `docker/init/entrypoint.sh`), `docker/docker-compose.yml` with `setup` + `prod` profiles; published ports 8442:80, 8443:443, 127.0.0.1:8445:8445, wizard on 127.0.0.1:8444; secrets (master-key-v1, swarm.key, oidc-signing-key) generated by the wizard into the `nova-secrets` volume. The web wizard configures the **local issuer** (default); **external-OIDC is configured via the headless `novactl setup --config-file` / manual `operator.yaml`** path (`auth_mode: external` + `issuer_url`/`client_id`), not the web stepper. Integration test proves the two-vhost split + the setup→normal sentinel flip. Implemented (tag `m13-setup-wizard`). Design: `docs/superpowers/specs/phase1/2026-06-08-phase1-m13-setup-wizard-design.md`. Plan: `docs/superpowers/plans/phase1/2026-06-08-phase1-m13-setup-wizard.md`. Deferrals: exhaustive container hardening + release signing + CI e2e smoke + screenshot quickstart → M14 / Phase 5; full `dns-01`/`onion` automation → later; certbot full deploy-hook/reload + initial ACME issuance → M14; `operator.yaml` decode of the M7–M12 tuning knobs → later (those stay env-only); in-process uid-0 floor → later (non-root is enforced via the container today); web-wizard external-OIDC → the headless/manual path. |
| **M14** ✅ | Polish, security housecleaning, CI e2e smoke, release candidate. CI repairs: golangci-lint migrated to v2 (v2 config + `golangci-lint-action@v8` + Go version derived from `go.mod` via `go-version-file`); the dead schema-drift diff replaced by a migration-immutability check (`internal/db/migrations/MANIFEST.sha256` + `scripts/check-migrations-frozen.sh`, blocking CI job `migrations-frozen`; the `0001_init.sql` header corrected — `DATA_MODEL.sql` is the annotated living reference, the migrations are authoritative). Dependabot triage: all 25 alerts / 10 advisories assessed, **none enabling compromise of a production deployment** — the single runtime-reachable item (quic-go) is a memory-exhaustion DoS (full triage table in the design doc); the two runtime-reachable patches landed — quic-go v0.59.1 (CVE-2026-40898, DoS via the embedded Kubo QUIC stack) and otlptracehttp v1.43.0 (CVE-2026-39882) — and every npm advisory (all dev-toolchain-only) cleared by the toolchain jump: Vite 8.0.16 + Vitest 4.1.8 + plugin-react 6 + jsdom 29 across all three SPAs (the Node-16-era pins are gone), Node 22 in `.nvmrc`/engines/CI/`docker/Dockerfile`, a root npm `overrides` pinning @uppy/core's transitive nanoid ≥5.1.6. Ongoing currency: `.github/dependabot.yml` (gomod/npm/github-actions, weekly, grouped) + the CONTRIBUTING.md "Toolchain currency" policy. Full-stack e2e smoke (`scripts/smoke.sh`, wired as a blocking CI `smoke` job): image build → headless `novactl setup --config-file` → prod profile boot → anonymous upload → byte-identical `/blob` read → `/i/…/w320.png` transform → operator login + DELETE → 404/410. The M13 certbot deferral closed: http-01 **initial issuance is automated** (`docker/certbot/certbot-loop.sh` issues on first boot and deploys key-first/cert-atomic into `/etc/nova/tls`; a self-signed placeholder breaks the nginx⇄certbot bootstrap deadlock) and renewals hot-reload nginx (`docker/nginx/cert-watch.sh` watches the cert hash and SIGHUPs nginx); the new `nova-letsencrypt` volume persists the ACME account/lineage; `dns-01`/`onion` stay operator-handoff. Container hardening floors: healthchecks on all five compose services; read-only rootfs + tmpfs on coordinator/nginx/nginx-setup (postgres pre-existing; certbot exempted with comment); `no-new-privileges` + `cap_drop: [ALL]` + minimal commented `cap_add`s everywhere. `docs/quickstart.md` operator quickstart (screenshot capture is a pending human action — file list in `docs/images/quickstart/README.md`). Implemented (tags `m14-polish-release` + `v0.1.0-rc1` — **Phase 1 complete at release candidate**). Design: `docs/superpowers/specs/phase1/2026-06-09-phase1-m14-polish-release-design.md`. Plan: `docs/superpowers/plans/phase1/2026-06-09-phase1-m14-polish-release.md`. Deferrals: release signing (sigstore/cosign + `release.yml`) → Phase 5 (the master plan's original position; the M13-spec line assigning it to M14 was in error and is corrected); seccomp/AppArmor profiles + dropping nginx's `DAC_READ_SEARCH` via entrypoint group-perm rework → Phase 5; per-service log shipping + chaos testing → Phase 5. |

## Phase 2 — Federation + streaming-AEAD envelope

Split coordinator from pinning-node binary. Mesh-VPN-authenticated
federation, replication-factor enforcement, donor-operated nodes.
Streaming-AEAD envelope (v2 wire format) so encrypted blobs support
HTTP Range requests, CDN partial-object caching, and modern web
media playback expectations.

> **Storage/read architecture redirect (P2-M2.1, 2026-06-20).** Phase 2's
> storage model was redirected: the operator is **not** required to retain the
> full corpus. Donor-backed reads, origin pruning, and reputation-based
> best-link selection (VPS-primary / residential-fallback) are the Phase-2
> target — the donor replica set is the durable substrate and the operator keeps
> a bounded cache. Storage and read fan-out are **independent axes**: donors are
> donor-blind, so the operator stays the decrypt/serve point and fan-out to many
> DAU scales via the operator hot cache and/or optional CDN, not via donors
> serving users. This amends the federation design's earlier "replicate, don't
> migrate" stance; binding M3/M4/M5 constraints live in that design's
> "Storage/read architecture (P2-M2.1 amendment)" section.

### Phase 2 — P2-M0.x remediation track (operator-UX / privacy / pitfall fixes before additive Phase 2 work)

| Slot | Deliverable |
|---|---|
| **M0.1** ✅ | Correctness fixes (admin SPA nav double-prefix, compose `name:` pin, plumb `NOVA_PUBLIC_UPLOADS`/`NOVA_TOS_URL`). Implemented (tag `p2-m0.1-correctness-fixes`). |
| **M0.2** ✅ | `paranoid` reframed as a default-off warn-not-force preset over individually addressable constituents (`record_source_ip`, `source_ip_retention_days`, `public_ipfs_dht`, `webhooks`); startup warning replaces forced override when a protective default is relaxed. Implemented (tag `p2-m0.2-privacy-posture`). Design: `docs/superpowers/specs/phase2/2026-06-13-m0.2-privacy-posture-model-design.md`. |
| **M0.3** ✅ | CORS + upload-credential hardening: scoped revocable upload tokens (`nova_ut_…`), per-session concurrency/file-count limits, CORS allowlist on upload routes. Implemented (tag `p2-m0.3-offorigin-widget`). Design: `docs/superpowers/specs/phase2/2026-06-13-m0.3-offorigin-widget-design.md`. |
| **M0.4** ✅ | Runtime config backend: `operator.yaml` read/update admin API (`GET`/`PATCH`/`PUT /api/v1/admin/config`), live hot-reload for `live`-class fields, `novactl config get/set/apply`. Implemented (tag `p2-m0.4-config-backend`). Design: `docs/superpowers/specs/phase2/2026-06-14-m0.4-config-backend-design.md`. Plan: `docs/superpowers/plans/phase2/2026-06-14-m0.4-config-backend.md`. |
| **M0.5** ✅ | Setup-wizard redesign: consequence copy + learn-this/abstract-away jargon info-buttons + tri-state paranoid delineation + additive `Answers` constituents. Implemented (tag `p2-m0.5-wizard-redesign`). Design: `docs/superpowers/specs/phase2/2026-06-14-m0.5-setup-wizard-redesign-design.md`. Plan: `docs/superpowers/plans/phase2/2026-06-14-m0.5-setup-wizard-redesign.md`. |
| **M0.6** ✅ | Admin Settings screen (`web/admin`): operator-only `/settings` route driving the M0.4 config API — curated, explained controls over a typed draft (tri-state webhook-aware `ParanoidSection`; CORS enable + origin add/remove with `new URL().origin` normalization + enabled-with-empty-list guard; live upload limits; public-uploads/ToS with a T1.20 local guard), minimal JSON-Merge-Patch save with `If-Match` optimistic concurrency (200 reseed + `restart_required` banner / 409 conflict-reload / 422 inline), plus a collapsible read-only full-surface effective-config viewer with live/restart/env badges driven by the GET `fields` metadata. `auth.paranoid` is derived as the AND of the children **and** webhooks-empty, so no save trips an `ApplyPrivacyPreset` startup WARN (the runtime webhook case the M0.5 first-run never hit); the screen surfaces `privacy_warnings` and resolves editable-constituent drift on save. Ported M0.5's `InfoTerm`/`ConsequenceNote`/`ParanoidSection`/glossary into `web/admin` (no shared package); badges prefer live `fields` metadata with a registry fallback + no-drift test. No backend change. Implemented (tag `p2-m0.6-settings-screen`). Design: `docs/superpowers/specs/phase2/2026-06-15-m0.6-settings-screen-design.md`. Plan: `docs/superpowers/plans/phase2/2026-06-15-m0.6-settings-screen.md`. **All P2-M0.x remediation items complete.** |

### Phase 2 — Progress (additive federation track)

The main-track federation milestones (P2-M1 … P2-M10) are detailed in the master
design's milestone breakdown
(`docs/superpowers/specs/phase2/2026-06-11-phase2-federation-design.md`). P2-M0
(spec reconciliation) is merged (tag `p2-m0-spec-reconciliation`).

| Slot | Deliverable |
|---|---|
| **P2-M1** ✅ | Build / repo separation: a `nova-node` donor binary (`cmd/node`) whose dependency graph **provably excludes operator-only code**. Extracted stdlib-only `internal/secret` leaf (coordinator re-pointed, behavior-preserving); shared `internal/federation/wire` (fed/v1 messages + fail-closed capability negotiation + canonical Ed25519 repair-token claim/`Verify` — **no mint, no replay**, those are M4); donor-only `internal/node/{config,bandwidth,state,agent,transfer,audit}` — in M1 only the authoritative daily bandwidth token-bucket (D11) has real logic, the rest are interface seams (transport→M2, sync/state→M3, transfer/mint→M4, audit→M6); `nova-node --config/--validate/--healthcheck` with a fail-fast loopback health server. Load-bearing gate: `scripts/check_node_deps.sh` (`go list -deps ./cmd/node`, **deny-by-default over all non-stdlib deps**), wired as blocking CI `donor-deps-boundary` and demonstrated red against an injected operator import. Split Dockerfiles (`docker/Dockerfile`→`coordinator.Dockerfile` + a minimal **8.97 MB** distroless-static CGO-off `docker/node.Dockerfile`; `scripts/check_node_image.sh` forbidden-inventory scan); `deploy/donor/{compose.yaml,node.yaml.example}` (Nebula sidecar, no published ports, `read_only`/`cap_drop: ALL`/`no-new-privileges`); CI SBOM (`donor-build`) + **cosign keyless** signing + provenance attested to the **pushed digest** (`donor-sbom-sign`, trusted-ref-gated, never on PRs); `docs/quickstart/donor.md` release-trust stub. `node.yaml` references all secret material by `*_path` (shallow validation — no PEM parse). **No live federation; no schema/migration** (`migrations-frozen` stays green). Implemented (tag `p2-m1-build-repo-separation`). Design: `docs/superpowers/specs/phase2/2026-06-15-phase2-m1-build-repo-separation-design.md`. Plan: `docs/superpowers/plans/phase2/2026-06-15-phase2-m1-build-repo-separation.md`. Deferrals: live mTLS-over-Nebula transport + registration + capability handshake → M2; `pin_changes` log + assignment sync + snapshot recovery → M3; coordinator-as-source + streaming transfer + deterministic re-import + Ed25519 token mint + donor↔donor repair → M4; 5-state liveness + healing + `blob_replication_state` → M5; possession audits + reputation → M6; volunteer digest-pin/`cosign verify` walkthrough + revocation/provider-loss drills → M7. |
| **P2-M2** ✅ | Identity, registration, capability negotiation — the **first live federation milestone** (the M1 seams become real). A standalone `internal/federation/coordinator` mTLS server runs as a **second listener** (Nebula-interface-bound, `RequireAndVerifyClientCert` against the operator federation CA; never on the public/admin mux — verified `/fed/v1/*`→404 there) serving `POST /fed/v1/register` + `POST /fed/v1/heartbeat`. **Identity derives only from the verified leaf cert** — DER fingerprint (`sha256:<hex>`, not SPKI) + a stable `node_id` UUID from the `nova://node/<uuid>` URI SAN; self-asserted JSON is never trusted as identity (the request `federation_cert_fingerprint` is cross-checked, `nebula_cert_fingerprint` is non-identity metadata). **Fail-closed** protocol + capability negotiation (no overlap → `400 incompatible_protocol`/`missing_capability`, never a refused 201; M2 advertises/requires an **honest empty** capability set — the machinery ships, the future cap ids stay unadvertised until M3/M4/M6). Handler-level authorization: revoked → `403 node_revoked`, presented-fp ≠ stored-fp → `403 fingerprint_mismatch` (rotation cutover / stale cert), heartbeat from an unknown node → `403 registration_required`. New rows land `trust_state='probationary'`, `status='active'`; heartbeat records `last_seen_at`/`last_free_bytes`/`last_stored_bytes` and returns `config_updates` timers + `current_epoch:0` + **empty** `repair_token_public_key` (the channel exists; the signer is M4). Shared stdlib-only `internal/federation/transport` (mTLS `tls.Config` builders + cert→identity); pure-Go Ed25519 `internal/federation/ca` (federation X.509 CA + coordinator server cert + donor client certs). `novactl node ca-init/issue/nebula-template` (local file ops; explicit two-trust-root naming `federation-*` vs `nebula-*`, no `nebula-cert` shell-out, no Nebula Go dep) + `revoke/rotate-cert/list` (DB-direct via `DATABASE_URL`, like `novactl setup`). Donor: the no-op agent becomes a real register→heartbeat loop (`internal/node/agent`) over an mTLS `HTTPClient`, with an **atomic-JSON** `RegistrationStore` (`internal/node/state`; temp→fsync→rename→dir-fsync, 0600). `cmd/coordinator` runs both listeners with a `runBoth` loop that **binds the federation listener before declaring startup success** and tears down as a unit on any exit. Operator `federation` config block (`listen_addr`/`nebula_interface` boot guard/`federation_{ca,cert,key}_path` + timers). `nodes`-scoped migration **0011** (`trust_state` text+CHECK, `selected_protocol`, `advertised`/`required_capabilities`, `client_version`, `cert_revoked_at`/`cert_rotation_started_at`/`cert_rotated_at`, `last_free_bytes`/`last_stored_bytes`). Donor dependency boundary extended by exactly one reviewed leaf (`internal/federation/transport`; gate stays green, demonstrated red against an injected `internal/db`). e2e loopback-mTLS integration test (register→heartbeat→`probationary`/`active`→revoke-blocks), public-mux 404 test, and rotation-cutover test (old→403/new→200/`node_id` unchanged). Implemented (tag `p2-m2-identity-registration`). Design: `docs/superpowers/specs/phase2/2026-06-16-phase2-m2-identity-registration-design.md`. Plan: `docs/superpowers/plans/phase2/2026-06-16-phase2-m2-identity-registration.md`. Decisions: `nodes`-scoped migration **amends** the P2-M0/M1 "all Phase-2 DDL in M3" note (schema lands when a milestone first needs durable truth); repair-token signer + `current_epoch` semantics → M4; **downtime** cert cutover (no zero-downtime overlap). Deferrals: `pin_changes` log + assignment/snapshot sync + node-local cursor → M3; coordinator-as-source + streaming transfer + deterministic re-import + Ed25519 token mint + donor↔donor repair → M4; 5-state liveness **sweeper** + healing + `blob_replication_state` + D8 failure-domain/`placement_weight` + `federation.node_revoked` webhook (no dispatcher exists yet) → M5; possession audits + reputation + trust **graduation** → M6; volunteer release docs/digest-pin drills → M7. |
| **P2-M2.1** ✅ | Reconciliation, supply-chain hardening, and **storage/read architecture redirect** — a between-milestone increment, **no normative protocol code or migration** (`migrations-frozen` stays green). **Docs reconciliation:** README (the `What is not yet wired` block listed shipped M7–M14 as deferred; `novactl user create` → setup-wizard account creation; status → Phase 2 in progress), CONTRIBUTING (`Phase 0` → active development), SECURITY (`Phase 0 / no releases / threat-model-planned` → current posture + threat model exists) — aspirational "durable federated storage" language deliberately kept. **Versioning:** `docs/VERSIONING.md` semver-per-milestone policy (no two builds share a version); `git describe --tags --always --dirty` build stamping wired via `make` (`VERSION`/`GO_LDFLAGS`) into a `main.buildVersion` fallback, `NOVA_VERSION` env still overrides. **Supply-chain CI:** `govulncheck` (reachability, complements the manual Dependabot triage), CodeQL (Go + JS/TS), OpenSSF Scorecard, and coordinator-image SBOM + cosign keyless signing + provenance (parity with the donor image). **Durability default:** `important` replication factor default **R=3 → R=5** (warn-not-force, tunable down to 3; lower R ⇒ higher permanent-loss risk, higher R ⇒ higher donor storage burden) — loader default applied **before** validation via shared `DefaultReplication*` constants so wizard-render and loader can't drift, the `HEALING_PROTOCOL` R=3/R=5 contradiction resolved, warn-not-force **emission** specified for P2-M5 where the orchestrator consumes R. **Storage/read architecture redirect (the substantive change):** the operator is no longer required to retain the full corpus — donor-backed reads, origin pruning, and reputation-based best-link selection (VPS-primary / residential-fallback) become the Phase-2 storage target; the donor replica set is the durable substrate, the operator keeps a bounded cache, and storage vs. read-fan-out are independent axes (donor-blind ⇒ operator stays the decrypt/serve point; fan-out via cache/optional CDN). Amends the federation design's "replicate, don't migrate" stance and moves the former out-of-scope bullet into scope; binding M3/M4/M5 constraints + a target config surface are recorded in the federation design's new "Storage/read architecture (P2-M2.1 amendment)" section (normative-spec edits deferred to each owning milestone, P2-M0 style). **Widget→public-URL loop (step 3):** closed the gap where anonymous/default uploads landed in no collection → private → the widget's returned `/blob/{cid}` 401'd anonymously, with no collection-create path outside raw SQL. Adds `novactl collection create` (DB-direct; owner = sole operator or `--owner`) + a live-reloadable `uploads.default_collection_id` so uploads with no explicit/token-bound collection auto-join a configured collection — point it at a public one to make anonymous widget uploads publicly viewable without per-upload wiring; the quickstart's raw-SQL seed is replaced. TDD: handler default-apply (tus+multipart, explicit-wins), config UUID validation, and DB-direct create + owner-resolution. Implemented (tag `p2-m2.1-reconciliation-storage-redirect`). Design: `docs/superpowers/specs/phase2/2026-06-11-phase2-federation-design.md` § "Storage/read architecture (P2-M2.1 amendment)". |
| **P2-M3** ✅ | Assignment synchronization — the donor's durable, recoverable view of what it should hold, with **no byte transfer and no donor `ack`** (those are M4). Migration **0012** versions `pin_assignments` (immutable `assignment_id` handle + `generation`; `(cid,node_id)` PK kept) and adds the durable `pin_changes` change-log (`sequence bigserial`, `(node_id,sequence)` index, `byte_size`) + a singleton `federation_change_log_state` retention watermark. Coordinator (operator-side `internal/federation/coordinator`, the M2 second mTLS listener) serves `GET /fed/v1/pins/changes` (incremental; `next_seq` + monotonic `current_epoch`; machine-readable `snapshot_required` when `since_seq` predates the prune watermark), `GET /fed/v1/pins/snapshot` (cid-cursor pagination with **per-node epoch consistency** → `409` when this node's set changed past the captured epoch), and `POST /fed/v1/pins/{cid}/{ack,fail}` (generation-keyed conditional state machine: `204` apply / `204` idempotent replay / `409 stale_assignment` / `404` unknown; method guards + body↔path `cid_mismatch` + fail-reason validation). The **advisory-locked, single-transaction** `AssignPin`/`UnpinPin` seam is the **only** writer of assignment state — committed `bigserial` sequences are commit-order-safe (a donor never advances its cursor past a row that can still commit) — and is reused by `novactl pin assign|unpin|list` (DB-direct operator/test seam; `list` prints **desired assignments** vs **verified holders** separately, the latter empty in M3) and, later, the M5 scheduler. A coordinator retention goroutine prunes `pin_changes` older than `federation.change_log_retention` (default 168h) and advances the watermark; the change-log head is `GREATEST(max(sequence), pruned_through_seq)` so a fully-pruned log never regresses the head into a snapshot-recovery loop. Heartbeat now returns the real `current_epoch`. Donor: durable atomic-JSON local state — `FileStore` (cursor) + `FileAssignmentStore` (the **desired-assignment set**, idempotent by `(assignment_id,generation)`, persisted set-first/cursor-second) — and a register→**immediate sync**→heartbeat+pins-poll control loop over an extended `Client` (`GetChanges`/`GetSnapshot`; **no `Ack`/`Fail` on the interface** — the donor cannot ack in M3); an unknown change `kind` **fails closed** into snapshot resync; `recoverSnapshot` returns the old cursor on any failure (never skips unpersisted state). Donor advertises `pin-change-log/v1` + `snapshot/v1`. Observability is a structured-slog signal set (`fed.changes.*`/`fed.snapshot.*`/`fed.ack.*`/`fed.changelog.pruned`/`fed.assign.txn`/`node.sync.*`), USE/RED-named for a P2-M7 Prometheus promotion. Tests: full coordinator handler matrix + advisory-lock commit-ordering + retention/`snapshot_required` + head-monotonicity, e2e loopback-mTLS changes-convergence + unpin + **deterministic snapshot recovery** (all asserting **zero acked rows**), and donor idempotent-apply / recovery / fail-closed / crash-before-cursor / 409-restart. `donor-deps-boundary` + `migrations-frozen` stay green. Implemented (tag `p2-m3-assignment-sync`). Design: `docs/superpowers/specs/phase2/2026-06-22-phase2-m3-assignment-sync-design.md`. Plan: `docs/superpowers/plans/phase2/2026-06-22-phase2-m3-assignment-sync.md`. Deferrals: coordinator-as-source + streaming transfer + deterministic re-import + Ed25519 token mint + donor↔donor repair + production donor fetch→verify→**ack** → M4; 5-state liveness sweeper + healing + `blob_replication_state` + placement → M5; possession audits + reputation → M6; Prometheus `/metrics` → M7. |

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

## Phase 6 — Multi-coordinator, single-authority HA (post-1.0)

Remove the coordinator as an availability single point of failure without ever
allowing two authorities to diverge. Several active coordinators behind
redundant ingress read one **strongly-consistent Postgres authority** (primary +
fenced streaming standbys); **exactly one fenced control-plane leader**
(monotonic control-term token) runs orchestration, liveness transitions, audits,
lifecycle sweeps, master-key rotation, and cert revocation. Reads and donor-API
traffic are active-active. Builds on and automates the manual
`docs/recipes/COLD_STANDBY.md` pattern with mechanical fencing.

Groundwork (surfaced by the second-pass resilience analysis so it is not built
as accidental tech debt): job-queue + control-plane fencing tokens
(`lease_id`/`generation`, `coordinator_leases(term)`), origin-location tracking
with a transactional outbox for the Kubo-pin/Postgres-commit boundary,
multi-endpoint donor config with `since_seq` cursor preservation, replicated or
shared upload staging, cross-instance signed-URL revocation, and redundant
Nebula lighthouses + Kubo bootstrap peers. Reframes `T1.27`; explicitly rejects
independent writable masters. Design + simulation evidence:
`docs/superpowers/specs/phase6/2026-06-12-resilience-and-post-1.0-architecture-design.md`.

## Phase 7 — Opaque inter-federation replica peering (post-1.0)

Off-site durability and disaster recovery across operators **without merging
trust domains**. A `peer/v1` protocol (distinct from donor `fed/v1`) in which a
peer stores **opaque ciphertext only** — never keys, plaintext, catalog,
moderation state, or assignment history. Invariants: every object has exactly
one home federation; peers count as at most one failure domain each (lease- and
audit-gated); no transit / no re-export without home authorization; signed,
generation-ordered tombstones propagate crypto-shred even to peers that no
longer hold the object; optional **encrypted DR packages** (Postgres base backup
+ WAL + manifests, encrypted under a recovery key the peer does not hold) turn
peering from ciphertext durability into full federation reconstruction. Peering
replicates bytes, not authority. Reframes `T1.28`.

## Phase 8+ — Research

Speculative directions: end-user client direct integration, browser-resident
pinning via WASM, FFI bindings for non-Go embedding, additional product modules
(`nova-video`, `nova-audio`, `nova-archive`, `nova-document`), formal Provable
Data Possession / Proof of Retrievability, hot-tier / cold-tier auto-migration,
optional S3 read-only adapter product layer, erasure coding for large archival
objects.

(v3.1: streaming-AEAD envelope was promoted from Phase 6+ research to a Phase 2
deliverable. v3.2 — 2026-06-12: multi-coordinator HA and inter-federation
peering were promoted from the Phase 6+ research grab-bag into deliberate
post-1.0 Phases 6 and 7, and the remaining research items renumbered to
Phase 8+; the earlier "read-only secondary coordinator" research line is
superseded by Phase 6. See the 2026-06-12 resilience design.)
