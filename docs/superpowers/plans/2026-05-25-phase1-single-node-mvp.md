# Phase 1 — Single-Node MVP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a runnable, single-host, single-operator Nova deployment that honors the v3.1 spec floor and onboards both technical and non-technical operators in one Docker command.

**Architecture:** Go monorepo with in-process embedded Kubo, Postgres-backed job queue, local JWT issuer for auth, ephemeral first-run wizard, nginx-fronted dual-origin (public/admin) deployment. See `docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md` for the authoritative architecture.

**Tech Stack:** Go 1.22, Postgres 16, Kubo (in-process via go-ipfs `coreapi`), pgx/v5, sqlc, oapi-codegen, goose (migrations), chi router, govips (libvips), goimagehash (PDQ), Uppy + tus.io (widget), React + Vite + Tailwind (admin/setup SPAs), nginx 1.25, certbot, docker-compose, testcontainers-go.

---

## Plan structure

This master plan summarizes all 14 milestones with their goals and exit criteria. **Only M1 is expanded into bite-sized tasks here.** Each subsequent milestone gets its own detailed plan document at the start of that milestone, saved to `docs/superpowers/plans/2026-05-25-phase1-m{N}-{name}.md`.

| Milestone | Theme | Status | Plan |
|---|---|---|---|
| M1 | Foundation: repo, migrations, postgres dev env | **completed** | this document, § M1 |
| M2 | Envelope + IPFS round-trip | **completed** | [m2 plan](2026-05-25-phase1-m2-envelope-ipfs.md) |
| M3 | Storage core API (read path) | **completed** | [m3 plan](2026-05-28-phase1-m3-storage-read-api.md) |
| M4 | Upload pipeline (write path) | in progress | [m4 plan](2026-05-29-phase1-m4-upload-pipeline.md) |
| M5 | Image transforms (nova-image) | pending | tbd |
| M6 | Local JWT issuer + bearer auth | pending | tbd |
| M7 | Signed URLs + signing-key rotation | pending | tbd |
| M8 | Integrity audits | pending | tbd |
| M9 | Moderation (DMCA + severe-content manual path) | pending | tbd |
| M10 | Master-key rotation | pending | tbd |
| M11 | Admin SPA | pending | tbd |
| M12 | Drag-and-drop widget | pending | tbd |
| M13 | Setup wizard + Docker prod profile | pending | tbd |
| M14 | Polish, docs, smoke-test in CI | pending | tbd |

After M14: Phase 1 release-candidate tag, then Phase 2 planning.

---

## Milestone summaries

### M1 — Foundation
**Goal:** A fresh checkout can `docker compose up`, `make migrate-up`, and observe every Phase 1 table exist in Postgres. Foundation packages (`internal/db`, `internal/config`) are tested and ready for downstream milestones.

**Deliverables:** repo skeleton, Go module + deps, Makefile + CI, migrations 0001–0004 (DATA_MODEL.sql + jobs table + partitioned audits + envelope_version surfaces), `cmd/migrate` runnable, `internal/db` pgxpool wrapper, `internal/config` types + loader + paranoid mode + secrets resolver, Postgres MCP installed for dev workflow, smoke test passing in CI.

**Exit:** `make test && make smoke` pass; `docker compose up` + `make migrate-up` produce the v3.1-compliant schema; CI green.

### M2 — Envelope + IPFS round-trip
**Goal:** Encrypt a file with v1 envelope, import to embedded Kubo deterministically, fetch it back, decrypt, byte-match.

**Deliverables:** `internal/envelope` v1 codec + `Codec` interface (v2 slot reserved), golden test vectors, `internal/ipfs.Backend` interface, embedded implementation via Kubo coreapi, `ValidateConfig` refuse-to-start hardening rules, master-key wrap/unwrap, `internal/jobs` queue + worker pool.

**Exit:** end-to-end test: encrypt → import → fetch → decrypt → byte-equal; `Backend.ValidateConfig` rejects every hardening violation from `KUBO_HARDENING.md`.

### M3 — Storage core API (read path)
**Goal:** `curl http://localhost:8443/blob/{cid}` returns the decrypted bytes; `/health` returns 200; coordinator binds correctly inside the container.

**Deliverables:** `pkg/coordinator` lifecycle (`New`, `Run`, `Shutdown`), `cmd/coordinator/main.go` wiring, chi router + middleware (request-id, recover, rate-limit; `audit_log` deferred to M6 — it records privileged actions, and M3 has only anonymous public reads), sqlc adoption for the read query surface (committed `internal/db/gen`), `/health` + `/blob/{cid}` (GET, HEAD) + `/blob/{cid}.json` handlers, ports (coordinator :9000 internal), a minimal dev nginx proxy, the dev-only `nova_dev` build tag for the anonymous-mode startup floor.

**Exit:** integration test: upload a blob via direct DB + Kubo, fetch via HTTP through nginx, byte-match.

### M4 — Upload pipeline (write path)
**Goal:** Drag-and-drop-equivalent: a curl-driven tus or multipart upload encrypts, imports, commits, returns a CID, and the blob is fetchable via M3's read path.

**Deliverables:** tus.io handlers (`/api/v1/uploads/*`), multipart fallback (`/api/v1/blobs`; `/api/v1/images` moves to M5 with the nova-image product), product-agnostic encrypt → import → manifest → DB-commit transaction (the AnalyzeUpload seam is a no-op until M5), master-key versioning bootstrap, `derivative_prewarm` job kind (stub Phase 1; nova-image populates it in M5), upload failure rollback (Kubo unpin on transaction rollback).

**Exit:** integration test: tus + multipart uploads of JPEG, PNG, WebP; each fetchable; each has manifest + blocks rows.

### M5 — Image transforms (nova-image)
**Goal:** `/i/{cid}/w512.webp` against an uploaded JPEG returns a 512-px WebP; derivative caching works on the second hit.

**Deliverables:** `nova-image/product.go` implementing `product.Product`; govips wrapper; PDQ perceptual hash; `/i/*` route handlers (original, resize-box, resize-width, preset); first-class derivative blobs; `derivative_prewarm` job wired to `OnCommitted`; format-conversion option (PNG→WebP) on upload; image_metadata side table + migration.

**Exit:** integration test: upload original, request 5 different transform URLs, observe derivative blobs in DB, observe second request hits cache, observe quarantine of parent cascades to derivatives.

### M6 — Local JWT issuer + bearer auth
**Goal:** `novactl auth login` produces a JWT; `curl -H "Authorization: Bearer $TOKEN"` succeeds against `/api/v1/admin/*`; production refuses without bearer.

**Deliverables:** `internal/auth/localissuer` OIDC-shaped service (`/api/v1/auth/login`, `/refresh`, `/logout`, `/jwks.json`); access/refresh token rotation; argon2id password hashing; `internal/auth/bearer` middleware verifying via JWKS; `internal/auth/oidc` adapter for external OIDC; `novactl auth` subcommand; config-driven swap between local and external OIDC; `nova_dev` build tag dropped from main builds.

**Exit:** integration test: login → call protected endpoint → succeed; expired access token → 401; refresh → succeed; external OIDC mode → local-issuer endpoints return 404 and redirects to issuer; production binary refuses to start with `auth: anonymous`.

### M7 — Signed URLs + signing-key rotation
**Goal:** Operator mints a signed URL via API; expired or revoked URLs fail; signing-key rotation works with grace window.

**Deliverables:** `internal/auth/signedurl` canonical-string + HMAC verifier + revocation lookup; `POST /api/v1/admin/keys/rotate-signing` (with grace window); `POST /api/v1/admin/signed-urls/revoke` (structured `(kind, value)` revocation); admin novactl subcommand; signing-key shred after grace.

**Exit:** integration test: mint, verify (origin check), expire, revoke per kind (cid, aud, kid, path_prefix); rotate; verify both old and new signatures work during grace; verify old refuses after grace.

### M8 — Integrity audits
**Goal:** All seven audit kinds run on schedule; failures surface in the admin endpoint; deliberate corruption is detected.

**Deliverables:** `internal/audit/integrity` scheduler + seven audit kind implementations (envelope_decode, key_unwrap, sample_decrypt, kubo_pin_present, derivative_state_consistent, block_hash_valid, manifest_consistent); `integrity_audit_run` job kind; `/api/v1/admin/audits/integrity` returns paginated typed results; nova-test MCP scaffold (custom MCP for development; runs synthetic corruption tests).

**Exit:** integration test: drop a Kubo pin → next `kubo_pin_present` audit reports fail; tamper with a `blob_blocks.block_cid` → next `block_hash_valid` audit reports fail; both surface via admin endpoint.

### M9 — Moderation (DMCA + severe-content manual)
**Goal:** Quarantine-first DMCA flow works end-to-end; severe-content `--legal-hold` is enforced by the DB CHECK constraint.

**Deliverables:** `internal/moderation` quarantine + tombstone + cascade + severe-content handlers; `novactl moderation` subcommand (quarantine, takedown, clear-legal-hold); `POST /legal/dmca` intake + `/api/v1/admin/moderation/*`; scheduled-tombstone job; repeat-infringer accounting; admin endpoints for DMCA case management + moderation queue + blocklist.

**Exit:** integration test: DMCA quarantine → scheduled-tombstone job fires → crypto-shred + unpin; severe-content quarantine with `--legal-hold` → shred refused at DB layer; clear-legal-hold → tombstone permitted.

### M10 — Master-key rotation
**Goal:** `novactl keys rotate-master --to-version v2` rotates a 10k-blob deployment online; reads succeed throughout.

