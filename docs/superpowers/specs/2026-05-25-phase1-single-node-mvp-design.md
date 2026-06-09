# Phase 1 — Single-node MVP Design

Status: **design** (spec floor: Phase 0 v3.1, see
`docs/REVIEW_2026_05_25.md`). Implementation plan will be generated
from this design under the writing-plans skill once approved.

Authors: Bug Plowman (operator), Claude (implementation partner).

## Purpose and scope

Phase 1 ships a runnable, single-host, single-operator Nova
deployment that honors every v3.1 spec commitment and is friendly
enough to onboard non-technical operators in one Docker command
while remaining transparent and tunable for technical operators.
It deliberately does **not** ship federation, donor binaries,
possession audits, automated PDQ scanning, NCMEC reporting, or
adapter SDKs — those are Phase 2+ work.

Phase 1 implements:

- Coordinator process (`cmd/coordinator`) with in-process embedded
  Kubo, the full Phase 1 HTTP API surface from
  `docs/specs/openapi.yaml`, hardened startup-validator refuse-to-
  start floors, and a worker pool consuming a Postgres-backed job
  queue.
- The `nova-image` product layer: govips-driven transforms, PDQ
  perceptual hashing, image-specific upload validation. No
  blocklist scan in Phase 1 (manual moderation only).
- v1 envelope encryption (single-shot XChaCha20-Poly1305) with a
  versioned `Codec` interface so v2 streaming-AEAD drops in at
  Phase 2 without disturbing v1 paths.
- Deterministic IPFS import per `IPFS_IMPORT_RULES.md`.
- Signed-URL HMAC with structured revocation per
  `SIGNED_URL_FORMAT.md`.
- Master-key versioning + online rotation via
  `novactl keys rotate-master`.
- All seven local integrity audits per `INTEGRITY_AUDIT.md`,
  scheduled through the job queue.
- Quarantine-first DMCA flow with scheduled tombstone job. Manual
  severe-content path (`novactl moderation quarantine --legal-hold`).
- Drag-and-drop upload widget (`web/widget`, Uppy + tus.io).
- Admin SPA (`web/admin`, React + Vite, hermetic build).
- Ephemeral first-run setup wizard (`web/setup`).
- Local OIDC-shaped JWT issuer for management auth, with external-
  OIDC swap-in via config.
- Docker-first deployment: one-command bootstrap, separate
  containers for coordinator / nginx / postgres / (optional)
  certbot, separate volumes per concern, file-mount-friendly
  secrets.

Phase 1 deliverable: a `docker compose up` that produces a working
operator deployment, with the wizard guiding first-run through key
generation + backup confirmation + TLS-mode choice + ToS URL +
admin login.

## Architecture overview

### Container topology

```
                ┌──────────────────────── host ──────────────────────────┐
                │  Internet                                              │
                │      │                                                 │
   public ─────►│  host:8443  nginx HTTPS  — public_host                 │
                │  host:8442  nginx HTTP   — redirects to 8443           │
                │                                                        │
   admin  ─────►│  host:8445  nginx HTTPS  — admin_host                  │
                │              (default 127.0.0.1:8445, deliberately     │
                │               loopback-only; operator opens to LAN     │
                │               via compose override)                    │
                │                                                        │
   setup  ─────►│  host:8444  setup wizard (loopback-only, only when     │
                │              .bootstrap-complete is absent; lives in   │
                │              the coordinator container)                │
                │                                                        │
                │  ┌──────────────── nova network ─────────────────────┐ │
                │  │                                                   │ │
                │  │  nginx        nginx:1.25-alpine                   │ │
                │  │               serves SPA bundles from /usr/share  │ │
                │  │               proxies /api, /blob, /i, /legal,    │ │
                │  │                       /admin, /setup to coordinator│ │
                │  │                                                   │ │
                │  │  coordinator  nova-coordinator:phase1             │ │
                │  │               API on :9000 (collision-avoiding)   │ │
                │  │               in-process Kubo:                    │ │
                │  │                  127.0.0.1:5001 (Kubo API)        │ │
                │  │                  127.0.0.1:8080 (Kubo Gateway,    │ │
                │  │                                  diagnostic only) │ │
                │  │               job worker pool (in-process)        │ │
                │  │                                                   │ │
                │  │  postgres     postgres:16-alpine                  │ │
                │  │               loopback inside nova network only  │ │
                │  │                                                   │ │
                │  │  certbot      prod profile only; ACME runner      │ │
                │  └───────────────────────────────────────────────────┘ │
                │                                                        │
                │  Named volumes (separate blast radius per concern):    │
                │    nova-postgres-data/   postgres data dir             │
                │    nova-kubo-data/       Kubo blockstore + datastore   │
                │    nova-config/          operator.yaml, nginx.conf,    │
                │                          tls/ certs, .bootstrap-complete│
                │    nova-secrets/         master-key, signing seed,     │
                │                          local-issuer signing key      │
                │    nova-tmp-uploads/     tus chunks                    │
                └────────────────────────────────────────────────────────┘
```

Key topology points (all justified in `docs/REVIEW_2026_05_25.md`):

- **One image per concern, one-command UX.** `docker compose up` is
  still the operator surface. The internals are unbundled enough
  that a CVE in the coordinator can be patched independently of the
  nginx upgrade cadence, and an OOM in Kubo doesn't take down the
  postgres process.
- **Coordinator API on :9000.** Inside the container, the
  coordinator binds to `:9000`. Kubo's hardened loopback Gateway
  stays on `127.0.0.1:8080` (its mandated port per
  `KUBO_HARDENING.md`); the Kubo Gateway is diagnostic-only and is
  never published outside the container. Avoids the 8080
  collision the v3 design had on the user's host.
- **Public vs admin origins on separate ports.** Default published
  ports: public on 8443, admin on 8445, wizard on 8444. The host's
  port 80/443 stay free; the operator can wire DNS later. The
  admin port binds to `127.0.0.1` by default; the operator opens
  it deliberately by editing `docker-compose.override.yml`.
- **Volumes scoped to blast radius.** Postgres, Kubo, config,
  secrets, and tmp-uploads are separate named volumes — different
  backup cadences (postgres = daily WAL or `pg_dump`; Kubo =
  weekly snapshot; secrets = paper backup off-host).
- **In-process worker pool** (landed in M5): the coordinator starts
  the job worker pool in `Run` on startup. The `derivative_prewarm`
  job kind is live; other job kinds follow in later milestones.

> **Reconciliation (M13, implemented).** This topology shipped as drawn — published
> ports 8442:80, 8443:443, 127.0.0.1:8445:8445, wizard 127.0.0.1:8444; `setup` + `prod`
> compose profiles; the setup wizard lives inside the coordinator container (setup mode
> is a reduced boot of the same binary). Two refinements: (1) `operator.yaml` (in the
> `nova-config` volume) is the **canonical** non-secret config source, wired into
> `cmd/coordinator` with the `NOVA_*` env vars preserved as **overrides** — not a
> wholesale env replacement (the M7–M12 tuning knobs stay env-only for now). (2) The
> wizard-generated secrets (master-key-`<label>`, swarm.key, oidc-signing-key) land in
> the `nova-secrets` volume at the M6.1 resolver file paths. The two-vhost split is
> nginx-only (the coordinator keeps its single mux). The runtime image is multi-stage
> Debian-slim/glibc, non-root via a `gosu` drop in `entrypoint.sh` (the in-process
> uid-0 floor named under "Network exposure floor" is deferred — non-root is enforced
> by the container today).

### Network exposure floor

The startup validator (per `KUBO_HARDENING.md` + `PRIVACY_AUDIT.md`)
refuses to start unless:

- `IPFS_SWARM_KEY` is set, OR `coordinator.public_ipfs_dht: true`.
- Kubo's `Bootstrap`, `Routing.Type`, `Discovery.MDNS.Enabled`,
  `Provider.Strategy`, `Reprovider.Strategy`, `Addresses.API`,
  `Addresses.Gateway`, and `Swarm.DisableNatPortMap` all satisfy
  the hardening table.
