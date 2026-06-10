# Phase 1 M14 — Polish, Security Housecleaning, and Release Candidate Design

## Purpose and scope

M14 is the **fourteenth and final Phase-1 milestone**. M13 made Nova deployable; M14 makes it
**releasable**: the CI pipeline goes fully green and gains the long-promised end-to-end smoke test
against the real compose stack, the operator gets the screenshot-rich `docs/quickstart.md`, the M13
TLS deferral (certbot initial issuance + deploy-hook reload) closes, and Phase 1 ends with the
`v0.1.0-rc1` release-candidate tag.

The milestone also absorbs a **security-housecleaning batch** that arrived with GitHub's scanners:

- **Two CI jobs are red.** The `lint` job fails because golangci-lint v1.64.8 (the final v1 release,
  built with go1.24) refuses the `go: "1.25"` target in `.golangci.yml` — and CI's `setup-go` pins
  `1.25` while `go.mod` says `1.26.2`. The `schema-drift` job fails **permanently and by design**:
  `internal/db/migrations/0001_init.sql` is correctly frozen (forward-only migration) while
  `docs/specs/DATA_MODEL.sql` was deliberately evolved during M9–M11 into an annotated living
  reference (blocklist table, `blobs.soft_deleted_at`, master-key-rotation commentary). The
  "bit-identical" invariant the job enforces no longer describes the repository's intent; the two
  file headers literally contradict each other.
- **25 Dependabot alerts (10 distinct advisories).** Triaged in full below. Every npm advisory lives
  in the **dev toolchain** (Vite 4.5.14 / Vitest 0.34.6 / esbuild 0.18.20 era pins, frozen in M11–M13
  by the since-retired Node 16 constraint); none is exploitable in production Nova because the SPAs
  ship as static hermetic bundles and no Vite/Vitest process ever runs in a deployment. Two Go
  advisories are **runtime-reachable transitive deps via embedded Kubo** (quic-go, the OTLP trace
  exporter) and get patch bumps.
- **Node 20 went EOL April 2026.** CI and the Docker node-builder stage move to Node 22 (active LTS),
  and **toolchain currency becomes standard process** (Dependabot config + a documented policy)
  instead of a per-incident scramble.

**Scope posture: many small, independently verifiable work packages, ordered CI-green-first.** Unlike
M7–M13 there is no single vertical slice; the correctness of this milestone lives in (a) restoring
trustworthy CI *before* changing anything it guards, (b) the e2e smoke proving the *shipped compose
artifact* (not just the in-process test harness) works end-to-end, and (c) the docs telling the truth
about Phase 1 when the RC tag is cut.

### Confirmed decisions

- **Schema-drift check → migration-immutability check.** The bit-identical diff is replaced by a
  sha256 manifest of shipped migrations: editing any shipped migration fails CI; adding a new one
  requires a reviewed manifest entry. `DATA_MODEL.sql` stays the annotated living reference;
  `0001_init.sql`'s stale "MUST remain bit-identical / Drift fails CI" header is corrected. This
  preserves a *real* invariant (goose forward-only discipline) instead of a dead one.
- **Release signing (sigstore/cosign) + `release.yml` image push → Phase 5.** The master plan already
  said so; the M13 design's deferral line ("Release signing … M14") is the error and gets fixed. The
  RC is a **local annotated tag** (`v0.1.0-rc1`), consistent with the no-remote-push milestone
  workflow.
- **Web toolchain: latest stable majors + Node 22.** Vite 7.x, Vitest latest stable (≥ 3.2.6 patched
  floor), current `@vitejs/plugin-react`, across all three workspaces; Node 22 in CI,
  `docker/Dockerfile`, root `engines`, and a new `.nvmrc`. The minimal-patch alternative (Vite 6.4.2
  on EOL Node 20) was rejected as doing the work twice.
- **Container hardening: cheap compose-level floors only.** Healthcheck matrix for every service,
  read-only rootfs + tmpfs where feasible, `cap_drop: ALL` + `no-new-privileges` with documented
  exceptions. seccomp/AppArmor profiles and base-image experiments stay Phase 5 (the security-audit
  milestone), per the M13 deferral split.

### In scope

