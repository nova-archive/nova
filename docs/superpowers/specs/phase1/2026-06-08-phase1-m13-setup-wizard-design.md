# Phase 1 M13 — Setup Wizard + Docker Production Design

## Purpose and scope

M13 is the **thirteenth Phase-1 milestone** and the one where Nova stops being a set of compiled
capabilities and becomes a **deployable product**. M11 shipped the operator command center, M12 the
end-user uploader; M13 ships the **first-run setup wizard and the Docker production topology** that
carry an operator from `git clone` to a live, TLS-fronted node *without hand-editing the ~25
environment variables* `cmd/coordinator` reads today. M14 then polishes (CI end-to-end smoke,
`docs/quickstart.md`, the Phase-1 release-candidate tag).

The milestone is motivated by a concrete gap in the committed tree: there is **no `Dockerfile`**, the
`docker/docker-compose.yml` is **postgres-only** (the coordinator/nginx/certbot services are TODO
comments), the coordinator is **100% env-var driven** even though `internal/config` already fully
models `operator.yaml` (the loader `config.LoadFromFile` + the `validate` refuse-to-start floors exist
but are **not wired into `cmd/coordinator`**), and `README.md` (`:116`–`:119`, `:230`–`:233`) states
plainly that production deployment is unsupported until the wizard + two-vhost nginx + certbot land.
M13 closes that gap.