- The coordinator process is not running as UID 0 (root) inside
  the container.
- `auth: anonymous` and `moderation: off` are not both set.
- Public uploads enabled without `tos_url` set is refused.
- Master key loaded successfully (env var OR file mount).
- `.bootstrap-complete` sentinel is present (wizard mode otherwise).

Refuse-to-start with precise error messages naming the offending
key; the validator's failure output is the operator's first
debugging input.

## Repository layout

```
cmd/
  coordinator/
    main.go                  wires storage core, products, jobs, HTTP, IPFS,
                             auth, audits, wizard (if not yet bootstrapped)
    wire.go                  dependency wiring (manual DI, no fancy DI lib)
  novactl/
    main.go                  Cobra root command
    cmd/                     subcommands:
      keys.go                rotate-master, rotate-signing
      moderation.go          quarantine, takedown, clear-legal-hold
      dmca.go                create (from email), action
      auth.go                login, token, logout
      audits.go              run-now, list, kubo-pin-verify
      jobs.go                list, retry, pause, dead-letter
      setup.go               headless setup alternative to web wizard
  migrate/
    main.go                  embeds migrations from internal/db/migrations + nova-image/migrations
                             runs forward-only, bootstraps master_key_versions row v1 on first run

pkg/coordinator/             public Go library; semver-stable from Phase 1
  coordinator.go             Config, New, Run, RegisterProduct, Shutdown
  storage/                   storage core helpers product layers use
    blob.go                  Get, Decrypt, PutDerivative
    scanresult.go            ScanResult (Action, Rule, RuleRef, Notes)
    types.go                 Blob, UploadContext (re-exported)
  product/                   v0.x.y (unstable until Phase 4 adapters land)
    interface.go             Product interface (AnalyzeUpload, OnCommitted,
                             OnDelete, RegisterRoutes, Migrations)
    metadata.go              Metadata interface

internal/
  envelope/
    envelope.go              Codec interface, v1 single-shot codec
    decoder.go               version-byte dispatcher; v2 slot reserved
    keywrap.go               master-key wrap/unwrap
    envelope_test.go         golden vectors per spec
    testdata/vectors.txt
  ipfs/
    backend.go               Backend interface (AddDeterministic, Get, Has, Pin, Unpin, BlockstoreHas, BlockGet)
    embedded.go              in-process Kubo via coreapi
    validate.go              ValidateConfig (refuse-to-start hardening checks)
    importrules.go           deterministic Add options constants
    embedded_test.go
    validate_test.go
  auth/
    bearer.go                bearer middleware (chi)
    signedurl.go             HMAC verify + canonical string + revocation lookup
    localissuer/             OIDC-shaped JWT issuer (Phase 1 default)
      issuer.go              login, refresh, jwks endpoints
      tokens.go              access (15-min) + refresh (12-h)
      keys.go                local signing-key management
    oidc/                    external OIDC adapter (config-driven; off by default)
      verifier.go            OIDC JWKS-cached verifier
  db/
    migrations/              forward-only SQL; embedded with embed.FS
      0001_init.sql          DATA_MODEL.sql verbatim
      0002_jobs.sql          job queue table
      0003_partitions.sql    convert integrity_audits + audit_log to partitioned
      0004_envelope_version.sql  blob_manifests.codec / blobs.envelope_version surfaces
      ...
    queries/                 sqlc input
      blobs.sql, users.sql, keys.sql, nodes.sql, audits.sql, jobs.sql, ...
    sqlc.yaml
    gen/                     generated; gitignored
    db.go                    pgxpool setup
  api/
    server.go                chi router setup + middleware stack
    codegen/                 oapi-codegen output; gitignored
    handlers/
      blob.go                /blob/{cid} (GET, HEAD), /blob/{cid}.json
      upload.go              tus.io endpoints + multipart fallback
      collection.go          /api/v1/collections/*
      user.go                /api/v1/users/me
      health.go              /health
      admin_nodes.go         (Phase 1: returns empty list)
      admin_keys.go          rotate-signing, rotate-master endpoints
      admin_moderation.go    queue, takedown, blocklist
      admin_dmca.go          list, transition
      admin_audits.go        integrity audit listing
      admin_jobs.go          job queue introspection (Phase 1 addition)
      legal.go               /legal/dmca, /legal/dsar
      auth.go                /api/v1/auth/* (local issuer)
      setup.go               /setup/* (gated by .bootstrap-complete absence)
    middleware/
      requestid.go           X-Request-ID
      ratelimit.go           defense-in-depth (nginx is primary)
      recover.go             panic → 500 with no leaked stack
      audit_log.go           record privileged actions
      origin_split.go        admin host vs public host vhost discriminator
    errors.go                JSON error model from openapi.yaml
  jobs/
    queue.go                 enqueue, lease, ack, fail, retry-with-backoff
    worker.go                worker pool; in-process Phase 1, splittable later
    kinds/                   one file per job kind
      integrity_audit.go
      scheduled_tombstone.go
      derivative_prewarm.go
      signing_key_rotate.go
      master_key_rotate_row.go
      webhook_emit.go
  audit/
    integrity/
      scheduler.go           per-audit-kind cron; enqueues jobs
      kinds.go               seven check implementations
      report.go              writes integrity_audits row + metric
  moderation/
    quarantine.go            transaction: blobs.state, sig-URL revoke, decision row, audit
    tombstone.go             gated by legal_hold; scheduled-job handler
    cascade.go               parent → derivative state propagation
    severe_content.go        manual quarantine + legal-hold path
  setup/                     Phase 1 ephemeral wizard
    wizard.go                handlers behind /setup/* (mounted only when sentinel absent)
    keygen.go                master-key + swarm-key + admin OIDC client gen
    validator.go             confirm-readback enforcement (no submit until operator types fingerprint)
    tls.go                   mode selection (http-01, dns-01, static, .onion, dev-self-signed)
    finalize.go              writes config + sentinel; signals coordinator to re-exec into normal mode
  config/
    operator_yaml.go         loader with strict validation
    paranoid.go              hardens further when paranoid: true
    secrets.go               NOVA_FOO → NOVA_FOO_FILE → /run/secrets/foo resolver
    types.go
  webhook/
    sender.go                outbound webhook (config-gated; disabled in paranoid)
  ratelimit/
    bucket.go                token bucket; nginx is primary, this is defense-in-depth
  node/                      Phase 1 internal-only (v3.1 amendment; promotes to pkg/node in Phase 2)
    types.go                 NodeConfig, NodeStatus, PolicyFilters
    placeholder.go           docs: Phase 2 surface

nova-image/                  product module
  product.go                 implements pkg/coordinator/product.Product
  internal/
    transform/               govips wrapper, pipeline
      transform.go
      presets.go             thumb, og, hero defaults
    perceptualhash/          goimagehash PDQ + BK-tree
      hash.go
      bktree.go
    imageapi/                /i/* route handlers
      routes.go              register on chi router
      resize.go              /i/{cid}/wNxN.ext, /i/{cid}/wN.ext
      preset.go              /i/{cid}/p/{preset}.ext
      original.go            /i/{cid}, /i/{cid}.{ext}
    imagemoderation/
      scanner.go             Phase 1: pass-through (manual moderation)
                             Phase 3: PDQ vs StopNCII blocklist
    formatconv/              optional PNG→WebP transform on upload
      convert.go
  migrations/                forward-only SQL for image_metadata
    0001_image_metadata.sql
  product_test.go

web/
  widget/                    Uppy + tus.io drag-and-drop
    package.json
    vite.config.ts           hermetic build, hashed assets, no external CDN
    src/
      widget.tsx
      uploader.ts            uppy + tus configuration
      api.ts                 generated from openapi-typescript
      widget.css
  admin/                     React + Vite; default styling = Tailwind + headless UI
                             primitives (revisitable at M11; see "What requires
                             your decision during implementation")
    package.json
    vite.config.ts           hermetic build, strict CSP, no external CDN
    src/
      App.tsx
      auth/                  login (local-issuer or OIDC), refresh, logout
      blobs/                 list, view, delete (soft + crypto-shred)
      collections/           CRUD
      pins/                  list (Phase 1: shows the coordinator-as-self
                             when populated by Phase 2)
      moderation/            queue, action, DMCA cases, severe-content manual quarantine
      audits/                integrity audit failure browser
      keys/                  rotate-signing, rotate-master
      jobs/                  introspect job queue (stuck, failed, recent)
      settings/              operator.yaml subset that's safe to edit at runtime
  setup/                     small ephemeral one-page wizard
    package.json
    vite.config.ts
    src/
      Wizard.tsx             step-by-step: welcome → master key gen + backup
                             readback → swarm key gen → admin login setup →
                             TLS mode → ToS URL → paranoid? → confirm

docker/
  Dockerfile                 multi-stage:
                             1) go-builder: builds coordinator + novactl
                             2) node-builder: builds widget, admin, setup bundles
                             3) runtime: Debian-slim/glibc (required for govips/libvips cgo link);
                                non-root UID; copies binaries + static assets in
  Dockerfile.dev             extends runtime with `air` watcher + source mount
  docker-compose.yml         dev profile (no certbot; self-signed TLS)
  docker-compose.prod.yml    prod profile (certbot, real TLS, no wizard port)
  docker-compose.setup.yml   first-run overlay (exposes :8444 wizard)
  nginx/
    nova.conf.template       templated by the wizard; renders into nova.conf
                             two server blocks (public_host + admin_host)
    bootstrap.conf           served before .bootstrap-complete exists
                             (just /setup → coordinator)
  init/
    entrypoint.sh            runs migrate, checks .bootstrap-complete sentinel,
                             execs coordinator (or wizard mode)

nginx/                       legacy reference config from Phase 0
  nova.conf.example          stays as documentation reference

scripts/
  install.sh                 one-command bootstrap helper: pulls compose,
                             generates POSTGRES_PASSWORD if absent, opens
                             the wizard URL in the operator's browser.

.github/workflows/
  ci.yml                     test, vet, lint, sqlc-diff, oapi-codegen-diff,
                             hermetic-spa-lint, image-build, integration-tests
  release.yml                builds & pushes the image (sigstore signing
                             arrives in Phase 5)
```