- **WP1 — CI repair: golangci-lint v2 migration.** `.golangci.yml` migrates to the v2 config schema
  (`golangci-lint migrate` as the mechanical baseline: `version: "2"`; `gofmt`/`goimports` move to
  the `formatters` section; `issues.exclude-rules` → `linters.exclusions.rules`; the `run.go`
  override is dropped so the target derives from `go.mod`). The CI lint job moves to
  `golangci/golangci-lint-action@v8` with a current v2.x binary. Both Go jobs switch from
  `go-version: "1.25"` to **`go-version-file: go.mod`**, eliminating this skew class permanently.
- **WP2 — CI repair: migration-immutability check.** New
  `internal/db/migrations/MANIFEST.sha256` (one line per shipped migration, `sha256sum` format,
  0001–0009 at authoring time) + `scripts/check-migrations-frozen.sh` (verifies every listed file
  hashes correctly **and** every `NNNN_*.sql` file in the directory is listed — both drift
  directions fail). The `schema-drift` CI job is replaced by this check. `0001_init.sql`'s header is
  rewritten to state the real invariant and point at `DATA_MODEL.sql` as the annotated reference.
- **WP3 — Go dependency security.** Module-graph minimum bumps for the two runtime advisories:
  `github.com/quic-go/quic-go` → v0.59.1 and
  `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp` → v1.43.0 (both transitive via
  `kubo`/`boxo`; a `require` bump + `go mod tidy` raises the selected version). Full triage table
  below.
- **WP4 — npm toolchain upgrade + Node 22.** All three workspaces (`web/admin`, `web/widget`,
  `web/setup`): Vite 7.x, Vitest latest stable, `@vitejs/plugin-react` current major, with
  jsdom/testing-library bumps as the Vitest major requires. The widget keeps its library-mode IIFE
  build (stable entry filename, CSS injected at runtime). Node 22 lands everywhere at once: root
  `engines` → `>=22`, new `.nvmrc` (`22`), CI `node-version-file: .nvmrc`, Dockerfile node-builder
  `node:20-bookworm` → `node:22-bookworm`. Root `package-lock.json` is regenerated. The existing
  lint/test/build + `hermetic-spa`/`hermetic-widget`/`hermetic-setup` gates are the designed safety
  net for exactly this jump.
- **WP5 — Toolchain currency as standard process.** `.github/dependabot.yml` (weekly; ecosystems
  `gomod`, `npm`, `github-actions`; minor/patch grouped to keep PR noise down — the `github-actions`
  ecosystem would have caught the lint-action skew before it broke). A short **"Toolchain currency"**
  policy section in `CONTRIBUTING.md`: Node tracks active LTS (bump within the next milestone after
  an LTS transition), the `go.mod` directive tracks current stable Go, and Dependabot-alert triage is
  part of every milestone's definition of done.
- **WP6 — CI end-to-end smoke (the headline deliverable).** `scripts/smoke.sh` grows from its M1
  shape (postgres + migrate + schema assert) into the full-stack proof: build the image → headless
  setup (`novactl setup --config-file` with `dev-self-signed` TLS, run against the compose volumes)
  → `--profile prod up` → wait healthy → curl-driven **upload → finalize → `GET /blob/{cid}`
  byte-roundtrip → `/i/{cid}/…` transform → authenticated soft-delete → gone** → teardown. A new CI
  `smoke` job runs `make smoke` (ubuntu-latest ships compose). This proves the *artifact operators
  deploy*, complementing (not replacing) the in-process testcontainers suite.
- **WP7 — Certbot completion (M13 deferral).** The `http-01` prod profile becomes fully automated:
  **initial issuance** (`certbot certonly --webroot` on first boot when no certificate exists for the
  configured hostname) and **renewal with reload** (deploy-hook signalling so nginx actually serves
  the renewed cert). The bootstrap deadlock (nginx can't start with missing cert files; certbot can't
  answer challenges without nginx on :80) is broken with a **self-signed placeholder leaf** generated
  at entrypoint when the real cert is absent; nginx starts, certbot issues, and a cert-watcher reload
  loop in the nginx container picks up the real certificate. `dns-01`/`onion` remain
  operator-handoff (unchanged from M13).