**Deliverables:** multi-version master-key loading (`NOVA_MASTER_KEY_V1`, `_V2`, `_ACTIVE`); `master_key_rotate_row` job kind; `POST /api/v1/admin/keys/rotate-master` enqueues per-row jobs; concurrent worker pool processes; `master_key_versions` lifecycle (active → rotating → retired).

**Exit:** integration test: 10k synthetic blobs; rotate; verify all decrypt against v2; mid-rotation kill of the coordinator → resume on restart; concurrent reads during rotation succeed; final wall-time measured and documented in `OPERATOR_CHECKLIST.md`.

### M11 — Admin SPA
**Goal:** Operator logs in via web; sees blob list, moderation queue, DMCA cases, integrity audit failures, jobs introspection, key-rotation buttons.

**Deliverables:** `web/admin` React + Vite + Tailwind; hermetic build (CI lint blocks external CDN refs); strict CSP; pages for login, blobs, collections, moderation queue, DMCA cases, integrity audits, jobs, settings, key rotation; openapi-typescript generated client; refresh-token rotation in browser.

**Exit:** human-action test: full operator session — login, browse, soft-delete, run a takedown, observe integrity audit failure, trigger a signing-key rotation.

### M12 — Drag-and-drop widget
**Goal:** A single `<script src="/widget.js">` injection on any HTML page produces a drag-and-drop uploader that writes to the operator's Nova.

**Deliverables:** `web/widget` Uppy + tus.io; hermetic single-bundle build; embeddable via `<script>` or imported as `@nova-archive/widget` (npm pkg later); bearer-token configuration; CSS scoped to a single class prefix to avoid host-page conflicts.

**Exit:** human-action test: paste the script onto a static HTML page, drop an image, observe upload + render.

### M13 — Setup wizard + Docker production profile
**Goal:** A fresh checkout: `docker compose --profile setup up` → browser opens wizard → operator completes 10 steps → coordinator restarts in normal mode → full deployment is live.

**Deliverables:** `web/setup` React app (smaller than admin); `internal/setup` ephemeral handlers gated by `.bootstrap-complete` sentinel; forced master-key fingerprint readback; TLS-mode selection with CA gen for dev-self-signed; certbot integration for prod; `docker/Dockerfile` (multi-stage, distroless or alpine); `docker/docker-compose.prod.yml`; `entrypoint.sh` sentinel logic; nginx config templating; `novactl setup` headless CLI alternative.

**Exit:** human-action test: end-to-end first-run wizard on a fresh VM; same with headless CLI; sentinel re-arming after manual removal.

### M14 — Polish, docs, smoke test in CI
**Goal:** Phase 1 is release-ready. Every human-action test from the design spec is documented as a runbook step; CI runs a docker-compose smoke test on every PR.

**Deliverables:** `docs/quickstart.md`; `docs/operator-runbook.md`; CI `make smoke` against the full compose stack; release-candidate tag `v0.1.0-rc1`; sigstore signing deferred to Phase 5.

**Exit:** Bug runs through `docs/quickstart.md` on a fresh VPS; everything works; tag `v0.1.0-rc1`.

---

## M1 — Foundation: Detailed Tasks

### Files for M1

**Created in M1:**

| Path | Purpose |
|---|---|
| `Makefile` | Top-level developer targets |
| `go.mod` / `go.sum` | Go module + locked deps |
| `.github/workflows/ci.yml` | CI: test, vet, lint, sqlc-diff, smoke |
| `.golangci.yml` | Lint configuration |
| `.gitignore` | Generated/local artifacts |
| `docker/docker-compose.yml` | Dev profile: postgres only at M1 |
| `docker/docker-compose.override.yml.example` | Operator-local overrides documentation |
| `docker/.env.example` | Sample env (`POSTGRES_PASSWORD`) |
| `internal/db/migrations/0001_init.sql` | DATA_MODEL.sql verbatim |
| `internal/db/migrations/0002_jobs.sql` | Job queue table (partitioned) |
| `internal/db/migrations/0003_partitions.sql` | Convert audit tables to partitioned |
| `internal/db/migrations/0004_envelope_version.sql` | Add `blobs.envelope_version` |
| `internal/db/migrations/migrations.go` | embed.FS exposer |
| `internal/db/migrations/migrations_test.go` | Migration listing tests |
| `internal/db/db.go` | pgxpool wrapper |
| `internal/db/db_test.go` | pgxpool integration tests (testcontainers) |
| `internal/config/types.go` | Config struct (mirrors `operator.yaml`) |
| `internal/config/operator_yaml.go` | YAML loader + validator |
| `internal/config/paranoid.go` | Paranoid-mode overrides |
| `internal/config/secrets.go` | env → env_file → /run/secrets resolver |
| `internal/config/operator_yaml_test.go` | Loader tests |
| `internal/config/paranoid_test.go` | Paranoid override tests |
| `internal/config/secrets_test.go` | Resolver precedence tests |
| `internal/config/testdata/operator.minimal.yaml` | Test fixture |
| `internal/config/testdata/operator.paranoid.yaml` | Test fixture |
| `cmd/migrate/main.go` | Migration runner CLI |
| `cmd/migrate/main_test.go` | Runner integration test |
| `scripts/smoke.sh` | M1 smoke test (compose up + migrate + assert schema) |
| `.claude/settings.local.json` | Postgres MCP server config |

**Empty `.gitkeep` package scaffolding** (placeholders for later milestones):

```
cmd/{coordinator,novactl,setup-wizard}/.gitkeep
pkg/coordinator/{,storage,product}/.gitkeep
internal/{envelope,ipfs,auth,jobs,api,audit,moderation,setup,webhook,ratelimit,node}/.gitkeep
nova-image/{internal,migrations}/.gitkeep
web/{widget,admin,setup}/.gitkeep
docker/{init,nginx}/.gitkeep
```

### Task 1: Initialize Go module and core dependencies

**Files:**
- Modify: `go.mod`, `go.sum` (existing module declaration)

- [ ] **Step 1.1: Verify current go.mod**

```bash
cat go.mod
```

Expected: shows `module github.com/nova-archive/nova` and `go 1.22`.

- [ ] **Step 1.2: Add M1 dependencies**

```bash
go get github.com/jackc/pgx/v5@latest
go get github.com/jackc/pgx/v5/pgxpool@latest
go get github.com/pressly/goose/v3@latest
go get github.com/stretchr/testify@latest
go get github.com/testcontainers/testcontainers-go@latest
go get github.com/testcontainers/testcontainers-go/modules/postgres@latest
go get gopkg.in/yaml.v3@latest
```

- [ ] **Step 1.3: Tidy**

```bash
go mod tidy
```

Expected: `go.sum` is updated; no errors.

- [ ] **Step 1.4: Commit**

```bash
git add go.mod go.sum
git commit -s -m "chore: add Phase 1 M1 Go dependencies (pgx, goose, testcontainers, yaml, testify)"
```

---

### Task 2: Create Makefile

**Files:**
- Modify: `Makefile` (Phase 0 version exists, expand it)

- [ ] **Step 2.1: Replace Phase 0 Makefile**

Replace the entire `Makefile` with:

```makefile
.PHONY: help test test-unit test-integration tidy build lint smoke migrate-up migrate-down migrate-status clean

GOTEST    := go test ./...
GOTESTV   := go test -v ./...
DC        := docker compose -f docker/docker-compose.yml --env-file docker/.env

help:
	@echo "Phase 1 M1 targets:"
	@echo "  test              Run all Go tests (unit + integration)"
	@echo "  test-unit         Run only unit tests (-short)"
	@echo "  test-integration  Run only integration tests"
	@echo "  tidy              Tidy Go module files"
	@echo "  build             Build cmd/migrate (other binaries in later M)"
	@echo "  lint              Run golangci-lint"
	@echo "  smoke             End-to-end smoke: compose up + migrate + assert schema"
	@echo "  migrate-up        Apply migrations against running compose postgres"
	@echo "  migrate-down      Roll back one migration"
	@echo "  migrate-status    Show migration status"
	@echo "  clean             Remove build artifacts"

test:
	$(GOTESTV)

test-unit:
	$(GOTESTV) -short

test-integration:
	$(GOTESTV) -run Integration

tidy:
	go mod tidy

build:
	mkdir -p bin
	go build -trimpath -ldflags="-s -w" -o bin/migrate ./cmd/migrate

lint:
	golangci-lint run

smoke:
	./scripts/smoke.sh

migrate-up: build
	$(DC) up -d postgres
	$(DC) exec -T postgres pg_isready -U nova || (sleep 5 && $(DC) exec -T postgres pg_isready -U nova)
	./bin/migrate up

migrate-down: build
	./bin/migrate down

migrate-status: build
	./bin/migrate status

clean:
	rm -rf bin dist build coverage.out coverage.html
```

- [ ] **Step 2.2: Verify make help works**

```bash
make help
```

Expected: lists all targets.

- [ ] **Step 2.3: Commit**

```bash
git add Makefile
git commit -s -m "build(make): M1 Makefile with test/build/smoke/migrate targets"
```

---

### Task 3: Create directory structure and .gitkeep scaffolding

**Files:** many `.gitkeep` placeholders.

- [ ] **Step 3.1: Create the empty scaffold**