This layout intentionally keeps `internal/` packages narrow (one
responsibility each), centralizes spec-vs-code drift detection in
CI, and isolates `nova-image` so future product modules drop in
adjacent without entangling.

## Data flows

### Upload pipeline (encrypted image, common case)

```
1. Browser (widget) opens tus session:
     POST /api/v1/uploads + Tus-Resumable + Upload-Length + Upload-Metadata
       (mime_type, product=image, collection_id)
     Authorization: Bearer <local-issuer JWT>
   nginx:
     enforces nova_uploads rate-limit zone (5 r/s burst 20)
     proxy_request_buffering off  (tus streams chunked)
   coordinator:
     auth middleware verifies JWT (local issuer kid OR external OIDC)
     creates upload session row, returns 201 + Location

2. Browser PATCHes chunks via tus to .../uploads/{id}
   Each chunk lands in nova-tmp-uploads/{id}/{offset}-{end}

3. Browser POSTs .../uploads/{id}/finalize
   coordinator:
     a) reassembles plaintext stream from chunks
     b) validates declared MIME against magic bytes; rejects mismatch
     c) routes to nova-image product based on declared product=image
     d) nova-image.AnalyzeUpload(plaintext):
        - decodes width/height (govips)
        - computes PDQ perceptual hash (goimagehash)
        - Phase 1 imagemoderation.Scan: pass-through (returns Action: allow)
        - if config.format_conversion enabled: re-encodes PNG/BMP/TIFF → WebP
        - returns Metadata{width, height, perceptual_hash}, transformedPlaintext
     e) internal/envelope.v1Codec.Encrypt(transformedPlaintext, per_blob_key):
        per_blob_key := CSPRNG(32)
        nonce := CSPRNG(24)
        ct, tag := XChaCha20-Poly1305-Encrypt(per_blob_key, nonce, plaintext)
        envelope := NOVE_header(v=1, algo=1, reserved=0) || nonce || ct || tag
     f) internal/ipfs.embedded.Backend.AddDeterministic(envelope):
        opts: CidVersion(1), Hash(sha2-256), RawLeaves(true),
              Chunker("size-262144"), Layout(BalancedLayout), Pin(true)
        returns cid + blob_blocks layout
     g) internal/envelope.keywrap.Wrap(per_blob_key, MK_active):
        wrap_nonce := CSPRNG(24)
        wrapped := XChaCha-Encrypt(MK, wrap_nonce, "", per_blob_key)
        wrapped_key := wrap_nonce || wrapped  (72 bytes)
     h) DB transaction:
          INSERT data_encryption_keys (wrapped_key, master_key_version_id, algorithm)
          INSERT blobs (cid, encryption_key_id, mime_type, byte_size, owner_id,
                        product='image', state='active')
          INSERT blob_manifests (cid, codec='chunked-aead-v0' if v1 OR raw/dag-pb,
                                 chunker='size-262144', plaintext_size, envelope_size,
                                 block_count, merkle_root)
          INSERT blob_blocks (rows for each block in DAG order)
          INSERT image_metadata (cid, width, height, perceptual_hash)
          COMMIT
        Atomic; failure rolls back the IPFS pin via deferred cleanup.
     i) ENQUEUE jobs:
          derivative_prewarm(cid, presets=[thumb, og])    # async OnCommitted work
          webhook_emit(image.created, {cid, ...})         # if webhooks configured

4. coordinator returns 200 + UploadResult JSON:
     { cid, byte_size, mime_type, product, urls: { original, json, presets: {} } }
     X-Nova-Envelope-Version: 1
```

### Read pipeline (anonymous public read)

```
1. Browser → nginx → coordinator: GET /blob/{cid}
   nginx:
     nova_per_ip rate-limit (30 r/s burst 120)
     proxy_cache lookup against nova_content (key includes Cache-Control)
     on HIT: serve from cache; nginx adds X-Cache-Status: HIT
     on MISS: forward to coordinator
   coordinator:
     a) blobs row lookup: state in (active) → 200; quarantined → 451;
        soft_deleted → 410 (per spec); tombstoned → 410
     b) collection visibility check:
          public          → no auth required
          unlisted        → no auth required (slug knowledge sufficient)
          private         → 401 (must use signed URL or bearer)
     c) data_encryption_keys lookup; state='shredded' → 410
     d) embedded.Backend.Get(cid) → io.Reader of envelope bytes
     e) keywrap.Unwrap(wrapped_key, MK_for_version)
     f) v1Codec.Decrypt(envelope, per_blob_key) → plaintext bytes
     g) Set headers:
          Content-Type: <stored mime>
          Cache-Control: per stratified policy (immutable for public,
                         private/max-age=300 for signed/private,
                         no-store for soft_deleted/quarantined/tombstoned)
          ETag: blobs.cid
          X-Nova-Cid: cid
          X-Nova-Envelope-Version: 1
     h) Stream plaintext to response.
   On Range request:
     v1 envelope → 416 Range Not Satisfiable (Phase 2 v2 will support).
     public_archival blob (cid is plaintext CID; no envelope) → normal Range OK.
```

### Image transform pipeline (cache miss derivative)

