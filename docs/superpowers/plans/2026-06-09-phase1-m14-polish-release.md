# M14 Polish, Security Housecleaning, and Release Candidate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close out Phase 1: repair the two red CI jobs (golangci-lint v2 migration; schema-drift → migration-immutability), patch/triage all Dependabot advisories (two Go runtime bumps; Vite-7/Vitest-3 toolchain jump on Node 22), institutionalize toolchain currency, ship the full-stack CI smoke test, finish the M13 certbot deferral, add compose hardening floors, write `docs/quickstart.md`, reconcile all Phase-1 docs, and tag `v0.1.0-rc1`.

**Architecture:** No new Go packages — M14 is scripts + CI + config + docs. Ordering is CI-green-first: restore the lint and schema gates before touching anything they guard, then dependencies, then the smoke/certbot/hardening/docs stack. The smoke test exercises the *shipped compose artifact* (image build → headless `novactl setup` → prod profile → upload/read/transform/delete through nginx), complementing the in-process testcontainers suite.

**Tech Stack:** golangci-lint v2, GitHub Actions, sha256sum, Go modules (quic-go/otel bumps), npm workspaces + Vite 7 + Vitest 3.x + Node 22, docker compose, certbot (webroot http-01), nginx, bash.

**Spec:** `docs/superpowers/specs/2026-06-09-phase1-m14-polish-release-design.md`

**Conventions (from prior milestones):**
- gofmt only the Go files you touch (toolchain-skew rule); golangci-lint is CI-side (exception: Task 1 validates the v2 config with a locally downloaded binary once).
- Conventional commits, `(m14)` suffix (e.g. `fix(ci): … (m14)`). End commit bodies with the `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` trailer.
- Work on branch `m14-polish-release` (already created; the design doc is committed there).
- Integration tests stay `-short`-skippable; nothing in this plan changes them.
- Verify before claiming done: every task ends with its own verification command(s).

---

## File structure

**Created:**
- `.github/dependabot.yml` — weekly grouped updates: gomod, npm, github-actions.
- `.nvmrc` — `22`.
- `internal/db/migrations/MANIFEST.sha256` — `sha256sum`-format manifest of shipped migrations.
- `scripts/check-migrations-frozen.sh` — both-directions manifest verification.
- `docker/nginx/cert-watch.sh` — placeholder-cert bootstrap + renewal reload watcher (http-01).
- `docs/quickstart.md` + `docs/images/quickstart/` — screenshot operator quickstart.

**Modified:**
- `.golangci.yml` — v2 schema (via `golangci-lint migrate`).
- `.github/workflows/ci.yml` — lint action v8; `go-version-file`; `node-version-file`; `schema-drift` job → `migrations-frozen`; new `smoke` job.
- `internal/db/migrations/0001_init.sql` — header comment only (before manifest generation).
- `go.mod` / `go.sum` — quic-go v0.59.1, otlptracehttp v1.43.0.
- `web/{admin,widget,setup}/package.json` (+ `vite.config.ts` where API drift requires) — Vite 7 / Vitest 3.x line.
- `package.json` (`engines: >=22`) / `package-lock.json` (regenerated).
- `docker/Dockerfile` — `node:22-bookworm`.
- `docker/docker-compose.yml` — certbot initial issuance; nginx cert-watch command; healthchecks; `read_only`/`cap_drop`/`no-new-privileges`.
- `docker/init/entrypoint.sh` — only if cert-path ownership requires (Task 8).
- `scripts/smoke.sh` — full-stack e2e.
- `Makefile` — `migrations-frozen` target; smoke help text.
- `CONTRIBUTING.md` — toolchain-currency policy.
- Docs: `README.md`, `docs/ROADMAP.md`, master plan, M13 spec (one line), `docs/VOLUNTEER_DEPLOYMENT_GUIDANCE.md`, `docs/recipes/NGINX_REFERENCE.md`.

**Reused unchanged:** `scripts/hermetic-spa.sh` + hermetic Make targets; the integration suite; `internal/setup` (unless Task 8 finds the template needs a path change); `docs/legal/OPERATOR_CHECKLIST.md` (linked from quickstart).

---

## Task 1: golangci-lint v2 migration + Go version unskew

**Files:**
- Modify: `.golangci.yml`
- Modify: `.github/workflows/ci.yml` (lint + test jobs)

- [ ] **Step 1: Download a golangci-lint v2 binary locally (one-time validation tool).**

```bash
curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh \
  | sh -s -- -b /tmp/golangci-v2
/tmp/golangci-v2/golangci-lint version
```