- **WP8 — Container hardening floors.** Compose-level: healthchecks for **all** services (postgres
  already has one), `read_only: true` + `tmpfs` where feasible (postgres already done; coordinator
  and nginx gain it; their writable paths are already volume-mounted), `cap_drop: ALL` +
  `security_opt: ["no-new-privileges:true"]` with **documented exceptions** (the coordinator
  entrypoint's `gosu` drop needs `SETUID`/`SETGID`(+`CHOWN`); nginx needs
  `NET_BIND_SERVICE`/`SETUID`/`SETGID`/`CHOWN` for :80/:443 + worker drop). Validated by the WP6
  smoke.
- **WP9 — `docs/quickstart.md` + screenshots.** The screenshot-rich operator quickstart M13 promised:
  prerequisites → `docker compose --profile setup up` → wizard walkthrough (screenshots captured from
  a real `dev-self-signed` run, stored under `docs/images/quickstart/`) → first upload (widget embed
  snippet) → admin SPA tour pointer → TLS-mode selection guidance → "next steps" linking
  `docs/legal/OPERATOR_CHECKLIST.md` as the deep runbook. The master plan's separate
  `docs/operator-runbook.md` deliverable is **declared satisfied** by the M13-expanded
  `OPERATOR_CHECKLIST.md` (master-plan wording updated; no duplicate runbook is created).
- **WP10 — Phase-1 docs reconciliation + RC tag.** Detailed below under "Source of truth."

### Out of scope (with the milestone/owner that holds each)