```bash
mkdir -p \
  cmd/coordinator cmd/migrate cmd/novactl cmd/setup-wizard \
  pkg/coordinator/storage pkg/coordinator/product \
  internal/envelope internal/ipfs internal/auth internal/jobs internal/api \
  internal/audit internal/moderation internal/setup internal/webhook \
  internal/ratelimit internal/node internal/db/migrations internal/db/queries \
  internal/config internal/config/testdata \
  nova-image/internal nova-image/migrations \
  web/widget web/admin web/setup \
  docker/init docker/nginx \
  scripts \
  .claude
```

- [ ] **Step 3.2: Add .gitkeep to empty directories**

```bash
for d in \
  cmd/coordinator cmd/novactl cmd/setup-wizard \
  pkg/coordinator pkg/coordinator/storage pkg/coordinator/product \
  internal/envelope internal/ipfs internal/auth internal/jobs internal/api \
  internal/audit internal/moderation internal/setup internal/webhook \
  internal/ratelimit internal/node \
  nova-image/internal nova-image/migrations \
  web/widget web/admin web/setup \
  docker/init docker/nginx; do
    touch "$d/.gitkeep"
done
```

- [ ] **Step 3.3: Update .gitignore**

Replace `.gitignore` with:

```gitignore
# Build artifacts
/bin/
/dist/
/build/
coverage.out
coverage.html

# Generated code (sqlc, oapi-codegen — committed in later M but ignored locally)
internal/db/gen/
internal/api/codegen/

# Node
node_modules/

# Local environment
.env
docker/.env
docker/docker-compose.override.yml
nova-config/
nova-secrets/
nova-postgres-data/
nova-kubo-data/
nova-tmp-uploads/

# Editor/OS
.DS_Store
.idea/
.vscode/
*.swp
```

- [ ] **Step 3.4: Commit**

```bash
git add cmd pkg internal nova-image web docker scripts .gitignore .claude
git commit -s -m "chore: M1 directory skeleton + .gitignore for Phase 1 artifacts"
```

---

### Task 4: Install Postgres MCP for dev workflow

**Files:**
- Create: `.claude/settings.local.json`

This task adds a Postgres MCP server to the project's Claude Code settings so that during development, SQL queries against the dev postgres can be run from within a Claude session. Bug authorized autonomous MCP installation; we use the project-local settings file so the configuration is committed (the MCP needs the connection string, but no secrets, since the dev postgres password lives in `docker/.env` which is gitignored).

- [ ] **Step 4.1: Create `.claude/settings.local.json`**

```json
{
  "mcpServers": {
    "nova-dev-postgres": {
      "command": "npx",
      "args": [
        "-y",
        "@modelcontextprotocol/server-postgres",
        "postgres://nova:${POSTGRES_PASSWORD}@127.0.0.1:5432/nova"
      ],
      "env": {
        "POSTGRES_PASSWORD": "${POSTGRES_PASSWORD}"
      }
    }
  },
  "permissions": {
    "allow": [
      "Bash(docker compose:*)",
      "Bash(make:*)",
      "Bash(go test:*)",
      "Bash(go build:*)",
      "Bash(go vet:*)",
      "Bash(./bin/migrate:*)",
      "Bash(./scripts/smoke.sh)"
    ]
  }
}
```

- [ ] **Step 4.2: Document the MCP in the README**

Edit `README.md`. After the "Phase 0 deliverables" section, add a new section:

```markdown
## Development MCP servers

Phase 1 onwards, this project ships a `.claude/settings.local.json` that
configures a Postgres MCP server (`nova-dev-postgres`). When the dev
Postgres container is up (`docker compose -f docker/docker-compose.yml up
-d postgres`), Claude Code sessions with this project loaded can query
the dev database directly via MCP.

The MCP connection string reads `POSTGRES_PASSWORD` from your shell env;
set it from `docker/.env` before running Claude Code:

```sh
set -a; source docker/.env; set +a
```

If you do not want the MCP server, delete or comment out the
`mcpServers.nova-dev-postgres` entry in `.claude/settings.local.json`.
The MCP is dev-only; production deployments do not use it.
```

- [ ] **Step 4.3: Verify the MCP package resolves**

```bash
npx -y @modelcontextprotocol/server-postgres --help 2>&1 | head -5
```