Expected: a `v2.x.y` version line. (If the installer requires an explicit version, pass the latest `v2.*` tag shown at https://github.com/golangci/golangci-lint/releases.)

- [ ] **Step 2: Migrate the config mechanically.**

```bash
cd /home/archbug/projects/ipfs_img_self_host
/tmp/golangci-v2/golangci-lint migrate
```

This rewrites `.golangci.yml` to the v2 schema. Expected shape (the migrate output is authoritative; verify it expresses the same intent):

```yaml
version: "2"
run:
  timeout: 5m
linters:
  default: none
  enable:
    - errcheck
    - govet
    - ineffassign
    - misspell
    - revive
    - staticcheck
    - unused
  settings:
    revive:
      rules:
        - name: exported
        - name: error-return
        - name: error-naming
        - name: package-comments
  exclusions:
    rules:
      - path: _test\.go
        linters:
          - errcheck
formatters:
  enable:
    - gofmt
    - goimports
```

Key checks: `version: "2"` present; `gofmt`/`goimports` now under `formatters`; the `_test.go` errcheck exclusion survived; **no `run.go:` key remains** (the target now derives from `go.mod`, which is the bug being fixed).

- [ ] **Step 3: Verify the config and run the linter.**

```bash
/tmp/golangci-v2/golangci-lint config verify
/tmp/golangci-v2/golangci-lint run ./... 2>&1 | tee /tmp/lint-v2-findings.txt
```

Expected: `config verify` exits 0. The run may surface new findings (staticcheck/revive evolved across two majors).

- [ ] **Step 4: Burn down findings per the design policy.**

Fix trivial findings (unused vars, misspellings, obvious staticcheck simplifications) in the files reported. For noisy-but-harmless classes, add a narrowly-scoped exclusion to `.golangci.yml` with a one-line comment explaining why. Do **not** mass-rewrite untouched code. Re-run Step 3 until clean. `gofmt -l` any Go file you touched.

- [ ] **Step 5: Update the CI workflow.**

In `.github/workflows/ci.yml`, in **both** the `test` and `lint` jobs, replace:

```yaml
      - uses: actions/setup-go@v5
        with:
          go-version: "1.25"
          cache: true
```

with:

```yaml
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
```

and replace the lint step:

```yaml
      - name: golangci-lint
        uses: golangci/golangci-lint-action@v6
        with:
          version: latest
```

with:

```yaml
      - name: golangci-lint
        uses: golangci/golangci-lint-action@v8
        with:
          version: latest
```

- [ ] **Step 6: Verify the Go suite still passes (config changes only, but cheap insurance).**

```bash
go vet ./... && make test-unit
```

Expected: PASS.

- [ ] **Step 7: Commit.**

```bash
git add .golangci.yml .github/workflows/ci.yml <any files touched in step 4>
git commit -m "fix(ci): migrate golangci-lint config to v2; derive Go version from go.mod (m14)"
```

---

## Task 2: migration-immutability check (replaces schema-drift)

**Files:**
- Modify: `internal/db/migrations/0001_init.sql` (header comment **first** — the manifest hashes the final content)
- Create: `internal/db/migrations/MANIFEST.sha256`
- Create: `scripts/check-migrations-frozen.sh`
- Modify: `Makefile`, `.github/workflows/ci.yml`

- [ ] **Step 1: Fix the contradictory 0001 header.**

In `internal/db/migrations/0001_init.sql`, replace lines 3–5:

```sql
-- Forward-only migration 0001: initial schema per docs/specs/DATA_MODEL.sql
-- This migration MUST remain bit-identical to docs/specs/DATA_MODEL.sql.
-- Drift fails CI (see .github/workflows/ci.yml: schema-drift check).
```

with:

```sql
-- Forward-only migration 0001: initial schema (Phase-0 v2 baseline).
-- Shipped migrations are immutable: any edit to a file listed in
-- MANIFEST.sha256 fails CI (scripts/check-migrations-frozen.sh).
-- docs/specs/DATA_MODEL.sql is the annotated living schema REFERENCE;
-- this directory (goose, read by sqlc) is the authoritative schema.
```

Also lines 10–12 of the embedded DATA_MODEL header inside 0001 ("This file is the authoritative schema. Production migrations … derive from this file") stay as-is — they are part of the frozen historical content; only the goose preamble above them (which is *about* the CI check) changes. The old CI `sed` filter that stripped these lines is being deleted in Step 5, so nothing else depends on their exact text.

- [ ] **Step 2: Write the check script.**

Create `scripts/check-migrations-frozen.sh` (mode 755):

```bash
#!/usr/bin/env bash
# scripts/check-migrations-frozen.sh — shipped goose migrations are immutable.
#
# Verifies both drift directions against internal/db/migrations/MANIFEST.sha256:
#   1. every file listed in the manifest exists and hash-matches (no edits);
#   2. every NNNN_*.sql migration on disk is listed (no unlisted migrations).
#
# Adding a new migration 00NN_x.sql:
#   (cd internal/db/migrations && sha256sum 00NN_x.sql >> MANIFEST.sha256)
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
MIG_DIR="internal/db/migrations"
MANIFEST="MANIFEST.sha256"

if [[ ! -f "$MIG_DIR/$MANIFEST" ]]; then
    echo "FAIL: $MIG_DIR/$MANIFEST missing" >&2
    exit 1
fi

# Direction 1: listed files unchanged.
if ! (cd "$MIG_DIR" && sha256sum --check --strict --quiet "$MANIFEST"); then
    echo "FAIL: a shipped migration was edited or deleted." >&2
    echo "Shipped migrations are forward-only and immutable; write a new" >&2
    echo "migration instead of editing an applied one." >&2
    exit 1
fi

# Direction 2: every migration on disk is listed.
status=0
for f in "$MIG_DIR"/[0-9][0-9][0-9][0-9]_*.sql; do
    base="$(basename "$f")"
    if ! grep -qE "^[0-9a-f]{64}  $base$" "$MIG_DIR/$MANIFEST"; then
        echo "FAIL: $base is not in $MANIFEST — append it:" >&2
        echo "  (cd $MIG_DIR && sha256sum $base >> $MANIFEST)" >&2
        status=1
    fi
done
exit "$status"
```

- [ ] **Step 3: Generate the manifest (after the Step-1 header fix) and self-test the script.**

```bash
cd internal/db/migrations && sha256sum 0*.sql > MANIFEST.sha256 && cd ../../..
cat internal/db/migrations/MANIFEST.sha256   # expect 9 lines, 0001..0009
./scripts/check-migrations-frozen.sh && echo OK
```

Expected: `OK`. Now prove both failure directions:

```bash
# Tamper: must fail.
echo "-- tamper" >> internal/db/migrations/0002_jobs.sql
./scripts/check-migrations-frozen.sh; echo "exit=$?"        # expect exit=1
git checkout internal/db/migrations/0002_jobs.sql

# Unlisted migration: must fail.
touch internal/db/migrations/9999_fake.sql
./scripts/check-migrations-frozen.sh; echo "exit=$?"        # expect exit=1
rm internal/db/migrations/9999_fake.sql

./scripts/check-migrations-frozen.sh && echo OK             # green again
```

- [ ] **Step 4: Add the Make target.**

In `Makefile`: add `migrations-frozen` to the `.PHONY` line, a help line (`@echo "  migrations-frozen  Verify shipped migrations are unmodified (MANIFEST.sha256)"`), and:

```make
migrations-frozen:
	./scripts/check-migrations-frozen.sh
```

- [ ] **Step 5: Replace the CI job.**

In `.github/workflows/ci.yml`, delete the entire `schema-drift` job (the `sed`/`diff` block) and add:

```yaml
  migrations-frozen:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Shipped migrations are immutable
        run: make migrations-frozen
```

- [ ] **Step 6: Verify migrations still apply (header comment is inside the goose Up block — prove goose still parses).**

```bash
go test ./internal/db/migrations/ -v
```

Expected: PASS (the existing `migrations_test.go` exercises parse/apply).

- [ ] **Step 7: Commit.**

```bash
git add internal/db/migrations/0001_init.sql internal/db/migrations/MANIFEST.sha256 \
        scripts/check-migrations-frozen.sh Makefile .github/workflows/ci.yml
git commit -m "fix(ci): replace dead schema-drift diff with migration-immutability manifest check (m14)"
```

---

## Task 3: Go dependency security bumps (quic-go, otel exporter)

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Bump the two advisories' modules.**

```bash
go get github.com/quic-go/quic-go@v0.59.1
go get go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp@v1.43.0
go mod tidy
```

Note: both are transitive (kubo/boxo); `go get` records them as pinned indirect requirements, which is the intended module-graph-minimum raise. If `go mod tidy` complains about otel sibling modules (otlptrace, otlptracegrpc, sdk) needing matching versions, bump those siblings to v1.43.0 too.

- [ ] **Step 2: Verify the resolved versions.**

```bash
go list -m github.com/quic-go/quic-go go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp
```

Expected: `… v0.59.1` and `… v1.43.0`.

- [ ] **Step 3: Run the full Go suite (quic-go rides under Kubo's libp2p — the integration paths exercise it).**

```bash
go build ./... && make test-unit && make test-integration
```

Expected: PASS.

- [ ] **Step 4: Commit.**

```bash
git add go.mod go.sum
git commit -m "fix(deps): quic-go v0.59.1 (CVE-2026-40898), otlptracehttp v1.43.0 (CVE-2026-39882) (m14)"
```

---

## Task 4: npm toolchain upgrade (Vite 7 / Vitest 3.x) + Node 22

**Files:**
- Create: `.nvmrc`
- Modify: `web/admin/package.json`, `web/widget/package.json`, `web/setup/package.json` (+ their `vite.config.ts` if API drift requires)
- Modify: `package.json`, `package-lock.json` (regenerated)
- Modify: `.github/workflows/ci.yml` (web-admin job), `docker/Dockerfile`

- [ ] **Step 1: Pin Node 22 declaratively.**

```bash
echo "22" > .nvmrc
```

In root `package.json`, change `"engines": { "node": ">=20" }` → `"node": ">=22"`.

- [ ] **Step 2: Confirm the local toolchain (Bug runs Node 22.22.3 / npm 10.9.8 as nvm default).**

```bash
node -v && npm -v
```

Expected: `v22.x`.

- [ ] **Step 3: Discover current stable majors before installing** (the design fixes floors, not exact versions: Vite ≥ 6.4.2 patched floor → target current major; Vitest ≥ 3.2.6 floor).

```bash
npm view vite version && npm view vitest version && npm view @vitejs/plugin-react version && npm view jsdom version
```

Record the majors; use `@latest` below (it resolves to these).

- [ ] **Step 4: Upgrade the React SPAs (admin + setup share identical pins).**

```bash
npm install -D -w web/admin -w web/setup \
  vite@latest vitest@latest @vitejs/plugin-react@latest jsdom@latest
```

- [ ] **Step 5: Upgrade the widget (no React plugin; keep the CSS-injection plugin current).**

```bash
npm install -D -w web/widget \
  vite@latest vitest@latest jsdom@latest vite-plugin-css-injected-by-js@latest
```

- [ ] **Step 6: Run every web gate; fix config drift as it surfaces.**

```bash
make admin-lint  && make admin-test  && make admin-build  && make hermetic-spa
make widget-lint && make widget-test && make widget-build && make hermetic-widget
make setup-lint  && make setup-test  && make setup-build  && make hermetic-setup
```

Known drift surface for Vite 4→7 / Vitest 0.34→3.x (fix only what the failures demand):
- `vitest` config: the `test` key and `environment: 'jsdom'` survive; `deps.inline` became `server.deps.inline`; globals behavior unchanged.
- `@vitejs/plugin-react` v5 requires no config change for plain React 18 apps.
- Widget `build.lib` IIFE: the entry-filename and global-name contract (`NovaUploadWidget`) is asserted by the existing M12 tests — if the new Rollup emits different asset names, pin them via `build.rollupOptions.output.entryFileNames` to preserve the M12 stable-entry contract rather than relaxing the test.
- Vite 7 raises the default build target; the hermetic gates and tests, not the bundler defaults, are the contract — accept the new target unless a test fails.
- If `@testing-library/react@14` breaks under the new Vitest, bump it (`npm install -D -w web/admin -w web/setup @testing-library/react@latest @testing-library/jest-dom@latest`).

Expected end state: all 12 commands PASS.

- [ ] **Step 7: Verify the alert set is actually closed in the regenerated lock.**

```bash
npm ls vite vitest esbuild nanoid | head -40
grep -c '"vite": "4' package-lock.json   # expect 0
```

Expected: vite ≥ 7, vitest ≥ 3.2.6, esbuild ≥ 0.25, no nanoid in (≥4.0.0 <5.0.9) or (<3.3.8).

- [ ] **Step 8: Move CI and the Docker node-builder to Node 22.**

`.github/workflows/ci.yml` `web-admin` job:

```yaml
      - uses: actions/setup-node@v4
        with:
          node-version-file: .nvmrc
          cache: npm
```

`docker/Dockerfile` line 17: `FROM node:20-bookworm AS node-builder` → `FROM node:22-bookworm AS node-builder`.

- [ ] **Step 9: Prove the image still builds with the new builder + toolchain.**

```bash
make docker-build
```

Expected: image builds; the node-builder stage compiles all three bundles.

- [ ] **Step 10: Commit.**

```bash
git add .nvmrc package.json package-lock.json web/admin web/widget web/setup \
        .github/workflows/ci.yml docker/Dockerfile
git commit -m "fix(deps): Vite 7 + Vitest 3 toolchain on Node 22; closes all npm Dependabot advisories (m14)"
```

---

## Task 5: toolchain currency as standard process

**Files:**
- Create: `.github/dependabot.yml`
- Modify: `CONTRIBUTING.md`

- [ ] **Step 1: Create `.github/dependabot.yml`.**

```yaml
# Dependabot keeps the three ecosystems current. Alerts and update PRs are
# triaged per-milestone (see CONTRIBUTING.md "Toolchain currency"), never
# auto-merged.
version: 2
updates:
  - package-ecosystem: gomod
    directory: /
    schedule:
      interval: weekly
    groups:
      go-minor-patch:
        update-types: [minor, patch]

  - package-ecosystem: npm
    directory: /
    schedule:
      interval: weekly
    groups:
      npm-minor-patch:
        update-types: [minor, patch]

  - package-ecosystem: github-actions
    directory: /
    schedule:
      interval: weekly
```

- [ ] **Step 2: Add the policy to `CONTRIBUTING.md`** (new section, after whatever build/toolchain prose exists — read the file first and place it where toolchain setup is discussed):

```markdown
## Toolchain currency

Staying current is treated as routine maintenance, not incident response:

- **Node** tracks the active LTS line (`.nvmrc` is authoritative; CI uses
  `node-version-file`, the Docker node-builder pins the same major). When a
  Node LTS transition happens, the bump lands within the next milestone.
- **Go** — the `go.mod` directive tracks current stable Go; CI derives its
  version from `go.mod` (`go-version-file`), never a workflow literal.
- **Dependabot** (`.github/dependabot.yml`) watches gomod, npm, and
  github-actions weekly. Triage of alerts and grouped update PRs is part of
  every milestone's definition of done: determine reachability on Nova,
  record the verdict (see the M14 design's triage table for the format), and
  patch — exploitable or not — unless a bump is genuinely breaking.
```

- [ ] **Step 3: Validate the YAML parses.**

```bash
python3 -c "import yaml,sys; yaml.safe_load(open('.github/dependabot.yml')); print('OK')"
```

Expected: `OK`.

- [ ] **Step 4: Commit.**

```bash
git add .github/dependabot.yml CONTRIBUTING.md
git commit -m "chore(ci): dependabot config + toolchain-currency policy (m14)"
```

---

## Task 6: full-stack smoke script

**Files:**
- Modify: `scripts/smoke.sh` (extend; keep the env-seeding bones)
- Modify: `Makefile` (help text only: "End-to-end smoke: image build + compose prod + upload/read/transform/delete")

The flow (design § "The smoke-test flow"): build image → headless setup with `dev-self-signed` TLS + `public_uploads: true` (+ ToS URL, the T1.20 floor) → `--profile prod up` → exercise through nginx on both vhosts → teardown.

- [ ] **Step 1: Read the seams the script drives (5 min, no changes).** Confirm in the working tree: compose service/volume names and published ports (`docker/docker-compose.yml`: public 8442/8443, admin 127.0.0.1:8445), the coordinator image's binary paths (`/usr/local/bin/{coordinator,novactl,migrate}` per `docker/Dockerfile`), and the `Answers` YAML keys (`internal/setup/answers.go`: `hostname`, `contact_email`, `admin_email`, `admin_password`, `tls_mode`, `auth_mode`, `public_uploads`, `tos_url`, `paranoid`).

- [ ] **Step 2: Rewrite `scripts/smoke.sh`.** Keep the existing shebang/`set -euo pipefail`/ROOT-cd/`.env` seeding preamble; replace the body from "Bringing up postgres" down with:

```bash
HOST="smoke.nova.test"
ADMIN_PW="$(openssl rand -hex 12)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"; $DC --profile prod down -v --remove-orphans >/dev/null 2>&1 || true' EXIT

echo "[smoke] Building the coordinator image..."
$DC build coordinator

echo "[smoke] Bringing up postgres..."
$DC up -d postgres
for i in {1..30}; do
    $DC exec -T postgres pg_isready -U nova -d nova >/dev/null 2>&1 && break
    [ "$i" = 30 ] && { echo "[smoke] FAIL: postgres not ready" >&2; exit 1; }
    sleep 1
done

echo "[smoke] Headless setup (novactl setup --config-file)..."
cat > "$TMP/answers.yaml" <<EOF
hostname: $HOST
contact_email: smoke@example.invalid
admin_email: operator@example.invalid
admin_password: $ADMIN_PW
tls_mode: dev-self-signed
auth_mode: local
public_uploads: true
tos_url: https://$HOST/tos
paranoid: false
EOF
# One-off container: migrate, then commit the first-run answers into the
# compose volumes (config/secrets/nginx). Runs as root; the entrypoint's
# chown -R on the next boot fixes ownership.
$DC run --rm --entrypoint /bin/sh \
    -v "$TMP/answers.yaml:/answers.yaml:ro" \
    coordinator -c "/usr/local/bin/migrate up && /usr/local/bin/novactl setup --config-file /answers.yaml"

echo "[smoke] Bringing up the prod profile..."
$DC --profile prod up -d

echo "[smoke] Waiting for the public vhost..."
CURL_PUB=(curl -ksS --resolve "$HOST:8443:127.0.0.1")
CURL_ADM=(curl -ksS --resolve "$HOST:8445:127.0.0.1")
for i in {1..60}; do
    "${CURL_PUB[@]}" -o /dev/null -w '%{http_code}' "https://$HOST:8443/health" 2>/dev/null | grep -q 200 && break
    [ "$i" = 60 ] && { echo "[smoke] FAIL: public /health never went 200" >&2; $DC --profile prod logs --tail 50; exit 1; }
    sleep 2
done

echo "[smoke] 1/5 upload (anonymous multipart; T1.20 public-uploads floor)..."
python3 - "$TMP/fixture.png" <<'PYEOF'
import struct, sys, zlib
def chunk(t, d):
    return struct.pack(">I", len(d)) + t + d + struct.pack(">I", zlib.crc32(t + d) & 0xffffffff)
w = h = 16
raw = b"".join(b"\x00" + b"\xc8\x32\x32" * w for _ in range(h))
png = (b"\x89PNG\r\n\x1a\n"
       + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0))
       + chunk(b"IDAT", zlib.compress(raw))
       + chunk(b"IEND", b""))
open(sys.argv[1], "wb").write(png)
PYEOF
CID="$("${CURL_PUB[@]}" -F "file=@$TMP/fixture.png;type=image/png" \
        "https://$HOST:8443/api/v1/blobs" | python3 -c 'import json,sys; print(json.load(sys.stdin)["cid"])')"
[ -n "$CID" ] || { echo "[smoke] FAIL: no cid from upload" >&2; exit 1; }
echo "[smoke]     cid=$CID"

echo "[smoke] 2/5 read-back byte identity..."
"${CURL_PUB[@]}" -o "$TMP/readback.png" "https://$HOST:8443/blob/$CID"
cmp "$TMP/fixture.png" "$TMP/readback.png" || { echo "[smoke] FAIL: byte mismatch" >&2; exit 1; }

echo "[smoke] 3/5 transform..."
code="$("${CURL_PUB[@]}" -o "$TMP/thumb.png" -w '%{http_code}' "https://$HOST:8443/i/$CID/w8.png")"
[ "$code" = 200 ] || { echo "[smoke] FAIL: transform returned $code" >&2; exit 1; }

echo "[smoke] 4/5 operator login on the admin vhost + soft-delete..."
TOKEN="$("${CURL_ADM[@]}" -H 'Content-Type: application/json' \
    -d "{\"username\":\"operator@example.invalid\",\"password\":\"$ADMIN_PW\"}" \
    "https://$HOST:8445/api/v1/auth/login" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')"
code="$("${CURL_PUB[@]}" -o /dev/null -w '%{http_code}' -X DELETE \
    -H "Authorization: Bearer $TOKEN" "https://$HOST:8443/api/v1/blobs/$CID")"
case "$code" in 2*) ;; *) echo "[smoke] FAIL: delete returned $code" >&2; exit 1;; esac

echo "[smoke] 5/5 soft-deleted blob no longer served..."
code="$("${CURL_PUB[@]}" -o /dev/null -w '%{http_code}' "https://$HOST:8443/blob/$CID")"
case "$code" in 404|410) ;; *) echo "[smoke] FAIL: expected 404/410, got $code" >&2; exit 1;; esac

echo "[smoke] PASS — upload → read → transform → delete proven through the prod stack."
```

Adaptation notes for the implementer (verify, don't assume): the login response field is `access_token` (assert against `internal/auth/localissuer/issuer.go` if extraction fails); `DELETE /api/v1/blobs/{cid}` with the operator token is the M11 owner/admin route — if it returns 403 for non-owner, switch step 4/5 to the admin moderation takedown route (`POST https://$HOST:8445/api/v1/admin/moderation/takedown`) and keep the "no longer served" assertion; if `POST /api/v1/blobs` requires a `collection_id`, add `-F "collection_id=<uuid>"` after creating a collection via the API, or use the simplest accepted anonymous shape per `internal/api/handlers/upload*` — the *assertion set* (byte identity, transform 200, delete → gone) is the contract, the exact route shapes follow the code.

- [ ] **Step 3: Run it locally.**

```bash
make smoke
```

Expected: `[smoke] PASS …`. Iterate on route-shape adaptation notes above until green. On failure, `docker compose -f docker/docker-compose.yml --profile prod logs` is the first diagnostic.

- [ ] **Step 4: Commit.**

```bash
git add scripts/smoke.sh Makefile
git commit -m "feat(smoke): full-stack e2e — image build, headless setup, upload/read/transform/delete through nginx (m14)"
```

---

## Task 7: CI smoke job

**Files:**
- Modify: `.github/workflows/ci.yml`

- [ ] **Step 1: Add the job.**

```yaml
  smoke:
    runs-on: ubuntu-latest
    timeout-minutes: 20
    steps:
      - uses: actions/checkout@v4

      - name: End-to-end smoke (compose prod stack)
        run: make smoke

      - name: Dump compose logs on failure
        if: failure()
        run: docker compose -f docker/docker-compose.yml --profile prod logs --no-color > smoke-logs.txt 2>&1 || true

      - name: Upload smoke logs
        if: failure()
        uses: actions/upload-artifact@v4
        with:
          name: smoke-logs
          path: smoke-logs.txt
```

(ubuntu-latest ships docker + compose v2; no service container needed — the smoke manages its own stack. Note the `trap … down -v` in smoke.sh runs before the log-dump step; if that proves to erase useful logs, move the dump *into* smoke.sh's failure path instead.)

- [ ] **Step 2: Sanity-check workflow syntax.**

```bash
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml')); print('OK')"
```

- [ ] **Step 3: Commit.**

```bash
git add .github/workflows/ci.yml
git commit -m "feat(ci): blocking smoke job against the compose prod stack (m14)"
```

---

## Task 8: certbot completion — initial issuance + deploy-hook reload (M13 deferral)

**Files:**
- Create: `docker/nginx/cert-watch.sh`
- Modify: `docker/docker-compose.yml` (nginx command + mount; certbot command)
- Modify (only if required): `docker/init/entrypoint.sh`, `internal/setup/templates/nova.conf.tmpl`

- [ ] **Step 1: Confirm the http-01 cert paths (10 min, no changes).** Read `internal/setup/templates/nova.conf.tmpl` (the `http-01` branch's `ssl_certificate`/`ssl_certificate_key` directives) and `docker/docker-compose.yml` (which volume carries the certs into both nginx and certbot — if no shared letsencrypt volume exists yet, add one: `nova-letsencrypt:/etc/letsencrypt` on both services, and confirm the template's cert paths point inside it; if the template points elsewhere, prefer adjusting compose mounts over editing the template, which would touch M13's render tests).

- [ ] **Step 2: Write `docker/nginx/cert-watch.sh`** (mode 755) — placeholder bootstrap + reload watcher:

```sh
#!/bin/sh
# docker/nginx/cert-watch.sh — http-01 bootstrap + renewal reload.
#
# 1. If the configured leaf is missing (first boot, before certbot has ever
#    issued), generate a self-signed placeholder so nginx can start and serve
#    the ACME challenge on :80. Certbot then issues the real cert.
# 2. Watch the cert file; when certbot writes a new one (initial issuance or
#    renewal), reload nginx so it actually serves it.
#
# Usage (compose nginx command):  /cert-watch.sh <cert> <key> <hostname>
set -eu
CERT="$1"; KEY="$2"; HOST="$3"

if [ ! -s "$CERT" ]; then
    echo "cert-watch: $CERT missing — generating self-signed placeholder for $HOST"
    mkdir -p "$(dirname "$CERT")" "$(dirname "$KEY")"
    openssl req -x509 -newkey rsa:2048 -nodes -days 7 \
        -keyout "$KEY" -out "$CERT" -subj "/CN=$HOST" \
        -addext "subjectAltName=DNS:$HOST" >/dev/null 2>&1
fi

last="$(sha256sum "$CERT" | cut -d' ' -f1)"
(
    while sleep 60; do
        cur="$(sha256sum "$CERT" 2>/dev/null | cut -d' ' -f1 || true)"
        if [ -n "$cur" ] && [ "$cur" != "$last" ]; then
            echo "cert-watch: certificate changed — reloading nginx"
            nginx -s reload || true
            last="$cur"
        fi
    done
) &

exec nginx -g "daemon off;"
```

- [ ] **Step 3: Wire it into compose.** nginx (prod) service: mount the script (`./nginx/cert-watch.sh:/cert-watch.sh:ro` — path relative to the compose file) and set `command: ["/cert-watch.sh", "<cert path from step 1>", "<key path from step 1>", "${NOVA_HOSTNAME:?}"]`. Only the `http-01` mode needs the watcher, but it is harmless under `dev-self-signed`/`static` (the leaf exists, the watcher just idles) — one command for all modes keeps compose simple.

- [ ] **Step 4: Add initial issuance to the certbot service.** Replace its renew-only loop command with issue-if-missing + renew:

```yaml
    command: >-
      /bin/sh -c '
        if [ ! -s /etc/letsencrypt/live/$${NOVA_HOSTNAME}/fullchain.pem ]; then
          certbot certonly --webroot -w /var/lib/certbot/webroot
            -d $${NOVA_HOSTNAME} --email $${NOVA_CONTACT_EMAIL}
            --agree-tos --non-interactive || true;
        fi;
        while :; do
          certbot renew --webroot -w /var/lib/certbot/webroot --quiet || true;
          sleep 12h;
        done'
```

(Adjust env-var names to whatever `docker/.env.example` already defines — read it first; add `NOVA_HOSTNAME`/`NOVA_CONTACT_EMAIL` there if absent. The `|| true` on certonly keeps non-http-01 deployments from crash-looping the service; the nginx watcher performs the reload, so no deploy-hook cross-container signalling is needed — this *is* the deploy-hook mechanism, volume-mediated.)

- [ ] **Step 5: Verify the dev-mode path end-to-end (CI cannot do real ACME).**

```bash
make smoke
```

Expected: PASS — proving the new nginx command + watcher don't break the `dev-self-signed` boot. The placeholder branch is exercised by deleting the leaf in a scratch run if cheap, otherwise by inspection; the real-ACME staging issuance is a runbook human action (Task 11 documents it).

- [ ] **Step 6: Commit.**

```bash
git add docker/nginx/cert-watch.sh docker/docker-compose.yml docker/.env.example
git commit -m "feat(tls): http-01 initial issuance + cert-watch reload; closes the M13 certbot deferral (m14)"
```

---

## Task 9: container hardening floors

**Files:**
- Modify: `docker/docker-compose.yml`

- [ ] **Step 1: Add healthchecks to every service that lacks one** (postgres already has one). Concretely:
- **coordinator**: the runtime image is Debian-slim without curl/wget — check what the image ships first (`docker run --rm --entrypoint /bin/sh <image> -c 'command -v curl wget'`). If neither exists, add `curl` to the runtime stage's `apt-get install` line in `docker/Dockerfile` (one small, justified addition) and use `healthcheck: test: ["CMD", "curl", "-fsS", "http://127.0.0.1:9000/health"]` with `interval: 30s, timeout: 5s, retries: 3, start_period: 30s` (confirm the coordinator's listen port from compose/`.env.example` — use what `NOVA_LISTEN_ADDR` says, not this plan).
- **nginx** (both `nginx` and `nginx-setup`): `test: ["CMD", "nginx", "-t"]`, same timings.
- **certbot**: a process-liveness check is the honest maximum: `test: ["CMD-SHELL", "pgrep -f certbot || exit 1"]` — or omit with an inline comment that the service is a best-effort loop (decide by what pgrep availability in the image allows).

- [ ] **Step 2: Read-only rootfs + tmpfs.**
- **coordinator**: `read_only: true` + `tmpfs: [/tmp]`; its writable paths (config, secrets, kubo repo, upload tmp) are already named volumes — verify each `NOVA_*_DIR` from the entrypoint is volume-backed, and add `tmpfs` entries for any that aren't.
- **nginx**: `read_only: true` + `tmpfs: [/var/cache/nginx, /var/run, /tmp]` (cert volume stays writable for the placeholder path from Task 8).
- **certbot**: leave writable (`/etc/letsencrypt` churn) — comment why.

- [ ] **Step 3: Capability floors with documented exceptions.** For each service add `security_opt: ["no-new-privileges:true"]` and `cap_drop: [ALL]`, then `cap_add` back only what breaks without it, with an inline comment per exception. Expected exceptions (verify empirically, comment what you keep):
- coordinator: `SETUID`, `SETGID`, `CHOWN` (the entrypoint's root-phase `chown -R` + `gosu` drop), `DAC_OVERRIDE` if volume ownership requires.
- nginx: `NET_BIND_SERVICE`, `SETUID`, `SETGID`, `CHOWN` (master binds :80/:443, workers drop).
- postgres: the official image needs `SETUID`/`SETGID`/`CHOWN`/`FOWNER`/`DAC_OVERRIDE` (initdb + ownership) — add back what its startup demands.
- **Important:** `no-new-privileges` does NOT block `gosu`'s privilege *drop* (only gaining), so the entrypoint keeps working — but prove it, don't trust this sentence.

- [ ] **Step 4: Prove the hardened stack still works — both profiles.**

```bash
make smoke                                            # prod profile, full e2e
docker compose -f docker/docker-compose.yml --profile setup up -d \
  && sleep 15 && curl -s http://127.0.0.1:8444/setup/state | head -c 200 \
  && docker compose -f docker/docker-compose.yml --profile setup down -v
```

Expected: smoke PASS; setup-profile `/setup/state` answers. Iterate cap_add per failure message.

- [ ] **Step 5: Commit.**

```bash
git add docker/docker-compose.yml docker/Dockerfile
git commit -m "feat(docker): hardening floors — healthchecks, read-only rootfs, cap_drop/no-new-privileges (m14)"
```

---

## Task 10: `docs/quickstart.md` + screenshots

**Files:**
- Create: `docs/quickstart.md`, `docs/images/quickstart/` (PNGs)

- [ ] **Step 1: Run the wizard for real** (source of screenshots *and* a cold-read of the actual flow):

```bash
docker compose -f docker/docker-compose.yml --profile setup up -d
# browse http://127.0.0.1:8444/setup/
```

- [ ] **Step 2: Capture screenshots** of: the welcome step, the master-key + fingerprint-readback step, the TLS-mode step, the review step, and the "you're live" orientation page → `docs/images/quickstart/{01-welcome,02-master-key,03-tls-mode,04-review,05-live}.png`. **If no browser/screenshot tooling is available to the implementing agent, write the doc with the image references in place and flag screenshot capture as a human action for Bug in the task report — do not ship broken image links silently; list them.**

- [ ] **Step 3: Write `docs/quickstart.md`.** Audience: an operator with Docker and a clone, zero Nova context; goal: "uploaded and served" without opening another doc; deep material is linked, not duplicated. Required sections (write full prose, not stubs):

```markdown
# Nova Quickstart
What you'll have at the end: a TLS-fronted single-node Nova on Docker, an
operator account, an uploaded image served at /blob and transformed at /i.

## Prerequisites          (Docker + compose v2; a hostname if using http-01;
                           5 GB disk; no Node/Go needed — everything is in the image)
## 1. Clone and start setup mode
    git clone … && cd … 
    cp docker/.env.example docker/.env        # set POSTGRES_PASSWORD
    docker compose -f docker/docker-compose.yml --profile setup up -d
    # open http://127.0.0.1:8444/setup/
## 2. The wizard, step by step                (one short paragraph + screenshot per step;
                           emphasize: the master-key backup is the unrecoverable step —
                           the fingerprint readback exists so you cannot skip it)
## 3. Restart into production
    docker compose -f docker/docker-compose.yml --profile setup down
    docker compose -f docker/docker-compose.yml --profile prod up -d
## 4. First upload         (the widget embed single-<script> snippet from the
                           orientation page; or the curl multipart command from
                           scripts/smoke.sh step 1)
## 5. The admin SPA        (https://<host>:8445/admin — loopback by default, how to open it)
## Choosing a TLS mode     (table: dev-self-signed / static / http-01 automated;
                           dns-01 + onion are operator-handoff — link OPERATOR_CHECKLIST)
## Headless / scripted setup   (novactl setup --config-file answers.yaml — show a
                           complete example answers.yaml, fields per internal/setup/answers.go)
## Next steps              (OPERATOR_CHECKLIST.md as the deep runbook: backups, key
                           rotation, moderation, sentinel re-arming; ROADMAP for what's next)
```

- [ ] **Step 4: Cold-read verification.** Follow your own doc top-to-bottom on a clean checkout (fresh volumes: `docker compose … down -v` first). Every command must work as pasted.

- [ ] **Step 5: Commit.**

```bash
git add docs/quickstart.md docs/images/quickstart/
git commit -m "docs(quickstart): screenshot operator quickstart — clone to served upload (m14)"
```

---

## Task 11: Phase-1 docs reconciliation

**Files:**
- Modify: `docs/ROADMAP.md`, `docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md`, `docs/superpowers/specs/2026-06-08-phase1-m13-setup-wizard-design.md`, `README.md`, `docs/VOLUNTEER_DEPLOYMENT_GUIDANCE.md`, `docs/recipes/NGINX_REFERENCE.md`, `docs/legal/OPERATOR_CHECKLIST.md`

- [ ] **Step 1: `docs/ROADMAP.md`.** Rewrite the M14 row in the completed style of M7–M13: status ✅, tag names (`m14-polish-release`, `v0.1.0-rc1`), one-paragraph summary (CI repairs: golangci-lint v2 + migration-immutability manifest; Dependabot triage outcome — nothing production-exploitable, quic-go/otel patched, Vite-7/Vitest-3/Node-22 toolchain jump; dependabot.yml + currency policy; full-stack CI smoke; http-01 issuance + cert-watch reload; compose hardening floors; quickstart), design/plan doc links, and the deferrals (release signing + seccomp/AppArmor/chaos → Phase 5). Mark the "Phase 1 — Single-node MVP" section complete at `v0.1.0-rc1`.

- [ ] **Step 2: Master plan.** In `2026-05-25-phase1-single-node-mvp-design.md`: mark the M14 milestone section complete with links; change the `docs/operator-runbook.md` deliverable wording to "satisfied by the M13-expanded `docs/legal/OPERATOR_CHECKLIST.md`" (no separate runbook file exists or is needed).

- [ ] **Step 3: M13 spec one-line fix.** In `2026-06-08-phase1-m13-setup-wizard-design.md` § "Out of scope": `- **Release signing (sigstore/cosign) + \`release.yml\` image push.** M14.` → `…** Phase 5 (per the master plan; this line previously said M14 in error).`

- [ ] **Step 4: `README.md`.** Status → Phase 1 complete at `v0.1.0-rc1` (Phase 2 next); add the `docs/quickstart.md` pointer near the top; correct any Node/Go prerequisite mentions to Node 22 / Go 1.26.

- [ ] **Step 5: Spot-fix stale claims** (surgical edits only): `docs/VOLUNTEER_DEPLOYMENT_GUIDANCE.md` (anything claiming deployment is unsupported / wizard doesn't exist — point at quickstart); `docs/recipes/NGINX_REFERENCE.md` (single-origin-era statements — note the M13 two-vhost reference in `nginx/nova.conf.example` is current). Grep aids: `grep -rn "Phase 1 will\|not yet implemented\|unsupported\|M13\|wizard" docs/VOLUNTEER_DEPLOYMENT_GUIDANCE.md docs/recipes/NGINX_REFERENCE.md`.

- [ ] **Step 6: `docs/legal/OPERATOR_CHECKLIST.md`.** Add the http-01 note: initial issuance is now automated in the prod profile (Task 8); the remaining human action is the staging-flag dry run (`certbot certonly --staging …`) before pointing at production ACME; renewal reload is automatic via cert-watch.

- [ ] **Step 7: Commit.**

```bash
git add docs/ROADMAP.md docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md \
        docs/superpowers/specs/2026-06-08-phase1-m13-setup-wizard-design.md README.md \
        docs/VOLUNTEER_DEPLOYMENT_GUIDANCE.md docs/recipes/NGINX_REFERENCE.md \
        docs/legal/OPERATOR_CHECKLIST.md
git commit -m "docs(m14): roadmap, master plan, README, runbook — Phase 1 complete at v0.1.0-rc1 (m14)"
```

---

## Task 12: final verification, merge, tags

- [ ] **Step 1: The full local battery (everything CI runs).**

```bash
go vet ./... && make codegen-check && make test-unit && make test-integration
make admin-lint && make admin-test && make admin-build && make hermetic-spa
make widget-lint && make widget-test && make widget-build && make hermetic-widget
make setup-lint && make setup-test && make setup-build && make hermetic-setup
make migrations-frozen
/tmp/golangci-v2/golangci-lint run ./...      # re-download per Task 1 step 1 if gone
make docker-build
make smoke
```

Expected: every command PASS. Fix anything red before proceeding; re-run the battery after fixes.

- [ ] **Step 2: Use the finishing skill.** Invoke `superpowers:finishing-a-development-branch`. Per the milestone workflow: fast-forward merge `m14-polish-release` into `main`, then:

```bash
git tag -a m14-polish-release -m "M14: polish, security housecleaning, CI e2e smoke, quickstart"
git tag -a v0.1.0-rc1 -m "Phase 1 release candidate 1"
```

No remote push (workflow rule). After Bug pushes manually, confirm on GitHub: all CI jobs green (including the new `smoke` and `migrations-frozen`), and the Dependabot alert list shows the 25 alerts resolved/closed by the merged versions.

- [ ] **Step 3: Update auto-memory.** Rewrite `~/.claude/projects/-home-archbug-projects-ipfs-img-self-host/memory/reference_node_toolchain_skew.md`: web/* is no longer Node-16-pinned — Vite 7/Vitest 3 on Node 22 everywhere (`.nvmrc` authoritative); the hermetic gates remain the no-CDN contract. Update `MEMORY.md`'s hook line to match.

---

## Verification reference (what "done" means, from the design's exit criteria)

1. CI fully green incl. lint (v2), migrations-frozen, smoke.
2. Dependabot alert set closed; triage table in the design records reachability verdicts.
3. Node 22 in `.nvmrc`/engines/CI/Dockerfile; dependabot.yml + CONTRIBUTING policy live.
4. `make smoke` green locally and in CI.
5. http-01: issuance + watcher-reload automated; staging dry-run documented as the human action.
6. Hardening floors in compose with commented exceptions.
7. `docs/quickstart.md` survives a cold read (screenshots or an explicit human-action flag).
8. Docs reconciled; `m14-polish-release` + `v0.1.0-rc1` annotated tags on `main`, local only.