```
1. Browser → nginx → coordinator: GET /i/{parent_cid}/p/thumb.webp
   nginx: cache check (cache miss because derivative not yet rendered)
   coordinator → nova-image/imageapi:
     a) Look up blobs WHERE parent_cid=parent AND derivative_preset='thumb'
        AND derivative_format='webp'
     b) Hit: same as read pipeline (decrypt + stream the derivative).
        Miss: continue.

2. On miss:
     a) Read parent envelope from local Kubo, decrypt.
     b) govips transform: parent_plaintext → resized WebP plaintext.
     c) Encrypt as a fresh blob (fresh per_blob_key, fresh nonce).
     d) Import deterministic envelope to Kubo → derivative_cid.
     e) DB transaction:
          INSERT data_encryption_keys (...) for derivative
          INSERT blobs (cid=derivative_cid, parent_cid, derivative_preset='thumb',
                        derivative_format='webp', encryption_key_id, ...)
          INSERT blob_manifests, blob_blocks
          INSERT image_metadata (width, height, perceptual_hash=NULL)
          COMMIT
     f) Stream derivative plaintext to caller.

Subsequent requests for /i/{parent_cid}/p/thumb.webp hit the derivative cache.

A derivative_prewarm job is enqueued on parent upload to preheat the common
presets (thumb, og) before the first user-visible read.
```

### Integrity audit loop

```
scheduler (in-process goroutine, ticks every ~10s):
   lastRun[kind] seeded at boot from MAX(audited_at) per kind (resume mid-cadence)
   for each audit_kind that is due (and not already running):
     run the check INLINE in a bounded goroutine under a context timeout
       samples N rows from blobs / data_encryption_keys / blob_blocks
       runs the per-kind check (decode envelope, unwrap key, kubo pin check,
         block hash recompute, manifest count match, derivative-state cascade)
       batch-INSERT integrity_audits (cid, audit_kind, result, error)
       on fail: warn log + FailureSink (the deferred integrity.audit_failed seam)

   NB (reconciled vs INTEGRITY_AUDIT.md, normative): audits run in-process —
   NOT through the persistent jobs.Queue. There is no in-flight queue, so a
   restart resumes from each kind's natural cadence. The
   nova_integrity_audit_failures_total metric is deferred (no metrics surface yet).

partitioning + retention (Maintainer goroutine, at boot + every 24h):
   integrity_audits is RANGE-partitioned by audited_at (monthly).
   Create-ahead: current + next 2 months provisioned so inserts never hit an
     uncovered range (the committed partitions stop at 2026-07-01).
   Pass-row pruning: DELETE passes older than 30d.
   Failure rows retained 1y+ by dropping whole partitions once aged out.
```

### Master-key rotation

```
operator: novactl keys rotate-master --to-version v2

novactl → POST /api/v1/admin/keys/rotate-master {from: v1, to: v2}

coordinator handler:
  1. Verify NOVA_MASTER_KEY_V2 loaded in process env.
  2. BEGIN; INSERT master_key_versions (label=v2, state=active);
       UPDATE master_key_versions SET state='rotating' WHERE label=v1;
     COMMIT.
  3. Enqueue one master_key_rotate_row job per active row:
       SELECT id FROM data_encryption_keys
        WHERE master_key_version_id = <v1 id> AND state IN ('active', 'rotating')
        LIMIT batch  -- enqueue in batches; worker pool processes parallel
       Same for signing_keys.
  4. Worker picks up master_key_rotate_row(key_id):
       UPDATE data_encryption_keys SET state='rotating' WHERE id=$1;
       SELECT wrapped_key, master_key_version_id FROM ... WHERE id=$1;
       unwrap with MK_v1; re-wrap with MK_v2 (fresh wrap_nonce);
       UPDATE data_encryption_keys
          SET wrapped_key=$new, master_key_version_id=<v2>,
              state='active'
        WHERE id=$1;
  5. When job queue drains for this rotation:
       UPDATE master_key_versions SET state='retired', retired_at=now()
        WHERE label='v1';
  6. Operator removes NOVA_MASTER_KEY_V1 on next redeploy.

Read path during rotation: any row in state='rotating' uses whichever
master_key_version_id is currently set; reads do not block.
```

### DMCA quarantine flow

```
1. Notice arrives at POST /legal/dmca (or operator novactl dmca create from email).
   coordinator validates statutory elements; INSERT dmca_cases (status='received').
2. Moderator reviews via admin UI: GET /api/v1/admin/dmca, GET /admin/dmca/{id}.
   Moderator runs:
     novactl moderation quarantine <cid> --case <dmca_case_id> --tombstone-after 14d
   In one transaction:
     INSERT moderation_decisions (cid, rule='dmca', action='quarantine',
            scheduled_tombstone_at=now()+14d, legal_hold=false)
     UPDATE blobs SET state='quarantined'  (cascade via OnDelete to derivatives)
     INSERT signed_url_revocations (kind='cid', value=<cid>)
     UPDATE dmca_cases SET status='actioned', actioned_at=now()
     INCREMENT takedown_repeat_infringers strikes
     INSERT audit_log rows for each step
3. scheduled_tombstone job kind (runs every minute via scheduler):
     SELECT md.id, md.cid FROM moderation_decisions md
      JOIN blobs b ON b.cid=md.cid
      JOIN data_encryption_keys dek ON dek.id=b.encryption_key_id
     WHERE md.scheduled_tombstone_at < now()
       AND md.legal_hold = false
       AND dek.legal_hold = false
       AND b.state='quarantined'
   For each: run tombstone procedure (state → tombstoned, crypto-shred, audit).
4. Counter-notice arrives:
     UPDATE moderation_decisions SET scheduled_tombstone_at=NULL WHERE id=$;
     blob remains quarantined; operator decides restore or hold.
```

### Severe-content (manual Phase 1 path)

```
operator: novactl moderation quarantine <cid> --reason "..." --legal-hold

In one transaction:
  INSERT moderation_decisions (cid, rule='severe_content', action='quarantine',
         scheduled_tombstone_at=NULL, legal_hold=true)
  UPDATE blobs SET state='quarantined' (cascade to derivatives)
  UPDATE data_encryption_keys SET legal_hold=true WHERE id=<blob's key>
  INSERT signed_url_revocations (kind='cid', value=<cid>)
  INSERT audit_log entries

Crypto-shred: DB CHECK constraint refuses; scheduled_tombstone job refuses.
Operator clears via novactl moderation clear-legal-hold <cid> after the
statutory preservation window. That command runs:
  UPDATE data_encryption_keys SET legal_hold=false WHERE id=$
  UPDATE moderation_decisions SET legal_hold=false, scheduled_tombstone_at=now()
   WHERE id=$
The next scheduled_tombstone tick fires the standard tombstone procedure.
```

### Job lifecycle

```
jobs table:
  id           uuid PK
  kind         text NOT NULL (e.g., 'integrity_audit_run')
  payload      jsonb NOT NULL
  state        text NOT NULL  ('pending', 'leased', 'completed', 'failed', 'dead')
  attempts     int NOT NULL DEFAULT 0
  max_attempts int NOT NULL DEFAULT 5
  lease_until  timestamptz
  not_before   timestamptz NOT NULL DEFAULT now()   -- for backoff scheduling
  last_error   text
  created_at   timestamptz NOT NULL DEFAULT now()
  updated_at   timestamptz NOT NULL DEFAULT now()
  PARTITION BY RANGE (created_at) — monthly

Worker loop (one goroutine pool per worker process):
  every 250ms:
    BEGIN
    SELECT id, kind, payload, attempts FROM jobs
     WHERE state='pending' AND not_before <= now()
     ORDER BY created_at ASC
     LIMIT 1
     FOR UPDATE SKIP LOCKED;
    UPDATE jobs SET state='leased', lease_until=now()+30s WHERE id=$;
    COMMIT
    --
    run handler(payload)
    on success: UPDATE jobs SET state='completed';
    on retryable error: UPDATE jobs SET state='pending', attempts=attempts+1,
                          not_before=now()+exp_backoff(attempts), last_error=$
                          WHERE attempts < max_attempts;
                        else state='dead', last_error=$
    on timeout: lease expires; another worker picks up

Lease reclaim ticker (every 10s):
  UPDATE jobs SET state='pending' WHERE state='leased' AND lease_until < now();

Admin visibility:
  GET /api/v1/admin/jobs?state=dead    operator inspects stuck work
  POST /api/v1/admin/jobs/{id}/retry   force-requeue
```