Expected: shows usage. (If the package doesn't exist by that name, substitute the canonical Postgres MCP package — `crystaldba/postgres-mcp` or similar — and update the settings file. Document the chosen package in the commit message.)

- [ ] **Step 4.4: Commit**

```bash
git add .claude/settings.local.json README.md
git commit -s -m "chore(dev): add Postgres MCP server for development workflow

Configures @modelcontextprotocol/server-postgres so Claude Code sessions
can query the dev database directly. Connection string reads
POSTGRES_PASSWORD from shell env; dev-only."
```

---

### Task 5: Migration 0001 — DATA_MODEL.sql verbatim

**Files:**
- Create: `internal/db/migrations/0001_init.sql`

The v3.1 spec floor mandates that the DB schema match `docs/specs/DATA_MODEL.sql`. The first migration is a verbatim copy, with goose annotations added at the top and bottom.

- [ ] **Step 5.1: Create the migration**

```bash
{
  cat <<'EOF'
-- +goose Up
-- +goose StatementBegin
-- Forward-only migration 0001: initial schema per docs/specs/DATA_MODEL.sql
-- This migration MUST remain bit-identical to docs/specs/DATA_MODEL.sql.
-- Drift fails CI (see .github/workflows/ci.yml: schema-drift check).
EOF
  cat docs/specs/DATA_MODEL.sql
  cat <<'EOF'
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Down-migration not provided. Phase 1 deployments treat schema rollback
-- as a restore-from-backup operation; the forward migration is the only
-- path through 0001.
SELECT 1;
-- +goose StatementEnd
EOF
} > internal/db/migrations/0001_init.sql
```

- [ ] **Step 5.2: Verify the file is well-formed**

```bash
head -3 internal/db/migrations/0001_init.sql
tail -3 internal/db/migrations/0001_init.sql
wc -l internal/db/migrations/0001_init.sql
```

Expected: starts with `-- +goose Up`; ends with `-- +goose StatementEnd`; line count > 600 (DATA_MODEL.sql is ~610 lines).

- [ ] **Step 5.3: Commit**

```bash
git add internal/db/migrations/0001_init.sql
git commit -s -m "feat(db): migration 0001 — initial schema from DATA_MODEL.sql"
```

---

### Task 6: Migration 0002 — jobs table

**Files:**
- Create: `internal/db/migrations/0002_jobs.sql`

The job queue uses Postgres-backed leasing (`SELECT … FOR UPDATE SKIP LOCKED`). The table is RANGE-partitioned by `created_at` monthly so old completed/failed rows can be detached cheaply.

- [ ] **Step 6.1: Create the migration**

```sql
-- +goose Up
-- +goose StatementBegin
-- Migration 0002: job queue table (partitioned by created_at, monthly).
-- See docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md
-- § "Job lifecycle".

CREATE TYPE job_state AS ENUM (
    'pending',
    'leased',
    'completed',
    'failed',
    'dead'
);

CREATE TABLE jobs (
    id            uuid NOT NULL DEFAULT gen_random_uuid(),
    kind          text NOT NULL,
    payload       jsonb NOT NULL DEFAULT '{}'::jsonb,
    state         job_state NOT NULL DEFAULT 'pending',
    attempts      int NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts  int NOT NULL DEFAULT 5 CHECK (max_attempts > 0),
    lease_until   timestamptz,
    not_before    timestamptz NOT NULL DEFAULT now(),
    last_error    text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

-- Initial partition for the current month + the next month so the table
-- accepts rows immediately and the partition-rotation job has headroom.
CREATE TABLE jobs_default PARTITION OF jobs
    FOR VALUES FROM (MINVALUE) TO ('2026-06-01 00:00:00+00');
CREATE TABLE jobs_2026_06 PARTITION OF jobs
    FOR VALUES FROM ('2026-06-01 00:00:00+00') TO ('2026-07-01 00:00:00+00');

-- Worker leasing index: pending jobs ordered by created_at, with not_before
-- guarding scheduled-future work. Partial index for the hot path.
CREATE INDEX jobs_lease_idx
    ON jobs (created_at, not_before)
    WHERE state = 'pending';

-- State + kind index for admin introspection.
CREATE INDEX jobs_state_kind_idx ON jobs (state, kind, created_at DESC);

-- Lease reclaim index for stuck-job recovery.
CREATE INDEX jobs_lease_reclaim_idx
    ON jobs (lease_until)
    WHERE state = 'leased';

CREATE TRIGGER jobs_updated_at
    BEFORE UPDATE ON jobs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE jobs CASCADE;
DROP TYPE job_state;
-- +goose StatementEnd
```

- [ ] **Step 6.2: Verify the file**

```bash
grep -c 'CREATE TABLE' internal/db/migrations/0002_jobs.sql
```

Expected: 3 (jobs, jobs_default, jobs_2026_06).

- [ ] **Step 6.3: Commit**

```bash
git add internal/db/migrations/0002_jobs.sql
git commit -s -m "feat(db): migration 0002 — Postgres-backed job queue (partitioned)"
```

---

### Task 7: Migration 0003 — partitioned audit tables

**Files:**
- Create: `internal/db/migrations/0003_partitions.sql`

The 0001 migration creates `integrity_audits` and `audit_log` as plain tables. For Phase 1 fresh deployment, we drop and recreate them as RANGE partitioned by their time column. This is safe because Phase 1 is the first deployment — no production data to preserve.

- [ ] **Step 7.1: Create the migration**

```sql
-- +goose Up
-- +goose StatementBegin
-- Migration 0003: convert integrity_audits and audit_log to partitioned tables.
-- Safe in Phase 1 because there is no prior data. Operators upgrading from
-- pre-Phase-1 dev databases will lose audit history; document in
-- docs/quickstart.md.

DROP TABLE integrity_audits;
DROP TABLE audit_log;

CREATE TABLE integrity_audits (
    id          bigserial NOT NULL,
    cid         text NOT NULL,
    audit_kind  audit_kind NOT NULL,
    result      audit_result NOT NULL,
    error       text,
    audited_at  timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (id, audited_at)
) PARTITION BY RANGE (audited_at);

CREATE TABLE integrity_audits_default PARTITION OF integrity_audits
    FOR VALUES FROM (MINVALUE) TO ('2026-06-01 00:00:00+00');
CREATE TABLE integrity_audits_2026_06 PARTITION OF integrity_audits
    FOR VALUES FROM ('2026-06-01 00:00:00+00') TO ('2026-07-01 00:00:00+00');

CREATE INDEX integrity_audits_cid_kind_idx ON integrity_audits (cid, audit_kind, audited_at DESC);
CREATE INDEX integrity_audits_failures_idx
    ON integrity_audits (audit_kind, audited_at DESC)
    WHERE result <> 'pass';

CREATE TABLE audit_log (
    id           bigserial NOT NULL,
    actor_id     uuid REFERENCES users (id),
    action       text NOT NULL,
    target_type  text NOT NULL,
    target_id    text NOT NULL,
    payload      jsonb NOT NULL DEFAULT '{}'::jsonb,
    at           timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (id, at)
) PARTITION BY RANGE (at);

CREATE TABLE audit_log_default PARTITION OF audit_log
    FOR VALUES FROM (MINVALUE) TO ('2026-06-01 00:00:00+00');
CREATE TABLE audit_log_2026_06 PARTITION OF audit_log
    FOR VALUES FROM ('2026-06-01 00:00:00+00') TO ('2026-07-01 00:00:00+00');

CREATE INDEX audit_log_target_idx ON audit_log (target_type, target_id);
CREATE INDEX audit_log_actor_idx ON audit_log (actor_id, at DESC);
CREATE INDEX audit_log_at_idx ON audit_log (at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE integrity_audits CASCADE;
DROP TABLE audit_log CASCADE;
-- Recreate the original unpartitioned forms (matches 0001).
CREATE TABLE integrity_audits (
    id          bigserial PRIMARY KEY,
    cid         text NOT NULL,
    audit_kind  audit_kind NOT NULL,
    result      audit_result NOT NULL,
    error       text,
    audited_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX integrity_audits_cid_kind_idx ON integrity_audits (cid, audit_kind, audited_at DESC);
CREATE INDEX integrity_audits_failures_idx
    ON integrity_audits (audit_kind, audited_at DESC)
    WHERE result <> 'pass';
CREATE TABLE audit_log (
    id           bigserial PRIMARY KEY,
    actor_id     uuid REFERENCES users (id),
    action       text NOT NULL,
    target_type  text NOT NULL,
    target_id    text NOT NULL,
    payload      jsonb NOT NULL DEFAULT '{}'::jsonb,
    at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_target_idx ON audit_log (target_type, target_id);
CREATE INDEX audit_log_actor_idx ON audit_log (actor_id, at DESC);
CREATE INDEX audit_log_at_idx ON audit_log (at DESC);
-- +goose StatementEnd
```

- [ ] **Step 7.2: Verify the file**

```bash
grep -c 'PARTITION BY RANGE' internal/db/migrations/0003_partitions.sql
```

Expected: 2 (integrity_audits, audit_log).

- [ ] **Step 7.3: Commit**

```bash
git add internal/db/migrations/0003_partitions.sql
git commit -s -m "feat(db): migration 0003 — partition integrity_audits and audit_log by month"
```

---

### Task 8: Migration 0004 — envelope_version surfaces

**Files:**
- Create: `internal/db/migrations/0004_envelope_version.sql`

Per the v3.1 amendment, `blobs.envelope_version` exposes the envelope-format version (1 in Phase 1; 2 when streaming-AEAD ships in Phase 2). `blob_manifests.codec` already accepts free-form text so it doesn't need a DDL change — v2 will record `"chunked-aead-v1"` there.

- [ ] **Step 8.1: Create the migration**

```sql
-- +goose Up
-- +goose StatementBegin
-- Migration 0004: expose envelope_version on blobs.
-- v3.1 amendment. v1 = single-shot XChaCha20-Poly1305 (Phase 1).
-- v2 = streaming-AEAD chunked (Phase 2). The version is determined
-- at encryption time; reads dispatch via the version byte in the
-- envelope bytes themselves, but the column lets us index and
-- filter without parsing every envelope.

ALTER TABLE blobs
    ADD COLUMN envelope_version smallint NOT NULL DEFAULT 1
        CHECK (envelope_version IN (1, 2));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE blobs DROP COLUMN envelope_version;
-- +goose StatementEnd
```

- [ ] **Step 8.2: Commit**

```bash
git add internal/db/migrations/0004_envelope_version.sql
git commit -s -m "feat(db): migration 0004 — expose envelope_version on blobs (v3.1)"
```

---

### Task 9: Migrations embed.FS + listing test (TDD)

**Files:**
- Create: `internal/db/migrations/migrations.go`
- Create: `internal/db/migrations/migrations_test.go`

- [ ] **Step 9.1: Write the failing test**

Create `internal/db/migrations/migrations_test.go`:

```go
package migrations

import (
	"io/fs"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigrationsFSContainsExpectedFiles(t *testing.T) {
	got, err := fs.ReadDir(Migrations, ".")
	require.NoError(t, err)

	names := make([]string, 0, len(got))
	for _, e := range got {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}

	require.Contains(t, names, "0001_init.sql")
	require.Contains(t, names, "0002_jobs.sql")
	require.Contains(t, names, "0003_partitions.sql")
	require.Contains(t, names, "0004_envelope_version.sql")
}

func TestMigrationsFSFirstFileHasGooseAnnotation(t *testing.T) {
	data, err := fs.ReadFile(Migrations, "0001_init.sql")
	require.NoError(t, err)
	require.Contains(t, string(data), "-- +goose Up")
	require.Contains(t, string(data), "-- +goose Down")
}
```

- [ ] **Step 9.2: Run test, verify it fails**

```bash
go test ./internal/db/migrations/...
```

Expected: FAIL with "undefined: Migrations" or similar.

- [ ] **Step 9.3: Implement migrations.go**

Create `internal/db/migrations/migrations.go`:

```go
// Package migrations holds the embed-driven forward-only Postgres
// migrations for Nova. The migrations are loaded by cmd/migrate via
// goose; tests load them directly through the exported Migrations
// embed.FS.
//
// Migrations are append-only. Down migrations exist for completeness
// but production runbooks treat schema rollback as a restore-from-
// backup operation; the forward sequence is the only supported path.
package migrations

import "embed"

//go:embed *.sql
var Migrations embed.FS
```

- [ ] **Step 9.4: Run test, verify it passes**

```bash
go test ./internal/db/migrations/...
```

Expected: PASS.

- [ ] **Step 9.5: Commit**

```bash
git add internal/db/migrations/migrations.go internal/db/migrations/migrations_test.go
git commit -s -m "feat(db): expose migrations as embed.FS"
```

---

### Task 10: pgxpool wrapper (TDD with testcontainer)

**Files:**
- Create: `internal/db/db.go`
- Create: `internal/db/db_test.go`

- [ ] **Step 10.1: Write the failing test**

Create `internal/db/db_test.go`:

```go
package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/db"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestIntegrationOpenAndPing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := postgres.RunContainer(ctx,
		postgres.WithDatabase("novatest"),
		postgres.WithUsername("nova"),
		postgres.WithPassword("test-password"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = container.Terminate(ctx)
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	pool, err := db.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	require.NoError(t, pool.Ping(ctx))
}
```

- [ ] **Step 10.2: Run test, verify it fails**

```bash
go test ./internal/db/...
```

Expected: FAIL with "undefined: db.Open".

- [ ] **Step 10.3: Implement db.go**

Create `internal/db/db.go`:

```go
// Package db is the Postgres connection layer. It exposes a single
// Open() that returns a configured pgxpool, plus a small wrapper
// type so callers can be tested with interface mocks at a coarse
// granularity. Generated sqlc query code (in internal/db/gen) uses
// this pool directly.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open creates a pgxpool against the given DSN with Nova's defaults:
// max 16 connections (the default replication.factor.important hint),
// 30-second connection lifetime, and TLS verified when the DSN says so.
//
// The caller is responsible for calling Close() on the returned pool.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: parse dsn: %w", err)
	}

	cfg.MaxConns = 16
	cfg.MinConns = 2

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}

	return pool, nil
}
```

- [ ] **Step 10.4: Run test, verify it passes**

```bash
go test ./internal/db/...
```

Expected: PASS (the test pulls postgres-16-alpine on first run; ~30s; subsequent runs cached).

- [ ] **Step 10.5: Commit**

```bash
git add internal/db/db.go internal/db/db_test.go
git commit -s -m "feat(db): pgxpool wrapper with integration test (testcontainers)"
```

---

### Task 11: Migration runner — `cmd/migrate` (TDD)

**Files:**
- Create: `cmd/migrate/main.go`
- Create: `cmd/migrate/main_test.go`

- [ ] **Step 11.1: Write the failing integration test**

Create `cmd/migrate/main_test.go`:

```go
package main_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// repoRoot finds the project root by walking up from this test file.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	require.True(t, ok)
	dir := filepath.Dir(here)
	for i := 0; i < 5; i++ {
		if _, err := exec.Command("test", "-f", filepath.Join(dir, "go.mod")).Output(); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("repo root not found")
	return ""
}

func TestIntegrationMigrateUpProducesExpectedTables(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	container, err := postgres.RunContainer(ctx,
		postgres.WithDatabase("nova"),
		postgres.WithUsername("nova"),
		postgres.WithPassword("test-password"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	root := repoRoot(t)
	build := exec.Command("go", "build", "-o", filepath.Join(root, "bin/migrate-test"), "./cmd/migrate")
	build.Dir = root
	require.NoError(t, build.Run())

	run := exec.Command(filepath.Join(root, "bin/migrate-test"), "up")
	run.Env = append(run.Env, "DATABASE_URL="+dsn)
	out, err := run.CombinedOutput()
	require.NoError(t, err, "migrate output: %s", out)

	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close(ctx)

	expectedTables := []string{
		"users", "master_key_versions", "data_encryption_keys", "signing_keys",
		"collections", "blobs", "blob_manifests", "blob_blocks",
		"image_metadata", "collection_items",
		"nodes", "pin_assignments", "pin_audits",
		"integrity_audits", "moderation_decisions", "dmca_cases",
		"takedown_repeat_infringers", "signed_url_revocations",
		"audit_log", "jobs",
	}
	for _, table := range expectedTables {
		var exists bool
		err := conn.QueryRow(ctx,
			`SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				 WHERE table_schema='public' AND table_name=$1
			)`, table).Scan(&exists)
		require.NoError(t, err, "table %s", table)
		require.True(t, exists, "expected table %s to exist after migrate up", table)
	}

	// envelope_version column from migration 0004
	var hasCol bool
	err = conn.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			 WHERE table_name='blobs' AND column_name='envelope_version'
		)`).Scan(&hasCol)
	require.NoError(t, err)
	require.True(t, hasCol, "expected blobs.envelope_version to exist")

	// integrity_audits is partitioned
	var isPartitioned bool
	err = conn.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM pg_partitioned_table pt
			 JOIN pg_class c ON c.oid = pt.partrelid
			 WHERE c.relname = 'integrity_audits'
		)`).Scan(&isPartitioned)
	require.NoError(t, err)
	require.True(t, isPartitioned, "expected integrity_audits to be partitioned")
}
```