**Scope posture: full vertical slice, narrowly defined.** M13 owns the *first complete operator
deployment path* — the wizard (web + headless CLI), the `internal/setup` domain core, the
`.bootstrap-complete` sentinel boot mode, the two-vhost templated nginx, the TLS-mode/certbot
plumbing, a real multi-stage coordinator Docker image (Debian-slim/**glibc** — `govips`/`libvips`
needs glibc, resolved in M5), and `setup`/`prod` compose profiles that actually boot the stack
end-to-end. It deliberately **defers** exhaustive container hardening, release signing, full CI
end-to-end smoke coverage, chaos testing, and screenshot-rich operator prose to M14/Phase 5. The
milestone is **done** when *a fresh checkout can become a working Nova deployment without hand-editing
twenty env vars* — not when *the wizard handlers compile*.

The load-bearing work is not the stepper UI; it is the **first-run lifecycle**: a reduced-boot
coordinator that mounts only `/setup/*` while the sentinel is absent, an **atomic commit** that stages
secrets → writes config → creates the operator → writes the sentinel **last** → re-execs into normal
mode, and the **two-vhost origin split** that keeps a public-origin XSS or cache-poison from becoming
administrative trust (`THREAT_MODEL.md` boundary ①). Getting that boundary and that ordering correct
is where the milestone's correctness lives.

### Confirmed decisions

- **TLS:** fully automate `dev-self-signed` (generate a CA + leaf), `static` (operator-supplied PEM),
  and `http-01` (certbot in the `prod` profile; webroot + nginx reload). `dns-01` + `onion` are
  selectable modes that render the correct `operator.yaml` + `nova.conf` and emit **documented
  operator-handoff instructions** — no DNS-provider plugins or bundled Tor in M13.
- **Two-vhost split:** **nginx-only.** The wizard-rendered `nova.conf` emits a public_host and an
  admin_host `server` block, each routing only its allowed locations to the coordinator. The
  coordinator keeps its single `chi` mux; **no coordinator-side origin-split middleware** is added.
- **`operator.yaml`:** **canonical for non-secret operator decisions, with env overrides preserved.**
  `config.LoadFromFile` becomes the coordinator's base config source; secrets stay on the M6.1
  `env → _FILE → /run/secrets/...` resolver; existing env knobs remain functional as overrides; the
  M7–M12 tuning knobs (signed-URL, master-key rotation, sweep cadences) stay env-only for now.

### In scope

- **`internal/setup/` — the UI-agnostic setup domain core** (shared verbatim by the web wizard backend
  and the headless CLI; this is where the milestone's testable logic lives):
  - `answers.go` — the `Answers` model (operator identity, TLS mode + per-mode params, auth mode,
    public-uploads + ToS URL, paranoid) and a step-by-step `Validate`, reusing `config.validate`'s
    floors where they overlap so the wizard and the runtime refuse the same shapes.
  - `keygen.go` — CSPRNG generation of the **master key** (32 bytes → hex), the **IPFS swarm key**
    (Kubo PSK format: `/key/swarm/psk/1.0.0/` + `/base16/` + 64 hex chars), and the **Ed25519 OIDC
    signing seed** (32-byte hex); plus the **fingerprint** helper (first 8 bytes of the master key,
    hex) used as the forced-readback challenge.
  - `render.go` — render `operator.yaml` from `Answers` (round-tripped through `config.LoadFromBytes`
    so an un-loadable render fails the commit) and render the two-vhost `nova.conf` from the template
    per the chosen TLS mode.
  - `tls.go` — per-mode logic: `dev-self-signed` (generate CA + leaf into the config volume's `tls/`),
    `static` (validate operator-supplied cert/key paths exist + parse), `http-01` (render the ACME
    challenge location + certbot webroot), `dns-01`/`onion` (render config + return the
    operator-handoff instruction text).
  - `commit.go` — the **atomic finalize**, ordered for crash-safety: stage secrets (mode `0600`) into
    the secrets volume → write `operator.yaml` + `nova.conf` into the config volume → `INSERT` the
    admin `users(role='operator')` row (argon2id via the existing `internal/auth/password` +
    `gen.CreateUser`) → write `.bootstrap-complete` **last** → signal re-exec. If the process dies
    before the sentinel is written, the next boot re-enters setup mode cleanly (idempotent staging).
- **`internal/api/handlers/setup.go` — the `/setup/*` coordinator seam.** A nil-gated handler
  (mirroring `admin_spa.go`/`widget_static.go`) mounted **only when the sentinel is absent**. It serves
  the `web/setup` bundle and the small JSON API the wizard drives: `GET /setup/state` (which steps are
  done), `POST /setup/keys/master` (generate + stage + return the hex **and** fingerprint for display +
  the backup file), `POST /setup/answers` (validate the collected answers), `POST /setup/commit`
  (run `commit.go`). Strict CSP (the `web/setup` bundle is hermetic). The setup origin is
  **loopback-bound** — never published on a public interface.
- **`web/setup/` — the wizard SPA.** A hermetic React + Vite app mirroring `web/admin`'s Node-16-safe
  pins (Vite 4.5 / Vitest 0.34 / TypeScript 5.3; CI `hermetic-spa` gate). A single linear stepper:
  Welcome + license → **master-key generation + download-backup + typed-fingerprint readback gate** →
  swarm/signing key generation → admin user (username/email/password, 12+ char strength check) → TLS
  mode (each mode shows its privacy cost — e.g. CT-log disclosure for `http-01`) → ToS URL (required
  when public uploads are on) → paranoid toggle → Review → Commit → a **"you're live" orientation
  page** (where the admin SPA is, the single-`<script>` widget embed snippet, how to upload, the
  secrets-backup reminder). The orientation page is the M13 answer to "explain the features/flows to a
  first-time operator"; the screenshot-rich `docs/quickstart.md` is M14's.
- **Headless `novactl setup`** — `--interactive` (terminal prompts including the fingerprint readback)
  and `--config-file answers.yaml` (non-interactive, for startup scripts / CI). Shares `internal/setup`
  verbatim. This is the wizard-skip path: an experienced operator never opens a browser.
- **The wizard-skip / fully-manual path** — a returning operator who pre-places `operator.yaml` +
  secrets + `.bootstrap-complete` (or runs `novactl setup --config-file`) boots straight into normal
  mode. The wizard is the default, never a requirement.
- **Config reconciliation — wire `config.LoadFromFile` into `cmd/coordinator`.** `operator.yaml`
  becomes the base source for non-secret operator decisions (operator identity, `tls.mode`, auth mode,
  `uploads`/`public_uploads`, `moderation`, `tos_url`, paranoid, `coordinator.public_ipfs_dht`); env
  knobs remain functional as overrides (cheap back-compat); secrets stay on the resolver chain.
- **Two-vhost nginx templating** — `docker/nginx/nova.conf.template`, rendered by the wizard into the
  config volume, emitting two `server` blocks (route map below). `docker/nginx/bootstrap.conf` proxies
  **only** `/setup/*` during first-run.
- **TLS modes + certbot** — as in "Confirmed decisions."
- **Docker image + compose** — a multi-stage `docker/Dockerfile` (go-builder → node-builder →
  Debian-slim/glibc non-root runtime, embedding `coordinator` + `novactl` + the migrate tool + the
  admin/widget/setup static bundles + the entrypoint); `docker/init/entrypoint.sh` (migrate →
  sentinel check → exec in setup vs normal mode); `docker-compose.yml` grown to postgres + coordinator
  + nginx with `setup` and `prod` profiles and per-blast-radius volumes.
- **Tests** — Go unit (`internal/setup`: keygen format/length, fingerprint gate, render →
  `LoadFromBytes` round-trip, per-mode nginx render, commit ordering with sentinel-last), the
  `setup.go` handler (state/keys/answers/commit + sentinel-gated 404), Vitest for the stepper + the
  readback gate, and an nginx-fronted integration test proving the two-vhost split and the setup→normal
  sentinel flip.

### Out of scope (with the milestone/owner that holds each)

- **Exhaustive container hardening** — read-only-rootfs polish, distroless experiments, the full
  healthcheck matrix, seccomp/AppArmor profiles. **M14 / Phase 5.** M13 ships the Tier-1 floors the
  startup validator already requires (non-root UID, hardened Kubo, swarm-key-or-explicit-DHT,
  master-key loaded, sentinel present in normal mode) — not a security-audited 1.0 appliance.
- **Release signing (sigstore/cosign) + `release.yml` image push.** **Phase 5** (per the master plan; this line previously said M14 in error).
- **CI end-to-end smoke test + screenshot-rich `docs/quickstart.md`.** **M14.** M13 ships the inline
  first-run orientation page in lieu of external prose.
- **Full `dns-01` / `.onion` automation** — DNS-provider credential plugins, bundled Tor. Later; M13
  renders config + operator handoff for these two modes.
- **Full `operator.yaml` decode of the M7–M12 tuning knobs** — signed-URL, master-key rotation, and
  sweep cadences stay env-only (the per-milestone deferral precedent holds for one more milestone).
- **Coordinator-side origin-split middleware** — the two-vhost split is enforced at nginx; no
  `internal/api/middleware/origin_split.go` is added in M13.
- **External-OIDC worked example (Authelia template).** An M6 follow-up; the M13 wizard offers
  local-issuer vs external-OIDC **mode selection** only.

## Source of truth and required doc reconciliations

1. **`docs/ROADMAP.md` — the M13 row.** Mark status, link this design + its implementation plan,
   record the `m13-setup-wizard` tag on completion, and record the deferrals (hardening / signing /
   CI-smoke / quickstart → M14; `dns-01`/`onion` automation → later; `operator.yaml` tuning-knob decode
   → later).
2. **The master plan (`docs/superpowers/specs/phase1/2026-05-25-phase1-single-node-mvp-design.md`).** Mark
   M13 status/links and reconcile its "Onboarding wizard" + container-topology sections with what
   actually ships (notably: setup mode is *folded into the coordinator boot path*, not a second
   long-lived binary; `operator.yaml` is canonical-with-env-overrides, not a wholesale env replacement).
3. **`docs/THREAT_MODEL.md` — boundaries ① and ①a.** Confirm the public/admin **two-vhost split** and
   the **loopback-only, self-disabling** setup wizard are now *implemented* (not planned). Note the
   widget bundle is served on the **public_host** (it is the end-user uploader; its API target
   `/api/v1/uploads/*` is a public-origin route).
4. **`docs/specs/openapi.yaml` — note-only.** Document the **ephemeral** `/setup/*` surface
   (sentinel-gated; present only during first-run) the same way M11/M12 documented the static
   `/admin/*` and `/widget/*` surfaces. The steady-state API is unchanged; keep the `oapi-codegen`
   drift gate green.
5. **`docs/legal/OPERATOR_CHECKLIST.md` — the first-run runbook.** The wizard path
   (`docker compose --profile setup up` → loopback wizard → commit → restart), the headless
   `novactl setup` path, the fully-manual skip path, per-TLS-mode guidance, the **secrets-volume backup
   obligation**, and **sentinel re-arming** (delete `.bootstrap-complete` to redo setup).
6. **`README.md`.** Replace the "production unsupported until M11–M13" note (`:116`–`:119`,
   `:230`–`:233`) with the `docker compose --profile setup up` quickstart pointer; the full operator
   quickstart remains M14.

## Preconditions from M1–M12 (confirmed in committed code)

- **The `operator.yaml` loader + floors exist and are unused by `cmd`.** `config.LoadFromFile` /
  `LoadFromBytes` parse + `validate` (`internal/config/operator_yaml.go:11`–`:99`) enforce
  `operator.hostname`/`contact_email` required, `tls.mode ∈ {dev-self-signed,http-01,dns-01,static,onion}`
  (`static` requires `cert_path`+`key_path`), replication bounds, the `auth.anonymous`+no-moderation
  refusal, and **T1.20** (`uploads.public_uploads` requires `tos_url`). `Config` (`types.go`) already
  models every operator-facing section. M13 wires the loader into `cmd/coordinator`; it does not
  rewrite the floors.
- **`cmd/coordinator/main.go` is the env surface to reconcile.** Its header documents ~25 `NOVA_*`
  vars; `run()` reads them directly and assembles `coordinator.Config` (`:189`–`:233`). The M13 change
  loads `operator.yaml` first, then applies env as overrides — preserving every existing test and
  deployment that sets env.
- **Admin-user creation is already available.** `gen.CreateUser` (`internal/db/gen/auth.sql.go:15`;
  `INSERT INTO users (email, role, password_hash)`), `UserRoleOperator` (`models.go:598`), and
  `internal/auth/password` (argon2id) are the primitives `commit.go` composes. No new query or hashing
  code is needed.
- **The nil-gated static-handler seam is the precedent to mirror.** `pkg/coordinator/coordinator.go`
  builds `handlers.NewAdminSPA(distDir, ...)` / `handlers.NewWidgetStatic(distDir)` (`:399`–`:400`)
  which return nil when the dir is empty; `server.go` (`:107`–`:118`) mounts `/admin*` / `/widget*`
  only when non-nil. `setup.go` follows the same pattern, gated additionally on sentinel-absence.
- **The secrets resolver chain is M6.1's.** `config.ResolveSecret("NOVA_X", "NOVA_X_FILE",
  "/run/secrets/x")` (used at `cmd/coordinator/main.go:309` for the OIDC seed) is where staged
  secret files are found. The wizard stages master key / swarm key / OIDC seed at the resolver's file
  paths so a normal boot picks them up with no env editing.
- **nginx already has the building blocks** (`nginx/nova.conf.example`): rate-limit zones, the content
  cache, the streamed-upload location (`proxy_request_buffering off`), the hermetic CSP, and
  `location /admin` / `location /widget`. M13 **splits** this single `server` block into two and
  templates the host/cert/upstream placeholders — it does not invent new directives.
- **The integration harness is reusable.** `startCoordinatorWithNginxCfg`
  (`m10_master_key_rotation_test.go:279`), `startNginxM11` (`m11_admin_spa_test.go:223`), `seedAuthUser`,
  and the testcontainers Postgres are the primitives the M13 nginx-fronted test composes for the
  two-vhost split + sentinel-flip assertions.
- **The hermetic gate is dir-parametrized** (`scripts/hermetic-spa.sh`): reused as
  `hermetic-spa.sh web/setup/dist`.

## Architecture

```
internal/setup/   UI-agnostic domain core (shared by web wizard + novactl)
  answers.go       Answers model + per-step Validate (reuses config.validate floors)
  keygen.go        CSPRNG master key / swarm key / Ed25519 seed + fingerprint(first 8 bytes hex)
  render.go        Answers -> operator.yaml (self-validated via LoadFromBytes) + nova.conf
  tls.go           per-mode: dev-self-signed CA/leaf, static validate, http-01 webroot, dns-01/onion handoff
  commit.go        stage secrets(0600) -> write config -> CreateUser -> sentinel LAST -> re-exec

internal/api/handlers/
  setup.go         /setup/* JSON API + web/setup static (nil-gated; sentinel-absent only; loopback)

internal/api/server.go    ServerConfig.Setup *SetupHandler; mount /setup* (nil-gated)
pkg/coordinator/...        SetupConfig{DistDir, SentinelPath}; reduced "setup mode" boot
cmd/coordinator/main.go    config.LoadFromFile (canonical) + env overrides; sentinel-gated setup vs normal
cmd/novactl/  setup        --interactive | --config-file answers.yaml (shares internal/setup)
cmd/setup-wizard/          thin alias -> coordinator setup mode (no duplicate DB/keystore boot)

web/setup/        hermetic React+Vite wizard SPA (mirrors web/admin pins; hermetic-spa gate)
  src/Wizard.tsx   linear stepper + master-key readback gate + "you're live" orientation page

docker/
  Dockerfile               multi-stage: go-builder -> node-builder -> Debian-slim glibc runtime (non-root)
  docker-compose.yml       postgres + coordinator + nginx; profiles: setup, prod
  init/entrypoint.sh       migrate -> sentinel check -> exec (setup mode vs normal mode)
  nginx/
    nova.conf.template     two-vhost (public_host + admin_host) — wizard-rendered
    bootstrap.conf         /setup/* only — first-run

nginx/nova.conf.example    Phase-0 single-origin reference -> updated to a two-vhost reference
```

### Package boundaries

| Unit | Responsibility | Depends on |
|---|---|---|
| `internal/setup/answers.go` | the answer model + per-step validation | `internal/config` (floors) |
| `internal/setup/keygen.go` | CSPRNG key material + fingerprint | `crypto/rand`, `crypto/ed25519` |
| `internal/setup/render.go` | `operator.yaml` + `nova.conf` rendering (self-validated) | `internal/config`, `text/template` |
| `internal/setup/tls.go` | per-mode cert generation / validation / handoff text | `crypto/x509`, `render` |
| `internal/setup/commit.go` | atomic finalize (secrets → config → user → sentinel) | `keygen`, `render`, `password`, `gen` |
| `internal/api/handlers/setup.go` | `/setup/*` JSON API + static; nil + sentinel gated | `internal/setup`, `net/http` |
| `pkg/coordinator` | reduced setup-mode boot; build the setup handler | `handlers`, `internal/setup` |
| `cmd/coordinator` | load `operator.yaml`; env overrides; sentinel branch | `config`, `coordinator` |
| `cmd/novactl setup` | interactive / file-driven headless setup | `internal/setup` |
| `web/setup` | the stepper UI; drives the `/setup/*` API | (build-time only) |

Each unit answers the three isolation questions: `keygen.go` *makes key material* (use:
`GenerateMasterKey()`; depends on `crypto/rand`); `render.go` *turns answers into config files* (use:
`RenderOperatorYAML(ans)`, which fails if the output won't load); `commit.go` *finalizes first-run*
(use: `Commit(ctx, ans, paths)`; depends on `keygen`+`render`+`password`+`gen`). The split keeps the
crash-sensitive commit ordering testable in isolation from the DOM/stepper and from the coordinator
boot path.

## The first-run lifecycle (the load-bearing logic)

```
docker compose --profile setup up
   │
   ├─ postgres up
   ├─ coordinator: entrypoint.sh
   │     ├─ run migrate (forward-only; bootstraps master_key_versions on first normal boot, not here)
   │     ├─ test -f $NOVA_CONFIG_DIR/.bootstrap-complete ?
   │     │      ABSENT  ─►  exec coordinator (SETUP MODE: DB pool + /setup/* only;
   │     │                   NO keystore / Kubo / auth / upload / audits)
   │     │      PRESENT ─►  exec coordinator (NORMAL MODE: full boot; /setup -> 404)
   │     └─
   └─ nginx: ABSENT -> bootstrap.conf (proxy /setup/* only, on loopback :8444)
             PRESENT -> wizard-rendered nova.conf (two-vhost)

WIZARD COMMIT (POST /setup/commit), ordered for crash-safety:
   1. stage secrets (master-key-<label>, swarm.key, oidc-signing-key) into secrets volume, mode 0600
   2. write operator.yaml + nova.conf into config volume          (render.go, self-validated)
   3. INSERT users(role='operator') with argon2id hash            (password + gen.CreateUser)
   4. write .bootstrap-complete                                   (LAST — the point of no return)
   5. signal re-exec (SIGTERM self; restart policy brings it back in normal mode)
```

Why this shape:

- **Setup mode is a *reduced* boot.** While the sentinel is absent the coordinator must run *without* a
  master key (it doesn't exist yet), so it opens only the DB pool (needed for `CreateUser`) and mounts
  only `/setup/*`. It does **not** boot the keystore, Kubo, auth, the upload pipeline, or the audit
  scheduler. This is why setup mode and normal mode are two branches of one binary rather than a
  second long-lived service.
- **Sentinel-last is the crash-safety invariant.** Every prior step is idempotent (staging overwrites,
  config render is deterministic, `CreateUser` is guarded against duplicates). The sentinel is the
  *only* irreversible signal; writing it last means a crash at any earlier point re-enters setup mode
  cleanly on the next boot. A half-written sentinel must never exist — it is written with a single
  atomic `os.WriteFile` of a tiny payload (a timestamp + the schema/version stamp) and `fsync`.
- **Re-arming is deliberate.** Deleting `.bootstrap-complete` and restarting re-enters setup mode
  (the documented "redo setup" recovery path). This is why the sentinel lives on the **config** volume
  (operator-visible), not buried in application state.

## Configuration reconciliation (`operator.yaml` canonical + env override)

`cmd/coordinator` gains a precedence chain. `NOVA_CONFIG_FILE` (default
`$NOVA_CONFIG_DIR/operator.yaml`) is loaded via `config.LoadFromFile` when present; the resulting
`*config.Config` seeds `coordinator.Config`; then the existing `NOVA_*` env reads run **as overrides**
(an env var, when set, wins). Secrets are never in `operator.yaml` — they stay on the M6.1 resolver.

| Concern | `operator.yaml` (canonical) | Env override | Secret (resolver) |
|---|---|---|---|
| operator identity / hostname | `operator.*` | — | — |
| TLS mode + static paths | `tls.*` | — | — |
| auth mode (local vs external OIDC) | `auth.issuer_url` / `client_id` | `NOVA_AUTH_ISSUER_URL` … | OIDC seed / client secret |
| public uploads + ToS | `uploads.public_uploads`, `tos_url` | `NOVA_PUBLIC_UPLOADS`, `NOVA_TOS_URL` | — |
| paranoid / source-IP | `auth.paranoid`, `source_ip_retention_days` | `NOVA_PARANOID` | — |
| moderation defaults | `moderation.*` | — | — |
| `public_ipfs_dht` | `coordinator.public_ipfs_dht` | — | swarm key |
| upload limits | `uploads.max_upload_size_bytes` … | `NOVA_MAX_UPLOAD_SIZE_BYTES` … | — |
| **M7–M12 tuning** (signed-URL, rotation, sweeps) | *(deferred — env only)* | `NOVA_SIGNED_URL_*`, `NOVA_MASTER_KEY_REWRAP_*`, `NOVA_*_SWEEP_*` | — |
| DB / Kubo repo / listen / dist dirs | — | `DATABASE_URL`, `NOVA_KUBO_REPO`, `NOVA_LISTEN_ADDR`, `NOVA_*_DIST_DIR` | — |
| master key (active label) | — | `NOVA_MASTER_KEY_ACTIVE` | `NOVA_MASTER_KEY_<LABEL>` / file |

This keeps the change cheap (no churn across the M7–M12 knobs, which never had `operator.yaml` decode),
honors back-compat (every existing env-driven test/deploy still works), and gives the wizard a single
canonical artifact (`operator.yaml`) to render.

## The two-vhost split (nginx-only)

`nova.conf.template` renders two `server` blocks fed by the same `nova_coordinator` upstream. The
split is the boundary-① enforcement point:

| Location | public_host | admin_host | both |
|---|---|---|---|
| `/health`, `/readyz` | ✓ | ✓ (probe) | |
| `/blob/*`, `/i/*` (cached, immutable) | ✓ | | |
| `/legal/*` (DMCA intake) | ✓ | | |
| `/api/v1/uploads*`, `/api/v1/blobs`, `/api/v1/images` (streamed) | ✓ | | |
| `/widget*` (end-user uploader bundle) | ✓ | | |
| `/admin*` (admin SPA) | | ✓ | |
| `/api/v1/admin/*` (admin JSON API) | | ✓ | |
| `/api/v1/auth/*` (local issuer / OIDC discovery) | | ✓ | |
| `/api/v1/users/me` | | ✓ | |
| `/metrics` | | | ACL (loopback) |
| `/fed/*` | | | `404` (never public) |
| everything else | `404` | `404` | |

Default published ports follow the master-plan topology: public_host HTTPS `:8443` (HTTP `:8442`
redirect + ACME), admin_host HTTPS `:8445` (loopback-bound by default; operator opens it via the
compose override), setup `:8444` (loopback, `setup` profile only). The coordinator keeps its single
mux on `:9000`; **nginx**, by routing each vhost to only its allowed locations, is what makes a
public-origin compromise unable to reach `/api/v1/admin/*` or `/api/v1/auth/*`. An admin/auth path
requested against public_host returns `404` because that `server` block has no such location.

## TLS modes

| Mode | M13 behavior |
|---|---|
| `dev-self-signed` | `tls.go` generates a CA + leaf (SAN = `operator.hostname`) into the config volume `tls/`; `nova.conf` points at them. Phase-1 dev default. |
| `static` | Operator drops PEMs and gives `tls.cert_path`/`key_path`; `tls.go` validates they exist + parse; `nova.conf` points at them. |
| `http-01` | `nova.conf` renders the `/.well-known/acme-challenge/` webroot location + the `prod`-profile **certbot** service obtains/renews; nginx reloads on renewal. CT-log disclosure shown in the wizard. |
| `dns-01` | Config rendered; wizard prints **operator-handoff** steps (provide DNS credentials, run the documented certbot DNS plugin out-of-band). No plugin bundled in M13. |
| `onion` | Config rendered for a `.onion` vhost; wizard prints **operator-handoff** steps (run Tor, supply the self-signed cert). No Tor bundled in M13. |

## The wizard host-facing surface

The web wizard, the headless CLI, and the manual path all converge on `internal/setup`:

- **Web** (`docker compose --profile setup up`): the loopback `:8444` SPA drives `GET /setup/state` →
  `POST /setup/keys/master` (display hex + fingerprint, offer the backup `.txt`) → readback gate →
  `POST /setup/answers` (validate) → `POST /setup/commit`. The orientation page renders after a `200`
  commit.
- **Headless** (`novactl setup`): `--interactive` reproduces the steps as terminal prompts (the
  fingerprint readback is a typed confirmation); `--config-file answers.yaml` runs the whole commit
  non-interactively for scripts/CI. Both call the same `internal/setup` functions the handler does.
- **Manual / skip**: pre-place `operator.yaml` + staged secrets + `.bootstrap-complete` → boot straight
  into normal mode. The wizard is default, not mandatory.

## Security and privacy considerations

- **The wizard is loopback-only and self-disabling.** `/setup/*` is mounted only while the sentinel is
  absent and is reachable only via the loopback-bound `:8444` port (published solely under the `setup`
  profile). After commit it is gone (`404`) and stays gone unless the operator deliberately re-arms.
  Boundary ①a holds in code, not just in docs.
- **Secrets never touch `operator.yaml`, the DOM, or logs.** The master key is displayed once
  (plainly, for backup) and staged to a `0600` file on the secrets volume; the swarm key and OIDC seed
  are staged, never shown. `operator.yaml` carries only non-secret decisions. The forced-fingerprint
  readback proves the operator captured the backup before the key can ever be lost.
- **The two-vhost split is a real privilege boundary.** Per boundary ①, a public-origin XSS or
  cache-poison cannot reach the admin/auth API because those locations do not exist on public_host.
  The admin host is loopback-by-default; the operator opens it deliberately.
- **The startup validator floors are unchanged and now wizard-fed.** The wizard cannot render an
  `operator.yaml` that the runtime would refuse — `render.go` round-trips through the same `validate`
  the coordinator runs at boot (T1.20 ToS, `tls.mode`, anonymous+no-moderation refusal, etc.), so the
  wizard surfaces the violation at Review rather than as a post-restart crash-loop.
- **Non-root, glibc, no telemetry.** The runtime image runs as a non-root UID (a startup floor) on
  Debian-slim (glibc, required by `govips`); the bundles (admin/widget/setup) remain hermetic — the
  CI `hermetic-spa` gate now also covers `web/setup/dist`.
- **`http-01` privacy cost is disclosed.** The wizard names the CT-log disclosure of the hostname
  before the operator chooses `http-01`, consistent with `PRIVACY_AUDIT.md` and the
  `OPERATOR_CHECKLIST.md` posture.

## Exit criteria

1. `docker compose --profile setup up` on a fresh checkout boots postgres + the coordinator (setup
   mode) + nginx (`bootstrap.conf`); `GET http://127.0.0.1:8444/setup/` serves the wizard; the
   master-key step refuses to advance until the typed fingerprint matches; the downloaded backup `.txt`
   contains exactly the displayed hex + fingerprint and nothing else.
2. Completing the wizard renders a valid `operator.yaml` (round-trips through `config.LoadFromBytes`)
   and a two-vhost `nova.conf`, creates the operator user, stages secrets at mode `0600`, writes
   `.bootstrap-complete` **last**, and the coordinator restarts in **normal mode** with `/setup` →
   `404`. Deleting the sentinel + restarting re-arms the wizard.
3. The rendered nginx enforces the split: an `/api/v1/admin/*` or `/api/v1/auth/*` request on
   public_host → `404`; a `/blob/*` request on admin_host → `404`; `/fed/*` → `404` on both. Proven in
   the nginx-fronted integration test.
4. `novactl setup --config-file answers.yaml` performs the identical commit headlessly (no browser);
   a manual operator who pre-places config + secrets + sentinel boots normally without touching the
   wizard.
5. TLS: `dev-self-signed` generates a working CA+leaf (TLSv1.3 via `openssl s_client`); `static` picks
   up operator PEMs; `http-01` renders the certbot webroot + ACME location under the `prod` profile;
   `dns-01`/`onion` render config + print operator-handoff instructions.
6. `config.LoadFromFile` is wired into `cmd/coordinator` as the canonical non-secret source with env
   overrides preserved; secrets stay file/env mounted; the existing env-driven integration tests stay
   green; `make` build + `web/setup` `hermetic-spa` + the CI lanes are green.

## Testing strategy

### Go unit
- **`internal/setup/keygen`:** master key is 32 bytes / 64 hex chars; swarm key matches the Kubo PSK
  format; Ed25519 seed is valid; the fingerprint is the first 8 bytes of the master key, hex; two
  generations differ (CSPRNG).
- **`internal/setup/answers` + `render`:** `Validate` rejects the same shapes `config.validate` does
  (missing hostname/contact, bad `tls.mode`, `static` without cert paths, public uploads without ToS);
  `RenderOperatorYAML` output round-trips through `config.LoadFromBytes`; `RenderNginx` emits two
  `server` blocks with the correct per-mode TLS directives and the boundary-① route map.
- **`internal/setup/commit`:** ordering — secrets and config are written **before** the sentinel; a
  simulated failure after step 3 leaves no sentinel (re-enters setup mode); `CreateUser` is invoked
  with `role='operator'` and an argon2id hash; staged secret files are mode `0600`.
- **`internal/api/handlers/setup`:** `state`/`keys/master`/`answers`/`commit` behave; the handler is
  nil (⇒ `/setup/*` `404`) when the sentinel is **present**; CSP + loopback posture asserted.

### Frontend (Vitest, jsdom)
- The stepper advances only when each step validates; the **master-key readback gate** blocks `Commit`
  until the typed fingerprint equals the generated one; the backup download contains exactly hex +
  fingerprint; the orientation page renders the embed snippet + admin URL after a `200` commit.

### Integration (`internal/integration/m13_setup_wizard_test.go`, nginx-fronted, testcontainers)
- **Two-vhost split:** boot the coordinator (normal mode) behind a rendered two-vhost `nova.conf`;
  assert admin/auth routes `404` on public_host, public routes `404` on admin_host, `/fed/*` `404` on
  both. (Reuses `startCoordinatorWithNginxCfg` / `seedAuthUser`.)
- **Sentinel flip:** boot with the sentinel **absent** → `/setup/state` `200`, a steady-state route
  `404`; run a commit; boot with the sentinel **present** → `/setup/*` `404`, steady-state routes
  served.
- `-short`-skippable like M2–M12; gofmt only the Go files M13 touches (toolchain-skew rule);
  `golangci-lint`/`eslint` are CI-side.

### CI
- New `web-setup` steps (or an extension of `web-admin`): `make setup-{lint,test,build} &&
  hermetic-spa web/setup/dist`; `web/setup` added to the root `workspaces`; `npm ci` green on Node 20.
- A `docker build` lane that builds the multi-stage image (no push; signing is M14).

## File structure

### Created in M13
```
internal/setup/answers.go                          Answers model + per-step Validate
internal/setup/keygen.go                           CSPRNG master/swarm/Ed25519 + fingerprint
internal/setup/render.go                           operator.yaml + nova.conf rendering (self-validated)
internal/setup/tls.go                              per-mode cert gen / validate / handoff
internal/setup/commit.go                           atomic finalize (secrets→config→user→sentinel)
internal/setup/*_test.go                           keygen / answers / render / commit unit tests
internal/api/handlers/setup.go                     /setup/* JSON API + static (nil + sentinel gated)
internal/api/handlers/setup_test.go
internal/integration/m13_setup_wizard_test.go      nginx-fronted two-vhost + sentinel-flip
web/setup/package.json                             @nova/setup; web/admin pins (Vite 4.5 / Vitest 0.34)
web/setup/vite.config.ts                           hermetic SPA build; base '/setup/'
web/setup/tsconfig.json                            mirrors web/admin
web/setup/index.html
web/setup/src/Wizard.tsx                           stepper + readback gate + orientation page
web/setup/src/api/client.ts                        drives /setup/* (mirrors web/admin client)
web/setup/src/*.test.ts                            Vitest: stepper, readback gate, orientation
docker/Dockerfile                                  multi-stage: go-builder → node-builder → Debian-slim glibc (non-root)
docker/init/entrypoint.sh                          migrate → sentinel check → exec (setup vs normal)
docker/nginx/nova.conf.template                    two-vhost (public_host + admin_host) — wizard-rendered
docker/nginx/bootstrap.conf                        /setup/* only — first-run
docs/superpowers/specs/phase1/2026-06-08-phase1-m13-setup-wizard-design.md   (this file)
docs/superpowers/plans/phase1/2026-06-08-phase1-m13-setup-wizard.md          (the implementation plan)
```

### Modified in M13
```
cmd/coordinator/main.go            load operator.yaml (canonical) + env overrides; sentinel-gated setup mode
cmd/setup-wizard/main.go           thin alias → coordinator setup mode (replaces the .gitkeep)
cmd/novactl/main.go                add `setup` subcommand (--interactive | --config-file)
internal/api/server.go             ServerConfig.Setup *SetupHandler; mount /setup* (nil-gated)
pkg/coordinator/coordinator.go     SetupConfig{DistDir, SentinelPath}; reduced setup-mode boot; build setup handler
docker/docker-compose.yml          add coordinator + nginx services; profiles setup, prod; per-concern volumes
docker/.env.example                NOVA_* runtime knobs + POSTGRES_PASSWORD guidance
nginx/nova.conf.example            single-origin → two-vhost reference (public_host + admin_host)
Makefile                           setup-{install,build,lint,test}; docker build target; web aggregate
.github/workflows/ci.yml           web-setup lane (lint/test/build/hermetic) + docker build lane
package.json / package-lock.json   add web/setup to workspaces
docs/ROADMAP.md                    M13 status + tag + deferrals (reconciliation #1)
docs/superpowers/specs/phase1/2026-05-25-phase1-single-node-mvp-design.md   M13 status/links (reconciliation #2)
docs/THREAT_MODEL.md               boundaries ① / ①a implemented (reconciliation #3)
docs/specs/openapi.yaml            note-only: ephemeral /setup/* surface (reconciliation #4)
docs/legal/OPERATOR_CHECKLIST.md   first-run runbook (reconciliation #5)
README.md                          production-status note → setup quickstart pointer (reconciliation #6)
```

### Reused unchanged
```
internal/config/{operator_yaml.go,types.go,validate}   the loader + floors the wizard renders against
internal/auth/password + internal/db/gen (CreateUser)  admin-user creation
internal/api/handlers/{admin_spa.go,widget_static.go}  the nil-gated static-seam pattern setup.go mirrors
scripts/hermetic-spa.sh                                 the dir-parametrized hermetic gate (web/setup/dist)
internal/integration (harness)                          startCoordinatorWithNginxCfg, seedAuthUser, dbtest
nginx/nova.conf.example                                 the location/zone/cache/CSP basis for the two vhosts
web/admin/{package.json,vite.config.ts,tsconfig.json}   the hermetic-SPA build conventions web/setup mirrors
```

## Risks and notes

- **Setup mode is a reduced boot, not the full coordinator (decided).** Anyone extending M13 must keep
  setup mode from booting the keystore/Kubo/auth — the master key does not exist yet during first-run.
  Mounting the full route set in setup mode would crash on the missing master key.
- **Sentinel-last is the safety invariant (decided).** Reordering the commit so the sentinel is written
  before secrets/config/user would create a state where a crash leaves a "complete" node with no usable
  key — the worst failure mode. The sentinel write is the single irreversible step and goes last.
- **`operator.yaml` precedence must preserve env back-compat (accepted).** The wiring loads the file as
  the base and applies env as overrides; the M7–M12 tuning knobs intentionally stay env-only. A future
  milestone can deepen the decode, but M13 must not regress the existing env-driven tests/deploys.
- **CI cannot exercise real ACME/Docker-runtime end-to-end (accepted).** The two-vhost split and the
  sentinel flip are proven with the nginx testcontainer + in-process coordinator (no Docker image
  required); `dev-self-signed`/`http-01-staging`/manual restart live in the human-action checklist;
  the full CI e2e smoke is M14. M13's `docker build` lane proves the image *builds*, not that ACME
  succeeds.
- **`operator.yaml` field-naming is the documented "humans will bikeshed this" risk** (master plan
  § "What requires your decision"). M13 freezes the names the wizard renders; they match the existing
  `internal/config` struct tags, so no new naming is invented — the wizard renders what the loader
  already parses.
- **govips/glibc forces Debian-slim (resolved in M5).** distroless/alpine/musl are unsuitable for the
  runtime image; this is a known constraint, not an M13 discovery.

## Cross-references
- `docs/ROADMAP.md` M13 row + the master plan (`2026-05-25-phase1-single-node-mvp-design.md`)
  § "Onboarding wizard" (`:722`–`:777`), § "Container topology" (`:56`–`:133`), and § "M13" (`:976`–`:981`).
- `docs/THREAT_MODEL.md` boundaries ① / ①a (`:59`–`:60`) — the two-vhost split + loopback-only,
  self-disabling wizard this milestone implements.
- `internal/config/operator_yaml.go` + `types.go` — the loader + floors `render.go` validates against.
- `cmd/coordinator/main.go` — the env surface reconciled with `operator.yaml`.
- M11 design (`2026-06-04-phase1-m11-admin-spa-design.md`) + M12 design
  (`2026-06-07-phase1-m12-upload-widget-design.md`) — the hermetic-SPA discipline, the nil-gated
  static seam (`admin_spa.go`/`widget_static.go`), and the nginx-fronted integration pattern M13
  mirrors.
- M6 design (`2026-05-30-phase1-m6-auth-design.md`) + M6.1 — the local-issuer/external-OIDC mode
  selection and the secrets resolver chain the wizard stages into.
- `docs/superpowers/plans/phase1/2026-06-08-phase1-m13-setup-wizard.md` — the implementation plan.
```