## Authentication architecture

Two cooperating layers:

**Bearer middleware** (in `internal/auth`): verifies a JWT presented
in `Authorization: Bearer <token>` against a configurable set of
issuers. JWT verification is constant-time, uses the issuer's JWKS
endpoint, and accepts both the local issuer and (if configured) an
external OIDC provider concurrently — useful during migration.

**Local JWT issuer** (`internal/auth/localissuer`): a small,
OIDC-shaped service mounted at:

- `POST /api/v1/auth/login` — accepts `{username, password}`,
  returns `{access_token (15-min TTL), refresh_token (12-h TTL),
  token_type: bearer, kid, ...}`.
- `POST /api/v1/auth/refresh` — accepts `{refresh_token}`, returns
  a new access + refresh pair (rotation).
- `GET /api/v1/auth/jwks.json` — JWKS for verification.
- `POST /api/v1/auth/logout` — revokes the presented refresh
  token.

Local-issuer signing keys are stored under `nova-secrets/` and
loaded via the same resolver as the master key
(`NOVA_OIDC_SIGNING_KEY` → `NOVA_OIDC_SIGNING_KEY_FILE` →
`/run/secrets/oidc-signing-key`). Bootstrap key is generated by
the wizard.

The admin SPA implements PKCE-style login (operator types
credentials, browser stores access+refresh in localStorage with
auto-refresh). The CLI implements browser-redirect login
(`novactl auth login` opens a browser, captures the callback,
caches the JWT in `~/.config/nova/credentials.json` with
restrictive permissions).

External OIDC swap-in is a config change:

```yaml
auth:
  issuer_url: https://authelia.example.com  # if set, external is primary
                                            # and local issuer is disabled
  client_id: nova
  client_secret_file: /run/secrets/oidc-client-secret
  scopes: [openid, profile, email, nova:operator, nova:moderator]
  jwks_cache_ttl_seconds: 600
```

When external OIDC is on, the local-issuer endpoints
(`/api/v1/auth/{login,refresh,logout,jwks.json}`) return
`404 external_oidc_active` and clients discover the IdP via the
always-served `GET /api/v1/auth/config`, then drive PKCE themselves.
This was clarified in the M6 design — see
`docs/superpowers/specs/2026-05-30-phase1-m6-auth-design.md`
§ "Mode selection" — superseding the earlier "SPA/CLI redirect" wording.

## Onboarding wizard

> **Reconciliation (M13, implemented — tag `m13-setup-wizard`).** Setup mode is
> *folded into the coordinator boot path* (`coordinator.RunSetupServer`, sentinel-gated
> in `cmd/coordinator`), not a second long-lived binary — `cmd/setup-wizard` is a thin
> alias. The web wizard and the headless `novactl setup --interactive | --config-file`
> share one UI-agnostic core (`internal/setup/`: answers, keygen, render, tls, commit).
> The `/setup/*` seam (`internal/api/handlers/setup.go`) binds loopback-only and is
> sentinel-gated. The **web** wizard configures the local issuer (the default); the
> **external-OIDC** path (`auth_mode: external` + `issuer_url`/`client_id`) is configured
> via the headless `novactl setup --config-file` / manual `operator.yaml` path, not the
> web stepper. Design: `docs/superpowers/specs/2026-06-08-phase1-m13-setup-wizard-design.md`.

The wizard runs only when `.bootstrap-complete` is absent. The
coordinator's entrypoint script (`docker/init/entrypoint.sh`) checks
for the sentinel and either:

1. Sentinel **absent**: launch in setup mode — a *reduced* boot that opens only the
   DB pool (needed to create the admin user) and mounts only the loopback `/setup/*`
   route group; no keystore / Kubo / auth / upload / audit subsystems start. nginx
   loads `bootstrap.conf` (proxies only `/setup/*`, on loopback `:8444`).
2. Sentinel **present**: launch normally. nginx loads the
   wizard-rendered `nova.conf` (two-vhost). Coordinator mounts the
   full route set; `/setup` returns 404.

Wizard flow (web UI; CLI equivalent for headless ops):

```
Step 1. Welcome + license
Step 2. Master key generation
        - generate via CSPRNG(32 bytes)
        - DISPLAY the hex value PLAINLY
        - DISPLAY a fingerprint (first 8 bytes hex, used as the readback challenge)
        - REQUIRE operator to download a backup .txt with the key + fingerprint
        - REQUIRE operator to TYPE BACK the fingerprint (forced-readback proof)
        - on submit: stage the master key in nova-secrets/ (mode 0600); NOT yet
                     committed to .bootstrap-complete
Step 3. IPFS swarm key generation
        - CSPRNG(32 bytes) in the IPFS swarm-key format
        - stage in nova-config/swarm.key
Step 4. Local OIDC signing key generation (admin login)
        - generate Ed25519 signing key
        - stage in nova-secrets/oidc-signing-key
Step 5. Admin user
        - prompt for username, email, password (12+ chars, basic strength check)
        - hash via argon2id; INSERT users(role='operator') in postgres
Step 6. TLS mode (explicit choice with privacy-cost displayed):
        a) dev-self-signed   (Phase 1 dev default; coordinator generates a CA)
        b) quick-setup HTTP-01 (CT-log disclosure shown)
        c) DNS-01 wildcard (operator provides DNS credentials separately)
        d) static (operator drops PEM files into nova-config/tls/)
        e) .onion (operator runs Tor + supplies self-signed)
Step 7. ToS URL (required if public uploads enabled; otherwise optional)
Step 8. Paranoid mode (single switch; explains tradeoffs)
Step 9. Review summary; confirm or go back.
Step 10. Commit (ordered for crash-safety; sentinel LAST):
        - stage secrets (master-key-<label>, swarm.key, oidc-signing-key) at the
          M6.1 resolver file paths in nova-secrets/, mode 0600
        - render operator.yaml (self-validated via config.LoadFromBytes) + nova.conf
          (two-vhost) into the config volume
        - INSERT users(role='operator') with an argon2id hash
        - write .bootstrap-complete sentinel (the point of no return)
        - SIGTERM ourselves; entrypoint.sh restarts in normal mode
```

> **As implemented (M13):** the commit is `internal/setup/commit.go`. Secrets are staged
> at the resolver file paths so a normal boot picks them up with no env editing; every
> step before the sentinel is idempotent, so a crash re-enters setup mode cleanly. The
> sentinel lives on the config volume so deleting it (and restarting) is the documented
> "redo setup" recovery path.

The wizard refuses Step 2 → Step 3 transition until the readback
fingerprint matches. The wizard refuses Step 10 until every prior
step is complete.

Headless CLI alternative: `docker compose run --rm coordinator novactl setup --interactive` or
`docker compose run --rm coordinator novactl setup --config-file path/to/answers.yaml`.

## Error handling

**Startup**: the validator's refuse-to-start list (per
`KUBO_HARDENING.md`, `PRIVACY_AUDIT.md`, `OPERATOR_CHECKLIST.md` and
this design's section "Network exposure floor") prints a precise
error message naming the offending key and exits non-zero. Docker
restart loops surface this in `docker compose logs`.

**Request-level**: every handler returns the JSON `Error` schema
from openapi.yaml. Codes are snake_case strings; HTTP status is
chosen per the spec; `request_id` (from middleware) and `details`
(optional) supplement.

**Audit failures**: never auto-remediate. The integrity audit
records the failure to `integrity_audits` and increments the
metric; the admin UI surfaces the failure for operator decision.
The seven failure modes have suggested actions in
`INTEGRITY_AUDIT.md` § "Failure handling".

**Job failures**: handled by the queue (retry with exponential
backoff up to `max_attempts`, then `dead`). Dead jobs surface in
the admin SPA's jobs view. Operators can manually retry, edit
payload, or delete.

**DB transaction failures**: rolled back; the upload pipeline's
post-import cleanup unpins the orphan IPFS pin so the blockstore
doesn't fill with rolled-back uploads.