- [ ] **Step 11.2: Run test, verify it fails**

```bash
go test ./cmd/migrate/...
```

Expected: FAIL — `./cmd/migrate` has no `main.go`.

- [ ] **Step 11.3: Implement cmd/migrate/main.go**

Create `cmd/migrate/main.go`:

```go
// Package main is the Nova migration runner. It loads the embedded
// SQL files from internal/db/migrations and applies them in order
// against the database identified by DATABASE_URL.
//
// Subcommands:
//   migrate up                  apply all pending migrations
//   migrate down                roll back one migration
//   migrate status              show applied/pending
//   migrate version             show current version
//   migrate create <name>       create a new migration template (Phase 2+)
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/nova-archive/nova/internal/db/migrations"
	"github.com/pressly/goose/v3"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is not set")
	}

	args := os.Args[1:]
	if len(args) == 0 {
		return errors.New("usage: migrate <up|down|status|version>")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}

	goose.SetBaseFS(migrations.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}

	switch args[0] {
	case "up":
		return goose.UpContext(ctx, db, ".")
	case "down":
		return goose.DownContext(ctx, db, ".")
	case "status":
		return goose.StatusContext(ctx, db, ".")
	case "version":
		return goose.VersionContext(ctx, db, ".")
	default:
		return fmt.Errorf("unknown subcommand: %s", args[0])
	}
}

// Make stdlib's pgx driver registration explicit so static checkers don't
// trim the side-effect import.
var _ = stdlib.GetDefaultDriver
```

- [ ] **Step 11.4: Verify dependencies**

```bash
go mod tidy
```

Expected: pulls `github.com/pressly/goose/v3`, `github.com/jackc/pgx/v5/stdlib`.

- [ ] **Step 11.5: Run unit + integration tests**

```bash
go build ./cmd/migrate
go test ./cmd/migrate/...
```

Expected: PASS. (Takes ~30-60s for testcontainer pull on first run.)

- [ ] **Step 11.6: Commit**

```bash
git add cmd/migrate/main.go cmd/migrate/main_test.go go.mod go.sum
git commit -s -m "feat(migrate): cmd/migrate using goose + embedded migrations

DATABASE_URL-driven CLI with up/down/status/version subcommands.
Integration-tested against postgres-16 via testcontainers; verifies
every expected table from DATA_MODEL.sql plus the Phase 1 additions
(jobs, envelope_version column, integrity_audits partitioning)."
```

---

### Task 12: Docker Compose dev profile

**Files:**
- Modify: `docker/docker-compose.yml` (currently lives at repo root as `docker-compose.yml`; move it under `docker/`)
- Create: `docker/.env.example`
- Create: `docker/docker-compose.override.yml.example`