- **Release signing (cosign) + `release.yml` image publish.** Phase 5 (confirmed decision; the M13
  spec's contrary line is corrected by this milestone).
- **seccomp/AppArmor profiles, distroless/base-image experiments, chaos testing.** Phase 5. (glibc is
  required by `govips`/`libvips`, resolved in M5 — distroless remains unsuitable regardless.)
- **`dns-01` / `.onion` automation.** Later, unchanged from M13 (operator handoff).
- **`operator.yaml` decode of the M7–M12 tuning knobs.** Later, unchanged from M13 (env-only).
- **Vitest UI / Vite preview workflows.** Not introduced; the upgrade is for hygiene, not new dev
  tooling surface.
- **Kubo/boxo major upgrades.** Only the two advisory-driven transitive bumps; the embedded Kubo
  0.41 line is not otherwise moved in M14.

## Dependabot triage (the exploitability determination)

25 alerts collapse to 10 distinct advisories. "Reachable on Nova?" asks: can an attacker exercise the
vulnerable code in a **production deployment** (compose prod profile) or in **CI**? Dev-machine
exposure assumes the documented loopback defaults.

| # | Advisory | Package (where flagged) | Severity | Reachable on Nova? | Action |
|---|---|---|---|---|---|
| 1 | CVE-2026-47429 / GHSA-5xrq-8626-4rwp — Vitest UI/API server arbitrary file read + RCE | `vitest` < 3.2.6 (`web/setup`, `web/admin`, `web/widget`, root lock) | Critical (10.0) | **No.** The Vitest UI/API server never runs: CI and `make *-test` run headless (`vitest run`); `--api.host` is never set; production ships no Vitest at all. The Windows `\\?\` bypass is additionally irrelevant on Linux CI. | Fixed by WP4 (Vitest ≥ 3.2.6). |
| 2 | CVE-2026-39365 / GHSA-4w7w-66w2-5vf9 — Vite dev-server `.map` path traversal | `vite` ≤ 6.4.1 (`web/setup` + siblings) | Moderate | **No.** Requires an exposed Vite dev server (`--host`). Nova dev servers are loopback-bound; production serves prebuilt `dist/` through the coordinator/nginx — no Vite process exists. | Fixed by WP4 (Vite 7.x). |
| 3 | CVE-2024-52011 / GHSA-c27g-q93r-2cwf — launch-editor command injection via Vite dev server | `vite` ≤ 5.4.8 (via launch-editor) | High | **No.** Windows-only sink; dev-server-only vector; same non-exposure as #2. | Fixed by WP4. |
| 4 | CVE-2025-62522 / GHSA-93m4-6634-74q7 — `server.fs.deny` bypass via trailing `\` | `vite` 4.5.x (root lock, 4.5.14) | Moderate | **No.** Windows-only, dev-server-only. | Fixed by WP4 (lock regenerated). |
| 5 | CVE-2025-58751 / GHSA-g4jq-h2w9-997c — public-dir symlink serve bypass | `vite` ≤ 5.4.19 (`web/setup` + siblings) | Low | **No.** Dev-server-only; requires a symlink in `public/` + network exposure. | Fixed by WP4. |
| 6 | CVE-2025-58752 / GHSA-jqfw-vq24-v9c3 — `server.fs` not applied to HTML | `vite` ≤ 5.4.19 (`web/setup` + siblings) | Low | **No.** Dev-server-only. | Fixed by WP4. |
| 7 | GHSA-67mh-4wv8-2f99 — esbuild serve-mode permissive CORS | `esbuild` ≤ 0.24.2 (root lock, 0.18.20 via Vite 4.5) | Moderate | **No.** esbuild's serve mode is never used; it is a build-time dependency of Vite. | Fixed by WP4 (Vite 7 carries esbuild ≥ 0.25). |
| 8 | CVE-2024-55565 — nanoid non-integer infinite loop / predictable output | `nanoid` (root lock) | Moderate | **No.** Root lock holds nanoid **3.3.12**, outside the vulnerable ranges (≥4.0.0 <5.0.9; <3.3.8); the alert keys on a nested copy in the stale Vite-4-era tree. Build-time-only either way. | Resolved by WP4 lock regeneration; verify with `npm ls nanoid`. |
| 9 | CVE-2026-40898 / GHSA-vvgj-x9jq-8cj9 — quic-go HTTP/3 QPACK trailer memory exhaustion (DoS) | `github.com/quic-go/quic-go` v0.59.0 (go.mod, transitive via kubo/boxo) | Moderate | **Plausibly.** This is a **runtime** dependency: the embedded Kubo node runs libp2p QUIC listeners. With the default private-swarm posture (PSK swarm key) an attacker must hold the swarm key; with `public_ipfs_dht: true` the listener is internet-reachable. Worst case is memory-exhaustion DoS, not compromise. Treated as reachable. | **WP3: bump to v0.59.1.** |
| 10 | CVE-2026-39882 / GHSA-w8rr-5gcm-pp58 — OTLP HTTP exporter unbounded response read (DoS) | `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp` v1.42.0 (go.mod, transitive via kubo) | Moderate | **Effectively no.** Exploitation requires the process to *export traces to an attacker-controlled collector*. Nova configures no OTLP exporter; the package rides in via Kubo and is dormant unless an operator deliberately wires tracing to a hostile endpoint. | **WP3: bump to v1.43.0** (hygiene; closes the alert). |

**Summary verdict:** nothing in the alert set is exploitable against a production Nova deployment
today. One advisory (quic-go, #9) is reachable in realistic configurations as a DoS and is patched on
its own merits. Everything else is dev-toolchain debt — fixed wholesale by the WP4 upgrade, which was
already owed once the Node 16 constraint died.

## Source of truth and required doc reconciliations

1. **`docs/ROADMAP.md` — the M14 row.** On completion: status ✅, the completed-style summary
   (CI repairs, triage outcome, toolchain jump, smoke, certbot, hardening floors, quickstart, RC),
   the `m14-polish-release` + `v0.1.0-rc1` tags, links to this design + the implementation plan, and
   the Phase-5 deferrals (signing, seccomp/AppArmor, chaos). Phase 1 section marked **complete at
   RC**.
2. **The master plan (`2026-05-25-phase1-single-node-mvp-design.md`).** Mark M14 status/links;
   reconcile the M14 deliverables list with what ships (notably: `docs/operator-runbook.md` is
   satisfied by `OPERATOR_CHECKLIST.md`; sigstore stays Phase 5 — already its position).
3. **The M13 design (`2026-06-08-phase1-m13-setup-wizard-design.md`).** Single-line correction:
   "Release signing (sigstore/cosign) + `release.yml` image push. **M14**" → **Phase 5** (the
   master plan was always the source of truth here).
4. **`README.md`.** Status refresh (Phase 1 complete at `v0.1.0-rc1`), quickstart pointer to
   `docs/quickstart.md`, Node 22 / Go 1.26 prerequisites where stated.
5. **`CONTRIBUTING.md`.** The WP5 "Toolchain currency" policy section.
6. **`internal/db/migrations/0001_init.sql` header** (WP2) — states the immutability invariant and
   points at `DATA_MODEL.sql` as the annotated living reference; drops "bit-identical"/"Drift fails
   CI".
7. **`docs/VOLUNTEER_DEPLOYMENT_GUIDANCE.md` + `docs/recipes/NGINX_REFERENCE.md`.** Spot-fix stale
   pre-M13 claims (deployment story, single-origin assumptions) — surgical edits, not rewrites.
8. **`docs/quickstart.md`** — created (WP9); `docs/images/quickstart/` for screenshots.

## Preconditions from M1–M13 (confirmed in the committed tree)

- **`make smoke` exists but is M1-shaped.** `scripts/smoke.sh` brings up compose postgres, runs
  migrate, asserts schema, tears down. The Makefile target and `.env.example` seeding logic are
  reusable bones for WP6.
- **The image already embeds everything the smoke needs.** The M13 multi-stage `docker/Dockerfile`
  ships `coordinator` + `novactl` + migrate + all three static bundles + `docker/init/entrypoint.sh`
  (migrate → sentinel check → exec); `novactl setup --config-file` is the headless path the smoke
  drives.
- **The compose prod profile is real.** `docker/docker-compose.yml` has postgres + coordinator +
  nginx(+nginx-setup) + certbot with `setup`/`prod` profiles, per-concern volumes, the ACME webroot
  volume, and a certbot `renew` loop (no deploy-hook, no initial issuance — exactly the WP7 gap).
  Postgres already has `healthcheck` + `read_only: true` — the WP8 pattern to extend.
- **The hermetic gates are dir-parametrized and CI-wired.** `scripts/hermetic-spa.sh` + the
  `hermetic-spa`/`hermetic-widget`/`hermetic-setup` Make targets are the regression net for WP4.
- **`go.mod` is `go 1.26.2`**; the toolchain skew is CI-side only (`setup-go: "1.25"`,
  `.golangci.yml run.go: "1.25"`).
- **Migrations 0001–0009 are the shipped set** (`0001_init` … `0009_blob_soft_delete`), with
  `migrations.go`/`migrations_test.go` alongside (the manifest check must list only `NNNN_*.sql`).
- **The two-vhost nginx template** (`internal/setup/templates/nova.conf.tmpl`) renders the
  `http-01` ACME webroot location; WP7 touches it only if the placeholder-cert path requires.

## Architecture

No new Go packages. M14's structure is scripts + CI + config + docs:

```
.github/
  workflows/ci.yml        lint→golangci v2 + go-version-file; schema-drift→migrations-frozen;
                          node-version-file; new smoke job
  dependabot.yml          NEW — gomod + npm + github-actions, weekly, grouped
.golangci.yml             v2 schema (formatters split; exclusions; no run.go)
.nvmrc                    NEW — 22
internal/db/migrations/
  MANIFEST.sha256         NEW — sha256 of every shipped NNNN_*.sql
  0001_init.sql           header comment only (immutability invariant)
scripts/
  check-migrations-frozen.sh   NEW — manifest verify (both drift directions)
  smoke.sh                M1 shape → full-stack e2e (build → headless setup → prod up → upload/
                          read/transform/delete → teardown)
docker/
  Dockerfile              node:20-bookworm → node:22-bookworm
  docker-compose.yml      WP7 certbot issuance + WP8 hardening floors (healthchecks, read_only,
                          cap_drop/no-new-privileges with documented exceptions)
  init/entrypoint.sh      http-01 placeholder-cert generation (WP7)
  nginx/                  cert-watcher reload wrapper (WP7)
web/{admin,widget,setup}/ package.json + vite.config.ts: Vite 7 / Vitest 3 line / plugin-react
docs/
  quickstart.md           NEW — screenshot operator quickstart
  images/quickstart/      NEW — wizard screenshots
  (+ reconciliations: ROADMAP, master plan, M13 spec line, README, CONTRIBUTING,
     VOLUNTEER_DEPLOYMENT_GUIDANCE, NGINX_REFERENCE)
go.mod / go.sum           quic-go v0.59.1; otlptracehttp v1.43.0
package.json / package-lock.json   engines >=22; regenerated lock
```

### The smoke-test flow (WP6, the load-bearing piece)

```
make smoke
  ├─ docker build (the real multi-stage image)
  ├─ generate answers.yaml          dev-self-signed; local issuer; operator user with a
  │                                 generated password; public_uploads on + tos_url
  │                                 (the T1.20 floor) so an unauthenticated upload works
  ├─ one-off container: novactl setup --config-file answers.yaml
  │                                 (stages secrets + operator.yaml + nova.conf into the
  │                                 compose volumes; writes .bootstrap-complete LAST)
  ├─ docker compose --profile prod up -d
  ├─ wait: compose healthchecks + /readyz via public_host (curl -k --resolve, dev CA)
  ├─ EXERCISE
  │    1. POST multipart upload → finalize → cid
  │    2. GET /blob/{cid} → bytes identical to the fixture
  │    3. GET /i/{cid}/<preset> → 200, image content-type
  │    4. login operator @ admin_host → DELETE /api/v1/blobs/{cid} (soft-delete)
  │    5. GET /blob/{cid} → gone (the soft-delete read posture)
  └─ teardown (compose down -v on the temp project)
```

Decisions baked in: the smoke uses **public uploads + ToS URL** so step 1 needs no token (and
exercises the T1.20 floor exactly as a real anonymous-upload deployment would), while step 4
exercises the **admin-host auth path** with the operator user the headless setup created — together
the two vhosts, the sentinel lifecycle, TLS (`dev-self-signed`), upload, read, transform, and
moderation-adjacent delete are all proven against the shipped artifact in one pass. CI cannot do real
ACME; `http-01` issuance is validated at the config/unit level plus the human-action staging check in
the runbook (unchanged posture from M13).

### The certbot completion (WP7)

```
nginx (http-01 mode) entrypoint:
  leaf cert missing? → generate self-signed placeholder (hostname SAN) → nginx starts
  + background watcher: cert file mtime changed → nginx -s reload   (6h cadence + SIGHUP-safe)

certbot service:
  on start: cert for $HOST absent → certbot certonly --webroot -w /var/lib/certbot/webroot
            -d $HOST --email $CONTACT --agree-tos --non-interactive
  then:     loop: certbot renew --webroot … (renewed certs land on the shared volume;
            the nginx watcher performs the reload — no cross-container signalling)
```

The placeholder-cert trick resolves the M13 bootstrap deadlock (nginx refuses to start when the
configured cert files don't exist, but certbot needs nginx serving `:80` to answer the challenge)
without adding any cross-container control channel.

## Security and privacy considerations

- **The triage table is the deliverable, not just the bumps.** The design records *why* each advisory
  is or isn't reachable so future alert triage has a precedent format (and so the "fix everything
  anyway" outcome isn't mistaken for "everything was exploitable").
- **CI-green-first ordering is a security property.** The lint and migration-immutability gates are
  restored *before* the dependency and toolchain changes land, so every subsequent WP is reviewed by
  a working pipeline.
- **The migration-immutability check protects the crypto-shred audit trail.** Shipped migrations
  define the `no_shred_under_legal_hold` CHECK, partition layouts, and key-state machines; silent
  edits to applied DDL are a correctness *and* compliance hazard. The manifest makes tampering a
  loud, reviewed event.
- **Node 22 / dependency currency is the cheap half of supply-chain defense.** EOL runtimes stop
  receiving security patches; the WP5 policy + Dependabot config make currency routine. The hermetic
  gates ensure the upgraded toolchain still produces CDN-free bundles (the actual production
  surface).
- **Hardening floors reduce blast radius, not threat-model claims.** `cap_drop`/`no-new-privileges`/
  read-only rootfs narrow what a compromised service can do; the documented exceptions (gosu's
  SETUID/SETGID, nginx's NET_BIND_SERVICE) keep the floors honest. No THREAT_MODEL boundary changes
  — Phase 5 owns the audited hardening pass.
- **The smoke test's secrets are ephemeral.** Generated per-run into a temp compose project and
  destroyed at teardown; nothing is committed; the CI job uses no repository secrets.

## Exit criteria

1. **CI is fully green on the milestone branch**: lint (golangci-lint v2 against the migrated
   config), the migration-immutability check, Go vet/codegen/unit/integration, all three SPA lanes +
   hermetic gates on Node 22, docker-build, and the new `smoke` job.
2. **The Dependabot alert set is closed**: quic-go ≥ 0.59.1 and otlptracehttp ≥ 1.43.0 in `go.mod`;
   no vite < 6.4.2 / vitest < 3.2.6 / esbuild ≤ 0.24.2 / vulnerable-range nanoid anywhere in the
   regenerated lockfile (`npm ls` spot-checks); the triage table in this design records the
   reachability verdicts.
3. **Node 22 everywhere**: `.nvmrc`, root `engines`, CI `node-version-file`, Dockerfile node-builder;
   `docs`/`CONTRIBUTING.md` record the currency policy; `.github/dependabot.yml` exists.
4. **`make smoke` passes locally and in CI**, executing the full flow above against the built image
   (build → headless setup → prod profile → upload → byte-identical read → transform → authenticated
   soft-delete → gone).
5. **`http-01` is fully automated in the prod profile**: first boot with no cert issues via webroot
   (placeholder-cert bootstrap, proven by unit/config tests + the smoke's nginx-start path);
   renewal lands certs that nginx picks up via the watcher reload (staging-ACME validation is the
   documented human action).
6. **Hardening floors are in compose**: every service has a healthcheck; coordinator + nginx run
   read-only with tmpfs/volume writable paths; `cap_drop: ALL` + `no-new-privileges` with the
   exceptions documented inline in the compose file.
7. **`docs/quickstart.md` exists** with real wizard screenshots and survives a cold read (a person
   with Docker and a clone reaches "uploaded and served" without consulting any other doc; deep
   material is linked, not duplicated).
8. **All doc reconciliations land** (§ "Source of truth") and the milestone finishes with a
   fast-forward merge to `main` + annotated tags **`m14-polish-release`** and **`v0.1.0-rc1`**
   (local; no remote push).

## Testing strategy

- **Go**: no production-code changes expected outside `go.mod`; the full unit + integration suites
  are the regression net for the dependency bumps (quic-go rides under Kubo's libp2p — the
  M2/M3/M5 integration paths exercise it). Any WP7 Go-side template change gets a render unit test in
  `internal/setup`.
- **Shell**: `check-migrations-frozen.sh` gets self-tests-by-construction (run against a temp dir
  with a tampered file and a missing manifest entry — both must fail; wired as a Make target check or
  test fixture in the implementation plan). `smoke.sh` is its own test; shellcheck-clean like the
  existing scripts.
- **Frontend**: the existing Vitest suites must pass unmodified-in-intent under the new majors
  (config-level changes only: `vitest.config`/`vite.config` API drift, jsdom env). The three hermetic
  gates assert the upgraded toolchain still emits CDN-free bundles; the widget's IIFE
  global/entry-filename contract is asserted by the existing M12 tests.
- **CI**: the lint job is validated once locally with a downloaded golangci-lint v2 binary
  (`golangci-lint config verify` + full run) before being trusted in CI; the smoke job is capped
  (~15 min timeout) and uploads compose logs on failure for diagnosability.
- **Human-action checklist** (runbook items, not CI): real-ACME staging issuance for `http-01`;
  a cold-read pass of `docs/quickstart.md` on a fresh machine; screenshot capture.

## File structure

### Created in M14
```
.github/dependabot.yml
.nvmrc
internal/db/migrations/MANIFEST.sha256
scripts/check-migrations-frozen.sh
docker/nginx/cert-watch.sh
docs/quickstart.md
docs/images/quickstart/*.png
docs/superpowers/specs/2026-06-09-phase1-m14-polish-release-design.md   (this file)
docs/superpowers/plans/2026-06-09-phase1-m14-polish-release.md          (the implementation plan)
```

### Modified in M14
```
.github/workflows/ci.yml          lint v2 + go-version-file + node-version-file + migrations-frozen + smoke job
.golangci.yml                     v2 schema
go.mod / go.sum                   quic-go v0.59.1; otlptracehttp v1.43.0
web/admin/package.json            Vite 7 / Vitest 3-line / plugin-react current (+ vite.config.ts as needed)
web/widget/package.json           same (library-mode IIFE preserved)
web/setup/package.json            same
package.json                      engines >=22
package-lock.json                 regenerated
docker/Dockerfile                 node:22-bookworm
docker/docker-compose.yml         certbot issuance + healthchecks + read_only + cap_drop/no-new-privileges
docker/init/entrypoint.sh         http-01 placeholder-cert generation
internal/db/migrations/0001_init.sql   header comment only
scripts/smoke.sh                  full-stack e2e
Makefile                          smoke description; migrations-frozen target
CONTRIBUTING.md                   toolchain-currency policy
README.md                         Phase-1-complete status + quickstart pointer
docs/ROADMAP.md                   M14 row ✅ + Phase 1 complete-at-RC
docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md   M14 status; operator-runbook wording
docs/superpowers/specs/2026-06-08-phase1-m13-setup-wizard-design.md  signing-deferral line → Phase 5
docs/VOLUNTEER_DEPLOYMENT_GUIDANCE.md   stale pre-M13 claims
docs/recipes/NGINX_REFERENCE.md         stale single-origin claims
```

### Reused unchanged
```
scripts/hermetic-spa.sh + the hermetic-* Make targets    the WP4 regression net
docker/Dockerfile multi-stage layout                      only the node base tag moves
internal/setup/templates/nova.conf.tmpl                   unless WP7's placeholder path requires
internal/integration/ (testcontainers suite)              untouched; the smoke complements it
docs/legal/OPERATOR_CHECKLIST.md                          the deep runbook quickstart links to
```

## Risks and notes

- **golangci-lint v2 may surface new findings** (staticcheck/revive evolve across two majors). Policy:
  fix trivial findings in touched files; add narrowly-scoped exclusions with a comment for anything
  noisy-but-harmless; do **not** mass-rewrite untouched code in a polish milestone. The config
  migration is mechanical (`golangci-lint migrate`); the finding burn-down is the variable cost.
- **Vite 4 → 7 / Vitest 0.34 → 3.x is a multi-major jump.** Known surface: ESM-only config (already
  ESM), `test` config key changes, jsdom/environment options, plugin-react major, the widget's
  `build.lib` IIFE contract. The hermetic gates + existing test suites bound the risk; the fallback
  (Vite 6.4.2) exists if a blocker appears, but is not the plan.
- **The CI smoke is the flakiest new surface** (image build time, compose startup ordering, ACME-less
  TLS trust). Mitigations: compose healthcheck-gated waits (not sleeps), `--resolve`-pinned curl with
  the dev CA, a hard job timeout, log upload on failure. If runner capacity makes it chronically slow,
  it can become a non-blocking lane *temporarily* — but the exit criterion is a blocking green job.
- **Initial ACME issuance cannot be proven in CI.** The placeholder-cert bootstrap and webroot
  rendering are testable; the actual Let's Encrypt round-trip is a documented staging-flag human
  action. This is the same epistemic line M13 drew; M14 just moves it from "renewal scaffold" to
  "fully automated, staging-verified".
- **The manifest check changes the new-migration workflow** (author must append a hash line). The
  check's error message must say exactly that (`sha256sum internal/db/migrations/00NN_x.sql >>
  MANIFEST.sha256`), or it becomes a contributor trap.
- **Node 22 + Vite 7 in the Docker node-builder changes build output bytes** (different esbuild/
  rollup). The hermetic gates re-validate the bundles; the widget's stable-entry-filename contract is
  what downstream embedders depend on and is explicitly re-asserted.
- **Dependabot will now open PRs against a repo whose workflow is local-merge.** Grouping + weekly
  cadence keeps the noise low; the WP5 policy says alerts are triaged per-milestone, not auto-merged.

## Cross-references

- `docs/ROADMAP.md` M14 row; the master plan § "M14 — Polish + docs" (`:1034`–`:1042`) and the M14
  section of `docs/superpowers/plans/2026-05-25-phase1-single-node-mvp.md` (`:133`–`:138`).
- M13 design (`2026-06-08-phase1-m13-setup-wizard-design.md`) § "Out of scope" — the deferral list
  this milestone consumes (and the one line it corrects).
- `.github/workflows/ci.yml` — every WP1/WP2/WP4/WP6 change lands here.
- `docs/specs/DATA_MODEL.sql` header + `internal/db/migrations/0001_init.sql` header — the
  contradiction WP2 resolves.
- `docker/docker-compose.yml` + `docker/init/entrypoint.sh` — the WP7/WP8 surface.
- `docs/legal/OPERATOR_CHECKLIST.md` — the deep runbook `docs/quickstart.md` fronts.
- `docs/superpowers/plans/2026-06-09-phase1-m14-polish-release.md` — the implementation plan.