**Kubo failures**: bubble up as 500. The integrity audit's
`kubo_pin_present` check catches drift; orchestrator (Phase 2) will
re-pin from donors. Phase 1 has no donors so this is a hard
failure surfaced to the operator.

## Testing strategy

### Unit tests
- Per package; standard `go test ./...`.
- `internal/envelope`: golden vectors for v1 encrypt/decrypt; round-trip
  tests against `IPFS_IMPORT_RULES.md` parameters.
- `internal/auth/signedurl`: canonical-string vectors; revocation
  matrix; constant-time comparison.
- `internal/ipfs/validate`: every refuse-to-start rule with a
  table-driven `failing config` case.
- `internal/jobs`: simulate worker crashes; verify lease reclaim.

### Integration tests
- `internal/integration/` runs against testcontainers
  (postgres-16-alpine) + an in-memory Kubo backend.
- Upload → decrypt → CID match round-trip.
- Master-key rotation against a synthetic 1k-key dataset.
- DMCA flow end-to-end (quarantine → scheduled tombstone → crypto-shred).
- Integrity audit: drop a Kubo pin, verify the next audit catches it.
- Signed-URL: mint, expire, revoke; verify each path; constant-time
  comparison (timing-tolerance assertion).

### Docker-level smoke tests
- `make smoke` brings up the full compose stack (in a tmp project
  dir), runs the wizard via headless CLI, runs a curl-driven upload
  + read + delete cycle. CI runs this on every PR.

### CI gates
- `make test`: unit + integration.
- `make codegen-check`: `oapi-codegen` and `sqlc generate` re-run;
  fail if generated files diff.
- `make hermetic-spa`: greps the built admin/widget bundles for
  external origins; fails on hit.
- `make vet` + `golangci-lint`.
- `make smoke`: docker integration.

### Human-action tests (operator must run)
See § "Human-action test checklist" below.

## Walking-skeleton milestone breakdown

Each milestone is independently testable and merges to `main`
behind a feature gate (in `operator.yaml`) when the SPA isn't yet
ready to render the surface.

**M1 — Foundation (~week 1)**
- Repo bones: cmd/, internal/, pkg/ skeletons; Makefile targets;
  CI pipeline with empty test passing.
- `internal/db/migrations` from DATA_MODEL.sql + `0002_jobs.sql` +
  `0003_partitions.sql`.
- `internal/db` pgxpool, sqlc.
- `internal/config` loader + paranoid mode.
- `cmd/migrate` runs migrations cleanly against a docker-compose'd
  postgres-16.

**M2 — Envelope + IPFS (~week 2)**
- `internal/envelope` v1 codec + golden vectors.
- `internal/ipfs` embedded backend + `ValidateConfig` + deterministic
  Add round-trip.
- `internal/jobs` queue + worker pool.
- Smoke: encrypt a file → import to Kubo → fetch back → decrypt → bytes match.

**M3 — Storage core API (~week 3)**
- `internal/api/handlers/health.go`, `blob.go` (GET, HEAD, .json).
- `pkg/coordinator/coordinator.go` lifecycle (New, Run, Shutdown).
- `cmd/coordinator/main.go` wires everything; ports right; loopback Kubo correct.
- Auth bypass in dev (build tag `nova_dev`); production refuse-to-start works.
- curl `GET /blob/{cid}` returns 200 with decrypted bytes; `GET /health` returns 200.

**M4 — Upload pipeline (~week 4)**
- tus.io endpoints (`/api/v1/uploads/*`) + multipart fallback
  (`/api/v1/blobs`).
- AnalyzeUpload → encrypt → import → manifest → DB commit transaction.
- Master-key wrap/unwrap; `data_encryption_keys` lifecycle.
- Product-agnostic write path; the AnalyzeUpload seam is a no-op in M4.
  nova-image AnalyzeUpload (width/height/PDQ) moves to M5 — see
  `docs/superpowers/specs/2026-05-29-phase1-m4-upload-pipeline-design.md`
  § "Source of truth and required doc reconciliations".
- curl upload → GET /blob/{cid} round-trips a JPEG.

**M5 — Image transforms (~week 5)**
- govips wrapper; transform pipeline.
- `/i/{cid}.{ext}`, `/i/{cid}/wNxN.ext`, `/i/{cid}/wN.ext`,
  `/i/{cid}/p/{preset}.ext`.
- Derivative cache (parent_cid + preset + format) → first-class blob with own key.
- derivative_prewarm job runs OnCommitted.
- curl `/i/{cid}/w512.webp` against a JPEG upload produces a 512-px WebP.

**M6 — Auth (~week 6)**
- `internal/auth/localissuer` + bearer middleware.
- `/api/v1/auth/login`, `/refresh`, `/logout`, `/jwks.json`.
- `novactl auth login` CLI.
- DEV → PROD: nova_dev build tag dropped; production refuses without bearer.
- Design: `docs/superpowers/specs/2026-05-30-phase1-m6-auth-design.md`.
  Implementation plan: `docs/superpowers/plans/2026-05-30-phase1-m6-auth.md`.

**M6.1 — Keystore hardening (out-of-band)**
- Master-key resolver chain (env → `_FILE` → `/run/secrets/master-key-<label>`)
  for both `NOVA_MASTER_KEY_<LABEL>` and `NOVA_OIDC_SIGNING_KEY`.
- ACTIVE/FILE pseudo-label filtering so typo'd forms cannot leak.
- `THREAT_MODEL.md` boundary ③ amended.
- Design: `docs/superpowers/specs/2026-05-31-m6.1-keystore-secret-mount-design.md`.