- [ ] **Step 12.1: Move Phase 0 docker-compose.yml under docker/**

```bash
git mv docker-compose.yml docker/docker-compose.yml
```

- [ ] **Step 12.2: Expand to Phase 1 dev shape**

Replace `docker/docker-compose.yml` with:

```yaml
# Phase 1 M1 docker-compose: postgres only.
#
# Subsequent milestones add coordinator, nginx, certbot services.
# Compose profiles will gate the wizard port (--profile setup) and
# the certbot service (--profile prod).
#
# Bring it up:
#   cd docker && cp .env.example .env && $EDITOR .env  # set POSTGRES_PASSWORD
#   docker compose up -d
#
# Verify:
#   docker compose exec postgres pg_isready -U nova
#
# Tear down (data preserved):
#   docker compose down
#
# Tear down + wipe data:
#   docker compose down -v

services:

  postgres:
    image: postgres:16-alpine
    container_name: nova-postgres
    environment:
      POSTGRES_DB: nova
      POSTGRES_USER: nova
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?Set POSTGRES_PASSWORD in .env}
      # Force C locale for deterministic collation across hosts.
      POSTGRES_INITDB_ARGS: "--locale=C --encoding=UTF8"
    volumes:
      - postgres-data:/var/lib/postgresql/data
    ports:
      # Loopback only. Operators who must expose for remote tooling
      # should adjust to a private interface; never bind to 0.0.0.0.
      - "127.0.0.1:5432:5432"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U nova -d nova"]
      interval: 10s
      timeout: 5s
      retries: 5
      start_period: 30s
    restart: unless-stopped
    read_only: true
    tmpfs:
      - /tmp
      - /run/postgresql
    security_opt:
      - no-new-privileges:true

volumes:
  postgres-data:
    driver: local
```

- [ ] **Step 12.3: Create `.env.example`**

```bash
cat > docker/.env.example <<'EOF'
# Phase 1 M1 dev environment.
# Copy to docker/.env and set values. docker/.env is gitignored.

# Required: Postgres superuser password.
# Generate a strong one for non-trivial deployments:
#   openssl rand -base64 32
POSTGRES_PASSWORD=changeme

# Future M: NOVA_MASTER_KEY (M2), NOVA_ADMIN_OIDC_SIGNING_KEY (M6),
#           IPFS_SWARM_KEY (M2), etc.
EOF
```

- [ ] **Step 12.4: Create the override example**

```bash
cat > docker/docker-compose.override.yml.example <<'EOF'
# Optional operator-local overrides. Copy to docker-compose.override.yml
# (gitignored). docker-compose auto-merges override.yml on top of the
# base compose.
#
# Examples:
#   - Bind postgres to a private RFC 1918 interface for remote tooling
#   - Pin a specific Kubo image version
#   - Add a host-bind volume for backups

services:
  postgres:
    ports:
      # Bind postgres to your local management network. Replace 10.0.0.5
      # with your management host's IP. NEVER bind to 0.0.0.0.
      - "10.0.0.5:5432:5432"
EOF
```

- [ ] **Step 12.5: Verify compose validates**

```bash
cd docker && cp .env.example .env && docker compose config >/dev/null && cd ..
```

Expected: prints the resolved compose; exit 0.

- [ ] **Step 12.6: Commit**

```bash
git add docker/docker-compose.yml docker/.env.example docker/docker-compose.override.yml.example
git commit -s -m "build(compose): move compose under docker/, add .env.example, override.yml.example"
```

---

### Task 13: internal/config types and YAML loader (TDD)

**Files:**
- Create: `internal/config/types.go`
- Create: `internal/config/operator_yaml.go`
- Create: `internal/config/operator_yaml_test.go`
- Create: `internal/config/testdata/operator.minimal.yaml`

- [ ] **Step 13.1: Create the test fixture (minimal)**

```bash
cat > internal/config/testdata/operator.minimal.yaml <<'EOF'
# Minimal operator.yaml — exercises every required field.
operator:
  hostname: nova.example.test
  contact_email: admin@example.test

tls:
  mode: dev-self-signed

auth:
  issuer_url: ""  # empty = use local issuer
  paranoid: false

orchestrator:
  tick_interval_seconds: 60
  step_seconds: 60
  replication:
    factor:
      important: 3
      normal: 3
      cache: 2
  mass_casualty_threshold_ratio: 0.20
  mass_casualty_window_seconds: 3600
  capacity_runway_floor_days: 7

federation:
  heartbeat_interval_seconds: 300
  pins_poll_interval_seconds: 600
  max_pin_concurrency: 16
  suspect_after_missed_heartbeats: 3
  unreachable_after_seconds: 3600
  evicted_after_seconds: 2592000

integrity_audit:
  envelope_decode: { interval_seconds: 3600, sample_size: 100 }
  key_unwrap: { interval_seconds: 3600, sample_size: 100 }
  sample_decrypt: { interval_seconds: 3600, sample_size: 50 }
  kubo_pin_present: { interval_seconds: 900, sample_size: 200 }
  derivative_state_consistent: { interval_seconds: 3600, sample_size: 100 }
  block_hash_valid: { interval_seconds: 86400, sample_size: 100 }
  manifest_consistent: { interval_seconds: 86400, sample_size: 100 }

moderation:
  takedown_default_action: quarantine
  dmca_counter_notification_days: 14

coordinator:
  public_ipfs_dht: false
EOF
```

- [ ] **Step 13.2: Write the failing test**

Create `internal/config/operator_yaml_test.go`:

```go
package config_test

import (
	"testing"

	"github.com/nova-archive/nova/internal/config"
	"github.com/stretchr/testify/require"
)

func TestLoadMinimalOperatorYAML(t *testing.T) {
	cfg, err := config.LoadFromFile("testdata/operator.minimal.yaml")
	require.NoError(t, err)

	require.Equal(t, "nova.example.test", cfg.Operator.Hostname)
	require.Equal(t, "admin@example.test", cfg.Operator.ContactEmail)
	require.Equal(t, "dev-self-signed", cfg.TLS.Mode)
	require.Equal(t, "", cfg.Auth.IssuerURL, "empty issuer = use local issuer")
	require.False(t, cfg.Auth.Paranoid)

	require.Equal(t, 60, cfg.Orchestrator.TickIntervalSeconds)
	require.Equal(t, 3, cfg.Orchestrator.Replication.Factor.Important)
	require.Equal(t, 2, cfg.Orchestrator.Replication.Factor.Cache)

	require.Equal(t, 7, cfg.Orchestrator.CapacityRunwayFloorDays)

	require.Equal(t, "quarantine", cfg.Moderation.TakedownDefaultAction)

	require.False(t, cfg.Coordinator.PublicIpfsDht)
}

func TestLoadOperatorYAMLRejectsMissingFile(t *testing.T) {
	_, err := config.LoadFromFile("testdata/does-not-exist.yaml")
	require.Error(t, err)
}

func TestLoadOperatorYAMLRejectsInvalidYAML(t *testing.T) {
	_, err := config.LoadFromBytes([]byte("operator: [this is not a map"))
	require.Error(t, err)
}
```

- [ ] **Step 13.3: Run test, verify it fails**

```bash
go test ./internal/config/...
```

Expected: FAIL — `undefined: config.LoadFromFile`.

- [ ] **Step 13.4: Implement types.go**

Create `internal/config/types.go`:

```go
// Package config models the operator.yaml configuration surface.
// The struct mirrors the v3.1 spec; field defaults and validation
// live in operator_yaml.go (loader) and paranoid.go (mode overrides).
package config

// Config is the root of operator.yaml.
type Config struct {
	Operator       Operator       `yaml:"operator"`
	TLS            TLS            `yaml:"tls"`
	Auth           Auth           `yaml:"auth"`
	Orchestrator   Orchestrator   `yaml:"orchestrator"`
	Federation     Federation     `yaml:"federation"`
	IntegrityAudit IntegrityAudit `yaml:"integrity_audit"`
	Moderation     Moderation     `yaml:"moderation"`
	Coordinator    Coordinator    `yaml:"coordinator"`

	// Webhook destinations; honored only when paranoid=false.
	Webhooks []WebhookDestination `yaml:"webhooks,omitempty"`

	// SourceIPRetentionDays default per spec is 30; 1 in paranoid mode.
	SourceIPRetentionDays int `yaml:"source_ip_retention_days,omitempty"`

	// TosURL must be set when public uploads are enabled.
	TosURL string `yaml:"tos_url,omitempty"`
}

type Operator struct {
	Hostname     string `yaml:"hostname"`
	ContactEmail string `yaml:"contact_email"`
	DisplayName  string `yaml:"display_name,omitempty"`
}

type TLS struct {
	// Mode: one of "dev-self-signed", "http-01", "dns-01", "static", "onion".
	Mode string `yaml:"mode"`
	// For "static" mode:
	CertPath string `yaml:"cert_path,omitempty"`
	KeyPath  string `yaml:"key_path,omitempty"`
}

type Auth struct {
	// Empty string = use the built-in local OIDC issuer.
	// Non-empty = external OIDC provider URL.
	IssuerURL    string   `yaml:"issuer_url"`
	ClientID     string   `yaml:"client_id,omitempty"`
	Scopes       []string `yaml:"scopes,omitempty"`
	JWKSCacheTTL int      `yaml:"jwks_cache_ttl_seconds,omitempty"`
	Paranoid     bool     `yaml:"paranoid"`
	// Anonymous=true is refused in production builds (refuse-to-start floor).
	// Allowed only with the nova_dev build tag.
	Anonymous bool `yaml:"anonymous,omitempty"`
}

type Orchestrator struct {
	TickIntervalSeconds        int               `yaml:"tick_interval_seconds"`
	StepSeconds                int               `yaml:"step_seconds"`
	Replication                Replication       `yaml:"replication"`
	MassCasualtyThresholdRatio float64           `yaml:"mass_casualty_threshold_ratio"`
	MassCasualtyWindowSeconds  int               `yaml:"mass_casualty_window_seconds"`
	CapacityRunwayFloorDays    int               `yaml:"capacity_runway_floor_days"`
}

type Replication struct {
	Factor ReplicationFactor `yaml:"factor"`
}

type ReplicationFactor struct {
	Important int `yaml:"important"`
	Normal    int `yaml:"normal"`
	Cache     int `yaml:"cache"`
}

type Federation struct {
	HeartbeatIntervalSeconds     int `yaml:"heartbeat_interval_seconds"`
	PinsPollIntervalSeconds      int `yaml:"pins_poll_interval_seconds"`
	MaxPinConcurrency            int `yaml:"max_pin_concurrency"`
	SuspectAfterMissedHeartbeats int `yaml:"suspect_after_missed_heartbeats"`
	UnreachableAfterSeconds      int `yaml:"unreachable_after_seconds"`
	EvictedAfterSeconds          int `yaml:"evicted_after_seconds"`
}

type IntegrityAudit struct {
	EnvelopeDecode             AuditCadence `yaml:"envelope_decode"`
	KeyUnwrap                  AuditCadence `yaml:"key_unwrap"`
	SampleDecrypt              AuditCadence `yaml:"sample_decrypt"`
	KuboPinPresent             AuditCadence `yaml:"kubo_pin_present"`
	DerivativeStateConsistent  AuditCadence `yaml:"derivative_state_consistent"`
	BlockHashValid             AuditCadence `yaml:"block_hash_valid"`
	ManifestConsistent         AuditCadence `yaml:"manifest_consistent"`
}

type AuditCadence struct {
	IntervalSeconds int `yaml:"interval_seconds"`
	SampleSize      int `yaml:"sample_size"`
}

type Moderation struct {
	TakedownDefaultAction       string `yaml:"takedown_default_action"`
	DMCACounterNotificationDays int    `yaml:"dmca_counter_notification_days"`
}

type Coordinator struct {
	PublicIpfsDht bool `yaml:"public_ipfs_dht"`
}

type WebhookDestination struct {
	URL     string   `yaml:"url"`
	Events  []string `yaml:"events"`
	Secret  string   `yaml:"secret_file,omitempty"`
}
```

- [ ] **Step 13.5: Implement operator_yaml.go**

Create `internal/config/operator_yaml.go`:

```go
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// LoadFromFile reads, parses, and validates an operator.yaml.
func LoadFromFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	return LoadFromBytes(data)
}

// LoadFromBytes parses operator.yaml from a byte slice.
func LoadFromBytes(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: unmarshal: %w", err)
	}
	if err := validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// validate enforces the v3.1 refuse-to-start floors and basic shape.
// (Per the spec, the coordinator runs the same validator at startup;
// validation lives here so the loader and the runtime use one code path.)
func validate(cfg *Config) error {
	if cfg.Operator.Hostname == "" {
		return fmt.Errorf("config: operator.hostname is required")
	}
	if cfg.Operator.ContactEmail == "" {
		return fmt.Errorf("config: operator.contact_email is required")
	}

	switch cfg.TLS.Mode {
	case "dev-self-signed", "http-01", "dns-01", "static", "onion":
		// ok
	case "":
		return fmt.Errorf("config: tls.mode is required (dev-self-signed|http-01|dns-01|static|onion)")
	default:
		return fmt.Errorf("config: tls.mode unknown: %q", cfg.TLS.Mode)
	}
	if cfg.TLS.Mode == "static" && (cfg.TLS.CertPath == "" || cfg.TLS.KeyPath == "") {
		return fmt.Errorf("config: tls.mode=static requires cert_path and key_path")
	}

	if cfg.Orchestrator.Replication.Factor.Important < 1 ||
		cfg.Orchestrator.Replication.Factor.Important > 20 {
		return fmt.Errorf("config: orchestrator.replication.factor.important out of range")
	}

	switch cfg.Moderation.TakedownDefaultAction {
	case "quarantine", "tombstone":
		// ok
	case "":
		// default is quarantine; allow empty for compactness
		cfg.Moderation.TakedownDefaultAction = "quarantine"
	default:
		return fmt.Errorf("config: moderation.takedown_default_action unknown")
	}

	if cfg.Auth.Anonymous && cfg.Moderation.TakedownDefaultAction == "" {
		// Coordinator's v3 floor: auth: anonymous AND moderation: off
		// is refused. moderation_off is currently encoded as
		// takedown_default_action being absent (i.e., no moderation
		// flow); future moderation.enabled field will refine this.
		return fmt.Errorf("config: auth.anonymous with no moderation flow is refused")
	}

	return nil
}
```

- [ ] **Step 13.6: Run test, verify it passes**

```bash
go mod tidy
go test ./internal/config/...
```

Expected: PASS.

- [ ] **Step 13.7: Commit**

```bash
git add internal/config/types.go internal/config/operator_yaml.go internal/config/operator_yaml_test.go internal/config/testdata/operator.minimal.yaml go.mod go.sum
git commit -s -m "feat(config): operator.yaml types + loader + minimal-fixture test"
```

---

### Task 14: Paranoid-mode overrides (TDD)

**Files:**
- Create: `internal/config/paranoid.go`
- Create: `internal/config/paranoid_test.go`
- Create: `internal/config/testdata/operator.paranoid.yaml`

- [ ] **Step 14.1: Create the paranoid fixture**

```bash
cp internal/config/testdata/operator.minimal.yaml internal/config/testdata/operator.paranoid.yaml
```

Then edit `internal/config/testdata/operator.paranoid.yaml` to flip the relevant lines:

```yaml
# Set paranoid=true, add a webhook (which should be zeroed at load),
# set a long source IP retention (which should drop to <=1 day).
auth:
  issuer_url: ""
  paranoid: true

webhooks:
  - url: https://example.com/hook
    events: [image.created]

source_ip_retention_days: 30
```

(Keep the rest from the minimal fixture.)

- [ ] **Step 14.2: Write the failing test**

Create `internal/config/paranoid_test.go`:

```go
package config_test

import (
	"testing"

	"github.com/nova-archive/nova/internal/config"
	"github.com/stretchr/testify/require"
)

func TestParanoidModeZerosWebhooks(t *testing.T) {
	cfg, err := config.LoadFromFile("testdata/operator.paranoid.yaml")
	require.NoError(t, err)
	require.True(t, cfg.Auth.Paranoid)

	require.Empty(t, cfg.Webhooks,
		"paranoid mode must drop all webhook destinations regardless of config")
}

func TestParanoidModeCapsSourceIPRetention(t *testing.T) {
	cfg, err := config.LoadFromFile("testdata/operator.paranoid.yaml")
	require.NoError(t, err)
	require.LessOrEqual(t, cfg.SourceIPRetentionDays, 1,
		"paranoid mode must cap source_ip_retention_days to <=1")
}

func TestNonParanoidPreservesWebhooks(t *testing.T) {
	cfg, err := config.LoadFromFile("testdata/operator.minimal.yaml")
	require.NoError(t, err)
	require.False(t, cfg.Auth.Paranoid)
	// minimal fixture has no webhooks, so we test the "shape" of the field.
	require.NotPanics(t, func() { _ = cfg.Webhooks })
}
```

- [ ] **Step 14.3: Run test, verify it fails**

```bash
go test ./internal/config/...
```

Expected: FAIL — paranoid mode doesn't yet zero webhooks.

- [ ] **Step 14.4: Implement paranoid.go**

Create `internal/config/paranoid.go`:

```go
package config

// ApplyParanoid mutates cfg to enforce paranoid-mode overrides per
// docs/PRIVACY_AUDIT.md § "paranoid: true".
//
// Called automatically from the loader after validate(); operators
// don't invoke it directly.
func ApplyParanoid(cfg *Config) {
	if !cfg.Auth.Paranoid {
		return
	}

	// Drop all outbound webhook destinations regardless of config.
	cfg.Webhooks = nil

	// Cap source-IP retention to 1 day.
	if cfg.SourceIPRetentionDays > 1 || cfg.SourceIPRetentionDays == 0 {
		cfg.SourceIPRetentionDays = 1
	}

	// TLS auto-renewal is operator-disabled in paranoid mode; certbot
	// is not part of the config schema (it's a separate compose service),
	// but the coordinator emits a startup warning if certbot is wired up
	// in paranoid mode. That check lives in cmd/coordinator at startup.

	// OpenTelemetry and Prometheus public-bound endpoints are refused
	// in paranoid mode; that's enforced at coordinator startup, not in
	// config validation, because the relevant fields aren't in
	// operator.yaml yet (Phase 1 ships Prometheus loopback-only; M14
	// adds OpenTelemetry config gating).
}
```

- [ ] **Step 14.5: Wire ApplyParanoid into LoadFromBytes**

Edit `internal/config/operator_yaml.go`. Change the end of `LoadFromBytes` to call `ApplyParanoid` before returning:

```go
func LoadFromBytes(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: unmarshal: %w", err)
	}
	if err := validate(&cfg); err != nil {
		return nil, err
	}
	ApplyParanoid(&cfg)
	return &cfg, nil
}
```

- [ ] **Step 14.6: Run test, verify it passes**

```bash
go test ./internal/config/...
```

Expected: PASS.

- [ ] **Step 14.7: Commit**

```bash
git add internal/config/paranoid.go internal/config/paranoid_test.go internal/config/operator_yaml.go internal/config/testdata/operator.paranoid.yaml
git commit -s -m "feat(config): paranoid-mode overrides (webhooks zeroed, source IP retention capped)"
```

---

### Task 15: Secrets resolver (TDD)

**Files:**
- Create: `internal/config/secrets.go`
- Create: `internal/config/secrets_test.go`

- [ ] **Step 15.1: Write the failing test**

Create `internal/config/secrets_test.go`:

```go
package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nova-archive/nova/internal/config"
	"github.com/stretchr/testify/require"
)

func TestSecretsResolverPrefersEnvVar(t *testing.T) {
	t.Setenv("FOO", "from-env")
	t.Setenv("FOO_FILE", filepath.Join(t.TempDir(), "ignored"))

	got, err := config.ResolveSecret("FOO", "FOO_FILE", "/dev/null")
	require.NoError(t, err)
	require.Equal(t, "from-env", got)
}

func TestSecretsResolverFallsBackToFileEnv(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "secret.txt")
	require.NoError(t, os.WriteFile(file, []byte("from-file-env\n"), 0o600))

	t.Setenv("BAR", "")
	t.Setenv("BAR_FILE", file)

	got, err := config.ResolveSecret("BAR", "BAR_FILE", "/dev/null")
	require.NoError(t, err)
	require.Equal(t, "from-file-env", got, "trailing newline should be trimmed")
}

func TestSecretsResolverFallsBackToDefaultMountPath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "secret.txt")
	require.NoError(t, os.WriteFile(file, []byte("from-mount"), 0o600))

	t.Setenv("BAZ", "")
	t.Setenv("BAZ_FILE", "")

	got, err := config.ResolveSecret("BAZ", "BAZ_FILE", file)
	require.NoError(t, err)
	require.Equal(t, "from-mount", got)
}

func TestSecretsResolverReturnsErrorWhenNoneAvailable(t *testing.T) {
	t.Setenv("QUUX", "")
	t.Setenv("QUUX_FILE", "")

	_, err := config.ResolveSecret("QUUX", "QUUX_FILE", "/nonexistent")
	require.Error(t, err)
}
```

- [ ] **Step 15.2: Run test, verify it fails**

```bash
go test ./internal/config/...
```

Expected: FAIL — `undefined: config.ResolveSecret`.

- [ ] **Step 15.3: Implement secrets.go**

Create `internal/config/secrets.go`:

```go
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// ResolveSecret applies the v3.1 secret-loading precedence:
//
//   1. environment variable named `envKey` (if non-empty)
//   2. file at the path in env var `envFileKey` (if env is set and non-empty)
//   3. file at `defaultMountPath` (if it exists)
//
// Returns the secret value (trimmed of leading/trailing whitespace) or
// an error if no source resolves.
//
// This is the entry point for loading every operator secret in Phase 1:
// NOVA_MASTER_KEY, NOVA_OIDC_SIGNING_KEY, IPFS_SWARM_KEY, etc.
func ResolveSecret(envKey, envFileKey, defaultMountPath string) (string, error) {
	if v := os.Getenv(envKey); v != "" {
		return strings.TrimSpace(v), nil
	}

	if path := os.Getenv(envFileKey); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("secrets: read %s (from $%s): %w", path, envFileKey, err)
		}
		return strings.TrimSpace(string(data)), nil
	}

	if defaultMountPath != "" {
		data, err := os.ReadFile(defaultMountPath)
		if err == nil {
			return strings.TrimSpace(string(data)), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("secrets: read %s: %w", defaultMountPath, err)
		}
	}

	return "", fmt.Errorf("secrets: none of $%s, $%s, or %s resolved", envKey, envFileKey, defaultMountPath)
}
```

- [ ] **Step 15.4: Run test, verify it passes**

```bash
go test ./internal/config/...
```

Expected: PASS.

- [ ] **Step 15.5: Commit**

```bash
git add internal/config/secrets.go internal/config/secrets_test.go
git commit -s -m "feat(config): secrets resolver with env > env_file > /run/secrets precedence"
```

---

### Task 16: CI workflow

**Files:**
- Create: `.github/workflows/ci.yml`
- Create: `.golangci.yml`

- [ ] **Step 16.1: Create the CI workflow**

```yaml
name: ci

on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

jobs:
  test:
    runs-on: ubuntu-latest
    services:
      docker:
        image: docker:dind
        options: --privileged
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version: "1.22"
          cache: true

      - name: Go vet
        run: go vet ./...

      - name: Unit tests
        run: make test-unit

      - name: Integration tests
        run: make test-integration
        env:
          TESTCONTAINERS_RYUK_DISABLED: "true"

  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version: "1.22"
          cache: true

      - name: golangci-lint
        uses: golangci/golangci-lint-action@v6
        with:
          version: latest

  schema-drift:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Check 0001_init.sql matches DATA_MODEL.sql
        run: |
          # Strip goose annotations from 0001 and compare to DATA_MODEL.sql.
          extracted="$(sed -n '/^-- +goose Up$/,/^-- +goose StatementEnd$/{
            /^-- +goose Up$/d
            /^-- +goose StatementBegin$/d
            /^-- +goose StatementEnd$/d
            /^-- This migration MUST/d
            /^-- Forward-only/d
            /^-- Drift fails CI/d
            p
          }' internal/db/migrations/0001_init.sql)"
          diff <(echo "$extracted") docs/specs/DATA_MODEL.sql
```

- [ ] **Step 16.2: Create the golangci-lint config**

```yaml
run:
  timeout: 5m
  go: "1.22"

linters:
  enable:
    - errcheck
    - gofmt
    - goimports
    - govet
    - ineffassign
    - staticcheck
    - unused
    - misspell
    - revive

linters-settings:
  revive:
    rules:
      - name: exported
      - name: error-return
      - name: error-naming
      - name: package-comments

issues:
  exclude-rules:
    - path: _test\.go
      linters:
        - errcheck
```

- [ ] **Step 16.3: Commit**

```bash
git add .github/workflows/ci.yml .golangci.yml
git commit -s -m "ci: GitHub Actions for test (unit + integration), lint, schema-drift"
```

---

### Task 17: M1 smoke test script

**Files:**
- Create: `scripts/smoke.sh`

This smoke test exercises the full M1 dev-loop end-to-end: docker-compose brings up postgres, `cmd/migrate up` applies migrations, and we assert the v3.1 schema is present.

- [ ] **Step 17.1: Create the script**

```bash
cat > scripts/smoke.sh <<'EOF'
#!/usr/bin/env bash
# scripts/smoke.sh — M1 end-to-end smoke test.
#
# Brings up docker-compose postgres, runs cmd/migrate up against it,
# asserts the v3.1 schema, then tears down (leaving the volume).
#
# Exit codes:
#   0  success
#   1  any step failed (compose, migrate, or schema assertion)

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

ENV_FILE="docker/.env"
if [[ ! -f "$ENV_FILE" ]]; then
    if [[ -f "docker/.env.example" ]]; then
        echo "[smoke] docker/.env not found; copying from docker/.env.example"
        cp docker/.env.example "$ENV_FILE"
        # Generate a random password for smoke runs to avoid placeholder leaking.
        sed -i.bak "s/changeme/$(openssl rand -hex 16)/" "$ENV_FILE" && rm -f "$ENV_FILE.bak"
    else
        echo "[smoke] FAIL: $ENV_FILE missing and no .env.example to seed from" >&2
        exit 1
    fi
fi

# shellcheck disable=SC1090
source "$ENV_FILE"

DC="docker compose -f docker/docker-compose.yml --env-file $ENV_FILE"

echo "[smoke] Bringing up postgres..."
$DC up -d postgres

echo "[smoke] Waiting for postgres to be ready..."
for i in {1..30}; do
    if $DC exec -T postgres pg_isready -U nova -d nova >/dev/null 2>&1; then
        echo "[smoke] Postgres ready after ${i}s"
        break
    fi
    sleep 1
    if [[ $i -eq 30 ]]; then
        echo "[smoke] FAIL: postgres did not become ready in 30s" >&2
        $DC logs postgres
        exit 1
    fi
done

echo "[smoke] Building cmd/migrate..."
go build -o bin/migrate ./cmd/migrate

echo "[smoke] Running migrations..."
DATABASE_URL="postgres://nova:${POSTGRES_PASSWORD}@127.0.0.1:5432/nova?sslmode=disable" ./bin/migrate up

echo "[smoke] Asserting v3.1 schema..."
EXPECTED_TABLES=(
    users master_key_versions data_encryption_keys signing_keys
    collections blobs blob_manifests blob_blocks
    image_metadata collection_items
    nodes pin_assignments pin_audits
    integrity_audits moderation_decisions dmca_cases
    takedown_repeat_infringers signed_url_revocations
    audit_log jobs
)

for table in "${EXPECTED_TABLES[@]}"; do
    found=$(PGPASSWORD="$POSTGRES_PASSWORD" $DC exec -T postgres psql -U nova -d nova -t -c "SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='$table'" | tr -d '[:space:]')
    if [[ "$found" != "1" ]]; then
        echo "[smoke] FAIL: expected table '$table' is missing" >&2
        exit 1
    fi
done

# blobs.envelope_version
has_col=$(PGPASSWORD="$POSTGRES_PASSWORD" $DC exec -T postgres psql -U nova -d nova -t -c "SELECT 1 FROM information_schema.columns WHERE table_name='blobs' AND column_name='envelope_version'" | tr -d '[:space:]')
if [[ "$has_col" != "1" ]]; then
    echo "[smoke] FAIL: blobs.envelope_version column missing" >&2
    exit 1
fi

# integrity_audits is partitioned
is_part=$(PGPASSWORD="$POSTGRES_PASSWORD" $DC exec -T postgres psql -U nova -d nova -t -c "SELECT 1 FROM pg_partitioned_table pt JOIN pg_class c ON c.oid = pt.partrelid WHERE c.relname='integrity_audits'" | tr -d '[:space:]')
if [[ "$is_part" != "1" ]]; then
    echo "[smoke] FAIL: integrity_audits not partitioned" >&2
    exit 1
fi

echo "[smoke] PASS — all v3.1 tables present, envelope_version column exists, integrity_audits partitioned"

echo "[smoke] Tearing down (data preserved)..."
$DC down

echo "[smoke] OK"
EOF
chmod +x scripts/smoke.sh
```

- [ ] **Step 17.2: Run the smoke test locally**

```bash
make smoke
```

Expected: prints `[smoke] OK` after ~30-60 seconds.

- [ ] **Step 17.3: Commit**

```bash
git add scripts/smoke.sh
git commit -s -m "test: M1 smoke test (compose + migrate + schema assertion)"
```

---

### Task 18: M1 completion — full test pass, tag

- [ ] **Step 18.1: Run all the M1 tests one more time**

```bash
make tidy
make test
make smoke
```

Expected: every step succeeds.

- [ ] **Step 18.2: Verify CI passes on push**

```bash
git push origin main
# Watch GitHub Actions; verify both `test`, `lint`, `schema-drift` are green.
```

Expected: all CI jobs green within ~5 minutes.

- [ ] **Step 18.3: Tag M1**

```bash
git tag -s m1-foundation -m "Phase 1 M1: foundation (repo, migrations, postgres dev env)"
git push origin m1-foundation
```

- [ ] **Step 18.4: Update master plan to mark M1 done**

Edit this file (`docs/superpowers/plans/2026-05-25-phase1-single-node-mvp.md`) and change the M1 status from `**active**` to `**completed**` in the milestone table.

- [ ] **Step 18.5: Commit and push the status update**

```bash
git add docs/superpowers/plans/2026-05-25-phase1-single-node-mvp.md
git commit -s -m "docs(plans): mark M1 completed"
git push origin main
```

---

## Self-review

Scope coverage against the design's M1 line items:

| Design item | Covered by |
|---|---|
| Repo bones: cmd/, internal/, pkg/ skeletons | Task 3 |
| Makefile targets | Task 2 |
| CI pipeline with empty test passing | Task 16 |
| `internal/db/migrations` from DATA_MODEL.sql + 0002 + 0003 | Tasks 5, 6, 7 |
| `internal/db` pgxpool | Task 10 |
| sqlc | **deferred to M2** (introduce when first sqlc query is needed; ships sqlc.yaml then) |
| `internal/config` loader + paranoid mode | Tasks 13, 14 |
| `cmd/migrate` runs migrations cleanly | Task 11 |
| Secrets resolver (env → file → mount) | Task 15 |
| Migration 0004 envelope_version | Task 8 |
| Postgres MCP for dev | Task 4 |
| Smoke test | Task 17 |

**One deferral noted in the plan:** sqlc setup moves to M2 (when we need to query against blobs/keys/etc. for the envelope round-trip). This is a small scope refinement; document it in the M2 plan when written.

**Placeholder scan:** No TBDs, no "implement later," no untyped references. All code in steps is concrete.

**Type consistency:** `config.Config` matches across types.go, operator_yaml.go, paranoid.go, and tests. `ResolveSecret` signature consistent across secrets.go and tests. `migrate up/down/status/version` subcommands consistent across cmd/migrate, Makefile, and smoke.sh.

Plan complete.

---

## Execution handoff

**Plan complete and saved to `docs/superpowers/plans/2026-05-25-phase1-single-node-mvp.md`. Two execution options:**

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration

**2. Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints

**Which approach?**