**M6.2 — Audit remediation (out-of-band)**
- Spec-drift reconciliation across persistent docs (README, ROADMAP,
  THREAT_MODEL asset table, SIGNED_URL_FORMAT, ENCRYPTION_ENVELOPE,
  PRODUCT_MODULE_INTERFACE, this design's L709 external-OIDC note).
- Verified security hardening: rate-limiter LRU + sweep,
  trusted-proxy XFF enforcement, login-failure log unification,
  refresh-family revocation correctness, master-key source logging,
  ctx-aware Unwrap, multipart `LimitReader`.
- Performance: refresh-token GC partial-index alignment.
- UX: `/readyz` with DB + Kubo + OIDC checks; structured coordinator
  startup log.
- Review: `docs/REVIEW_2026_05_31.md`.

**M7 — Signed URLs + signing-key rotation (~week 7)**
- `internal/auth/signedurl` + revocation lookup.
- `/api/v1/admin/keys/rotate-signing` + grace window.
- Admin can revoke `(kind, value)` tuples.
- curl with sig query verifies; expired sig 403; revoked sig 403.

**M8 — Integrity audits (~week 8)**
- `internal/audit/integrity` scheduler + seven audit kinds + reporting.
- In-process scheduler runs the checks directly (NOT the jobs.Queue):
  bounded goroutines under a per-run timeout, resuming from each kind's
  natural cadence on restart. A Maintainer provisions + prunes the
  monthly `integrity_audits` partitions.
- `/api/v1/admin/audits/integrity` returns paginated results.
- A test that deliberately drops a Kubo pin: next `kubo_pin_present`
  audit reports fail; admin UI surfaces it.

**M9 — Moderation (~week 9)**
- DMCA quarantine flow (`novactl moderation quarantine`, scheduled
  tombstone job, counter-notice handling).
- Severe-content manual quarantine (`--legal-hold`); clear-legal-hold.
- `/api/v1/admin/moderation/queue`, `/{id}/takedown`,
  `/blocklist` (Phase 1: blocklist is operator-curated; no PDQ scan
  on upload yet).
- DB CHECK constraint catches shred-under-legal-hold.

**M10 — Master-key rotation (~week 10)**
- `novactl keys rotate-master --to-version v2`.
- `/api/v1/admin/keys/rotate-master`.
- Worker handles `master_key_rotate_row` jobs in parallel.
- Read path during rotation works against either MK version.
- Test: 10k synthetic blobs; rotate; measure wall time; verify all
  blobs decrypt against the new master.

**M11 — Admin SPA (~week 11–12)**
- React + Vite hermetic build.
- Login screen (PKCE-style against local issuer).
- Blob list / view / soft-delete.
- Moderation queue + DMCA cases.
- Integrity audit failures view.
- Key rotation buttons (with confirmation modals).
- Jobs view.

**M12 — Widget (implemented, tag `m12-upload-widget`)**
- Hermetic Uppy + tus embeddable widget (`web/widget/`); single-`<script>` embed;
  `getToken` bearer; coordinator `/widget/*` seam (`NOVA_WIDGET_DIST_DIR`).
- Design: `docs/superpowers/specs/2026-06-07-phase1-m12-upload-widget-design.md`.
  Plan: `docs/superpowers/plans/2026-06-07-phase1-m12-upload-widget.md`.

**M13 — Setup wizard + Docker production (implemented, tag `m13-setup-wizard`)**
- Shared UI-agnostic core (`internal/setup/`) behind both a hermetic React+Vite web
  wizard (`web/setup/`) and a headless `novactl setup --interactive | --config-file`.
- Setup mode **folded into the coordinator boot path** (`coordinator.RunSetupServer`,
  sentinel-gated) — a reduced boot mounting only the loopback `/setup/*` seam until
  `.bootstrap-complete` is written; `cmd/setup-wizard` is a thin alias (not a second
  long-lived binary). `entrypoint.sh` does the sentinel branch.
- `operator.yaml` wired into `cmd/coordinator` as the canonical non-secret config,
  with `NOVA_*` env reads preserved as overrides (canonical-with-override, not a
  wholesale env replacement; the M7–M12 tuning knobs stay env-only for now).
- Two-vhost split is nginx-only (templated `nova.conf` from
  `internal/setup/templates/nova.conf.tmpl`); the coordinator keeps its single mux.
- TLS modes (`dev-self-signed` / `static` / `http-01`; `dns-01`/`onion` render config +
  print operator-handoff); certbot prod profile is a best-effort renewal scaffold
  (initial issuance + full deploy-hook/reload → M14).
- Docker multi-stage Debian-slim/glibc image (non-root via `gosu`); `setup` + `prod`
  compose profiles; published ports 8442:80, 8443:443, 127.0.0.1:8445:8445, wizard
  127.0.0.1:8444; wizard-generated secrets land in the `nova-secrets` volume.
- The web wizard configures the local issuer (default); external-OIDC is configured via
  the headless `novactl setup --config-file` / manual `operator.yaml` path.
- Design: `docs/superpowers/specs/2026-06-08-phase1-m13-setup-wizard-design.md`.
  Plan: `docs/superpowers/plans/2026-06-08-phase1-m13-setup-wizard.md`.

**M14 — Polish + docs (~week 14)**
- End-to-end smoke test in CI.
- Operator quickstart in `docs/quickstart.md`.
- Phase 1 release-candidate tag.

Each milestone ends with a working artifact that can be
demonstrated or tested. M1–M5 form the walking skeleton; M6–M10
fill out backend capability; M11–M13 add the human-friendly
surfaces; M14 polishes.

## Packages and services you (the operator) need to install or configure

**Host requirements (Arch Linux, your machine):**

```
sudo pacman -S --needed \
    docker docker-compose \
    go nodejs npm \
    git make \
    pkgconf gcc                     # transitive build needs for cgo libvips
```

(`libvips` is consumed inside the coordinator container via
`govips`; it does NOT need to be installed on the host. The host
only needs Go + Node for the dev loop and Docker for runtime.)

Then enable Docker:

```
sudo systemctl enable --now docker
sudo usermod -aG docker $USER     # log out + back in for group to take
```

**Things you'll do once during development:**

- Generate a development `POSTGRES_PASSWORD` for the compose's
  `.env` file. The repo's `.env.example` will document this.
- Run the wizard once (web UI in your browser); confirm the
  master-key backup readback challenge.
- For local TLS testing, accept the dev self-signed cert. The
  wizard's "dev-self-signed" TLS mode generates a local CA + cert
  on first boot.

**Things requiring your involvement during build (potentially):**

- ~~If `govips` build fails inside the container, we may need to
  pin a specific libvips version or use a different base image.~~
  **Resolved in M5:** runtime base image is Debian-slim/glibc
  (govips/libvips requires glibc; alpine/musl and distroless are
  unsuitable for the cgo libvips link).
- If `embedded Kubo` adds significant transitive Go module bloat,
  we may need to vendor or use Go workspace tricks. I'll surface
  this if it happens.

**Things requiring sudo on your machine: none beyond the initial
`pacman -S` and `systemctl enable docker`.** Everything else
runs inside Docker.

## MCP servers and tooling

Already available in your environment:

- **github MCP** (`mcp__plugin_github_github__*`): useful for issue
  tracking, PRs, releases as we move toward Phase 1 → Phase 2
  transitions. I'll use it for PR creation when you ask.

**Additional MCPs that would help during this build (you can
choose to add or skip):**

1. **Postgres MCP server** — direct SQL inspection during dev.
   Useful for: verifying migration shape, inspecting `jobs` table
   when a job is stuck, looking at `audit_log` for forensic
   debugging, sanity-checking `pin_assignments` (will be empty in
   Phase 1 but populated in Phase 2). The community has
   `crystaldba/postgres-mcp` and a few others on the MCP registry;
   any one that supports `EXPLAIN` and parameterized queries is
   fine.
2. **Docker MCP server** — `docker ps`, `docker compose logs`,
   `docker compose exec` from within our conversation. Reduces
   the back-and-forth of you running `docker compose logs
   coordinator` and pasting output to me. Several community
   options exist.

**Custom MCP I could build for you specifically (worth considering
once we have running code):**

3. **Nova-test MCP** — a small MCP that wraps `novactl` plus a
   curl harness for the OpenAPI endpoints. Lets us run quick
   "encrypt this file → upload → fetch back → decrypt → diff"
   checks from within a planning conversation without you needing
   to context-switch to a terminal. Phase 1.5 idea once the API
   is stable.

**Tooling that's NOT an MCP but I'd recommend installing on your
host alongside Docker:**

- `dive` (`yay -S dive` or `pacman -S dive` if available) — to
  introspect the multi-stage Docker image and confirm we don't
  ship secrets or unused layers.
- `mkcert` — for development TLS when the wizard's
  dev-self-signed isn't enough.
- `gh` CLI — already available via the github MCP, but the CLI is
  handy.

## Human-action test checklist (Phase 1 release)

Tests requiring you (the operator) to perform manual action and
observe outcomes. The automated test suite covers code paths;
these tests verify operator experience and integration with the
world outside the binary.

### First-run wizard
- [ ] **Master-key backup readback enforcement.** Run wizard;
      attempt to advance past Step 2 without typing the fingerprint;
      verify the wizard refuses.
- [ ] **Downloaded backup file integrity.** Download the master-key
      backup .txt; verify it contains exactly the displayed hex value
      and fingerprint; verify no extra metadata.
- [ ] **Sentinel refusal.** After completing the wizard, delete the
      coordinator container and recreate; verify the coordinator
      boots in normal mode (not wizard mode); verify `/setup`
      returns 404.
- [ ] **Sentinel re-arming.** Remove `.bootstrap-complete`; restart;
      verify the wizard mounts again (this is the recovery path if
      an operator wants to redo setup).

### TLS modes
- [ ] **dev-self-signed.** Wizard generates a CA + cert; browser
      shows the self-signed warning; after clicking through,
      curl with `-k` succeeds; `openssl s_client` shows TLSv1.3.
- [ ] **quick-setup HTTP-01 (against staging Let's Encrypt).** Use
      the LE staging URL to avoid CT-log pollution during testing;
      verify certbot completes; verify cert is reloaded by nginx
      without a coordinator restart.
- [ ] **static.** Drop a self-supplied PEM pair into
      `nova-config/tls/`; verify wizard picks them up; nginx serves
      them; certificate fingerprint matches.

### Upload + read + delete cycle
- [ ] **Drag-and-drop JPEG upload via widget.** Use the widget on a
      test HTML page; drop a 2 MB JPEG; observe upload progress;
      observe `201 + CID` returned; observe the image renders at
      `/i/{cid}` in the browser.
- [ ] **Resumable upload.** Start uploading a 30 MB image; kill the
      browser tab partway through; reopen, retry; verify tus resumes
      from where it left off.
- [ ] **Large image transform.** Upload a 12 MB PNG; request
      `/i/{cid}/w256.webp`; verify it renders quickly and the WebP
      is markedly smaller than 12 MB.
- [ ] **Soft delete.** From the admin SPA, delete a blob; verify
      `GET /blob/{cid}` returns 410 after the grace window; verify
      `data_encryption_keys.state='shredded'` (psql or admin UI).

### Authentication
- [ ] **Local issuer login.** Run wizard; create admin user; log in
      via admin SPA; verify access; log out; verify access denied
      until re-login.
- [ ] **Refresh flow.** Stay logged in for 16 minutes (access token
      expires at 15); verify the SPA silently refreshes; observe no
      visible interruption.
- [ ] **CLI login.** Run `novactl auth login` in a terminal;
      browser opens; complete login; CLI receives token; subsequent
      `novactl moderation list` works.
- [ ] **External OIDC.** (Skip if not configuring Authelia yet.) Set
      `auth.issuer_url`; verify local issuer endpoints return 404;
      verify admin SPA redirects to Authelia.

### Moderation
- [ ] **DMCA quarantine flow.** Submit a takedown via
      `POST /legal/dmca` (curl); approve via admin SPA; verify
      `/blob/{cid}` returns 451; verify the bytes are still in
      Kubo (psql `SELECT state FROM blobs`); wait 14 days
      (or set `--tombstone-after 1m` for testing); verify the
      scheduled tombstone job fires; verify `/blob/{cid}` returns
      410 and the key is shredded.
- [ ] **Counter-notification.** Submit takedown; quarantine; submit
      counter-notice; verify `scheduled_tombstone_at` cleared;
      verify operator can restore (set state back to active).
- [ ] **Severe-content legal-hold.** Run
      `novactl moderation quarantine <cid> --legal-hold`; verify
      `data_encryption_keys.legal_hold=true`; attempt to run the
      scheduled tombstone job manually; verify it refuses;
      `novactl moderation clear-legal-hold <cid>`; verify the next
      scheduled-tombstone tick fires.

### Master-key rotation
- [ ] **Synthetic 10k-blob rotation.** Use a test fixture script to
      upload 10,000 small files; run
      `novactl keys rotate-master --to-version v2`; observe the job
      queue drain; verify every blob still decrypts after rotation;
      measure wall time (expect ~30-60 seconds per simulation).
- [ ] **Read during rotation.** While rotation is in flight,
      continuously curl `/blob/{some-cid}`; verify no 5xx errors;
      observe latency may rise but reads succeed.
- [ ] **Crash mid-rotation.** Kill the coordinator container in the
      middle of a rotation; restart; verify the job queue picks up
      where it left off; verify final consistency.

### Integrity audits
- [ ] **Drop a Kubo pin manually.** Use `docker compose exec
      coordinator ipfs pin rm <cid>`; wait 15 minutes (or run
      `novactl audits run-now kubo_pin_present`); verify the audit
      reports `fail`; verify the admin SPA surfaces it.
- [ ] **Block hash corruption.** Use sqlite to alter a `blob_blocks`
      row to have a bogus `block_cid`; run the
      `block_hash_valid` audit; verify it reports `fail` with the
      affected blob.

### Paranoid mode
- [ ] **Enable paranoid mode.** Edit `operator.yaml` to set
      `paranoid: true`; restart; verify nginx access log retention
      drops to 24h; verify outbound webhook config is ignored;
      verify the Prometheus endpoint refuses non-loopback binding
      (test: try to publish the metrics port externally; coordinator
      refuses to start).

### Backup and restore
- [ ] **Postgres backup/restore.** Stop the coordinator;
      `pg_dump` the Postgres volume; wipe the volume; create
      fresh; `pg_restore` from the dump; start the coordinator;
      verify previous blobs all decrypt and serve.
- [ ] **Kubo blockstore loss.** Stop the coordinator; wipe
      `nova-kubo-data`; restart. Phase 1: every blob returns 5xx
      because there's no donor to re-pin from. Phase 2: re-pins
      from donors. Document this prominently in the quickstart.
- [ ] **Master-key recovery from paper backup.** Stop the
      coordinator; remove the master key from
      `nova-secrets/master-key`; type back the master key from
      your paper backup; restart; verify decryption resumes.

## Risks and open questions

**Open questions to revisit during implementation:**

1. **govips + libvips inside distroless.** ~~Whether `distroless`
   contains enough shared libraries for govips to link, or whether
   we need an alpine-based runtime base image. Test in M5.~~
   **RESOLVED in M5:** distroless (musl/alpine) is unsuitable because
   govips/libvips requires glibc. The runtime base image is
   **Debian-slim (glibc)**; non-root UID; copies coordinator binary
   and static assets in.
2. **In-process Kubo binary size.** Whether vendoring Kubo and its
   transitive deps blows past a comfortable container image size.
   If yes, mitigation: build with `-trimpath -ldflags="-s -w"`;
   consider Go workspace for selective vendoring.
3. **Postgres partition migration friction.** Converting an existing
   `integrity_audits` table to partitioned is non-trivial; we mint
   it partitioned from the start, but if a Phase 0.5 operator has
   already deployed with the un-partitioned schema, they need a
   migration path. Phase 1 is the first deployment, so this is
   moot for us — note for future operators.
4. **Webp encoder license / patent concerns** for the format-
   conversion feature. WebP itself is BSD-licensed and royalty-free;
   nothing concerning, but worth confirming if the deployment is
   in a jurisdiction with patent-aggressive regimes.

**Risks:**

1. **Wizard UX iteration debt.** The first-run wizard is critical
   for non-technical operators but its UX won't be polished until
   M13. Mitigate by writing it last, against the stable API of
   M1–M10. Roll the dev-only headless `novactl setup --config-file`
   path early so the team isn't blocked.
2. **Master-key rotation under load.** Rotation against a 10M-blob
   deployment could take hours per `key_rotation_load.py` sim.
   Phase 1 is single-operator with realistic <1M blobs; we'll
   document the load curve in `OPERATOR_CHECKLIST.md` for larger
   deployments.
3. **Kubo version pinning.** Kubo's CID determinism is guaranteed
   within a major version. We pin to a specific patch release in
   `go.mod`; CI tests against the next patch when it ships;
   `IPFS_IMPORT_RULES.md` constraint is independently verified.
4. **Spec drift during Phase 1.** Implementation will discover spec
   gaps; the project pattern is to address with a v3.x amendment.
   I'll surface each as it emerges.

## What requires your decision during implementation

These are things I'll bring to you as we hit them rather than
predict now:

- The admin SPA UI library choice (shadcn/ui vs MUI vs plain CSS
  modules vs minimal Tailwind). My current lean: minimal Tailwind
  + headless UI primitives. Surfaces at M11 start.
- Specific container base image (alpine vs distroless). Surfaces
  at M3 packaging.
- Exact Authelia configuration template (operators using external
  OIDC will need a worked example). Surfaces at M6.
- Concrete `operator.yaml` field naming. The wizard generates it,
  but field names get bikeshedded by humans. Surfaces at M13.

## Cross-references

- Spec floor: `docs/REVIEW_2026_05_25.md` and the v3.1 spec set.
- Architecture-review feedback that shaped this design: discussion
  history in this conversation; key changes documented in the v3.1
  amendment commit.
- The implementation plan derived from this design will live at
  `docs/superpowers/plans/2026-05-25-phase1-single-node-mvp-plan.md`
  (writing-plans skill output).
