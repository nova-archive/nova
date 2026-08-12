.PHONY: help test test-unit test-integration tidy build lint smoke migrate-up migrate-apply migrate-status clean docker-build docker-refresh-digests migrations-frozen

GOTEST    := go test ./...
GOTESTV   := go test -v ./...
# The dev overlay is not optional here: the base names RELEASED artifacts by
# digest (P2-M7.3, D-M7.3-14), so a developer's compose invocation has to say
# it wants to build. An operator's does not, and that is the point.
DC        := docker compose -f docker/docker-compose.yml -f docker/docker-compose.dev.yml --env-file docker/.env

# Unique per-build version stamp (see docs/VERSIONING.md). Tagged build => tag;
# untagged => nearest tag + commits + short SHA; dirty tree => -dirty suffix.
#
# P2-M7.3 P0-c: all three values land in internal/buildinfo, which every binary
# reads. Previously GO_LDFLAGS named main.buildVersion and no build target used
# it, so every containerized coordinator reported "dev" and the donor binary
# carried no version symbol at all. Anything that produces a shipped binary
# MUST pass $(GO_LDFLAGS).
VERSION       := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REVISION      := $(shell git rev-parse --short=7 HEAD 2>/dev/null || echo unknown)
BUILD_DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
BUILDINFO_PKG := github.com/nova-archive/nova/internal/buildinfo
GO_LDFLAGS    := -X $(BUILDINFO_PKG).version=$(VERSION) \
                 -X $(BUILDINFO_PKG).revision=$(REVISION) \
                 -X $(BUILDINFO_PKG).buildDate=$(BUILD_DATE)
DOCKER_BUILD_ARGS := --build-arg NOVA_VERSION=$(VERSION) \
                     --build-arg NOVA_REVISION=$(REVISION) \
                     --build-arg NOVA_BUILD_DATE=$(BUILD_DATE)

help:
	@echo "Phase 1 M1 targets:"
	@echo "  test              Run all Go tests (unit + integration)"
	@echo "  test-unit         Run only unit tests (-short)"
	@echo "  test-integration  Run only integration tests"
	@echo "  tidy              Tidy Go module files"
	@echo "  build             Build cmd/migrate (other binaries in later M)"
	@echo "  lint              Run golangci-lint"
	@echo "  smoke             End-to-end smoke: image build + compose prod + upload/read/transform/delete"
	@echo "  m2-exit           Run the M2 exit-criterion test (env → ipfs → decrypt round-trip)"
	@echo "  migrate-up        Apply migrations against running compose postgres"
	@echo "  migrate-apply     Apply exactly (applied, TO] under the advisory lock"
	@echo "  upgrade-evidence  Run the three compatibility-evidence gates"
	@echo "  migrate-status    Show migration status"
	@echo "  clean             Remove build artifacts"
	@echo "  build-context-check  Probe that .dockerignore keeps secrets and local state out of the build context"
	@echo "  docker-build      Build the multi-stage Docker image (no push)"
	@echo "  docker-refresh-digests  Re-resolve the digest pins in docker/*.Dockerfile"
	@echo "  migrations-frozen Verify shipped migrations are unmodified (MANIFEST.sha256)"

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
	go build -trimpath -ldflags="-s -w $(GO_LDFLAGS)" -o bin/migrate ./cmd/migrate

lint:
	golangci-lint run

smoke:
	./scripts/smoke.sh

m2-exit:
	$(GOTESTV) ./internal/integration/... -run TestIntegrationM2 -count=1

# P2-M7.3: every apply is target-bounded, advisory-locked and journalled.
# NOVA_UPGRADE_JOURNAL_DIR points at a local directory here; in the container it
# is an operator-owned volume, because a `run --rm` service's rootfs evaporates
# on exactly the failed upgrade you need the record for.
NOVA_UPGRADE_JOURNAL_DIR ?= $(CURDIR)/.nova-upgrade-journal

migrate-up: build
	$(DC) up -d postgres
	$(DC) exec -T postgres pg_isready -U nova || (sleep 5 && $(DC) exec -T postgres pg_isready -U nova)
	NOVA_UPGRADE_JOURNAL_DIR=$(NOVA_UPGRADE_JOURNAL_DIR) ./bin/migrate up

# `migrate apply --to <n>` is the reviewed form. TO is required; there is no
# default, because a default target is an unbounded apply wearing a flag.
migrate-apply: build
	@test -n "$(TO)" || (echo "usage: make migrate-apply TO=<schema> [ACK='--acknowledge <id>']" >&2; exit 2)
	NOVA_UPGRADE_JOURNAL_DIR=$(NOVA_UPGRADE_JOURNAL_DIR) ./bin/migrate apply --to $(TO) $(ACK)

migrate-status: build
	./bin/migrate status

clean:
	rm -rf bin dist build coverage.out coverage.html .nova-upgrade-journal

# P2-M7.3 D-M7.3-4: every Dockerfile does `COPY . .`, so the working tree is
# part of the signed artifact. The gate probes a real build rather than
# modelling Docker's ignore semantics — see the script's header for why.
.PHONY: build-context-check
build-context-check:
	./scripts/check-build-context.sh

# M13 Docker image build. Builds the multi-stage image locally (no push).
# Requires Docker 29+ with BuildKit enabled (the default).
docker-build: build-context-check
	docker build $(DOCKER_BUILD_ARGS) -f docker/coordinator.Dockerfile -t nova-coordinator:dev .

# P2-M7.1: base images are digest-pinned (FROM image:tag@sha256:...).
# Re-resolves each tag's current manifest-list digest and rewrites the pins in place.
docker-refresh-digests:
	./scripts/refresh-docker-digests.sh

migrations-frozen:
	./scripts/check-migrations-frozen.sh

.PHONY: gen-deploy gen-deploy-check deploy-gates
# P2-M7.2 D-M7.2-5: deploy/donor/ is GENERATED from internal/deploy/templates/.
# Edit the templates, not the output.
gen-deploy:
	go run ./cmd/gen-deploy

gen-deploy-check:
	./scripts/check-gen-deploy.sh

# P2-M7.2 D-M7.2-10: artifact-correctness gates — node.yaml completeness
# (reflection over nodeconfig.Config), rendered-config validity (the production
# loader against a fixture tree), and bundle topology.
deploy-gates:
	go test ./internal/deploy/... -count=1

.PHONY: docs-cli-check port-vocabulary-check no-mutable-tags-check compose-custody-check compose-topology-check artifact-gates
# P2-M7.2 D-M7.2-10: docs rot is what produced the federation productization
# finding — six sources of truth that had already diverged, with no test
# noticing. These make that class of drift a build failure.
docs-cli-check:
	./scripts/check-docs-cli.sh

port-vocabulary-check:
	./scripts/check-port-vocabulary.sh

no-mutable-tags-check:
	./scripts/check-no-mutable-tags.sh

# Plane C of federation doctor: CA custody proven structurally, no Docker socket.
compose-topology-check:
	./scripts/check-compose-topology.sh

compose-custody-check:
	./scripts/check-compose-custody.sh

# P2-M7.2 D-M7.2-3: all three doctor planes, one consolidated report. Plane B
# needs the coordinator network namespace, so it runs via the nova-doctor
# service; Plane C is a static check over the rendered compose definition.
.PHONY: federation-doctor
federation-doctor:
	@go run ./cmd/novactl federation doctor || true
	@$(MAKE) --no-print-directory compose-custody-check

# P2-M7.3 D-M7.3-2b: every checked-in release intent parses, validates and is
# named for the version it declares. novarel is a BUILD-TIME tool — it lives
# under internal/release/cmd so nothing ships it in a runtime image (T1.22).
.PHONY: release-validate catalog catalog-check
release-validate:
	go run ./internal/release/cmd/novarel validate

# P2-M7.3 D-M7.3-2c: internal/release/catalog/catalog_gen.go is GENERATED from
# the checked-in intent and compiled into every binary. T1.22 forbids fetching
# release metadata, so this is how a build knows what it claims to be.
catalog:
	go run ./internal/release/cmd/novarel catalog

catalog-check:
	go run ./internal/release/cmd/novarel catalog --check

# Everything the deployment-artifacts CI job runs, in one local target.
artifact-gates: gen-deploy-check deploy-gates docs-cli-check port-vocabulary-check no-mutable-tags-check compose-custody-check compose-topology-check release-validate catalog-check

.PHONY: sqlc-generate codegen-check build-coordinator run-coordinator

sqlc-generate:
	cd internal/db && go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate

codegen-check: sqlc-generate
	git diff --exit-code -- internal/db/gen || (echo "sqlc drift: run 'make sqlc-generate' and commit" && exit 1)

.PHONY: bench-corpus bench-corpus-ci bench-corpus-explain
# P2-M7 D-M7-2: full-scale LOCAL release gate (hours; scratch DB only).
bench-corpus:
	BENCH_ROWS=9800000 BENCH_PROFILE=release go test -v -timeout 240m -run TestCorpusBench ./internal/benchcorpus
# CI regression profile: shape + plans, small corpus.
bench-corpus-ci:
	BENCH_ROWS=250000 BENCH_PROFILE=ci go test -v -timeout 30m -run TestCorpusBench ./internal/benchcorpus
bench-corpus-explain:
	go test -v -timeout 20m -run 'TestExplainPlans|TestScratchDSNGuard|TestSeedSkewedCorpus' ./internal/benchcorpus

.PHONY: federation-deploy-e2e live-upgrade-e2e
# P2-M7.2 D-M7.2-11. federation-deploy-e2e needs TUN and privileged
# networking, so it is a local / self-hosted release gate like crossversion-e2e.
federation-deploy-e2e:
	./scripts/federation_deploy_e2e.sh

# live-upgrade-e2e is pure novactl against temp dirs — no TUN, no daemon — so it
# runs hermetically on every PR. An existing federation must cross every
# milestone in this track using only docs/UPGRADING.md.
live-upgrade-e2e:
	./scripts/live_upgrade_e2e.sh

# P2-M7.3 D-M7.3-12: three DISTINCT kinds of compatibility evidence. They are
# separate targets because they prove separate things, and a single "upgrade
# e2e" would let the cheapest of them stand in for the other two.
.PHONY: upgrade-wire-e2e upgrade-schema-e2e upgrade-release-e2e upgrade-evidence
upgrade-wire-e2e:
	./scripts/upgrade_wire_e2e.sh

upgrade-schema-e2e:
	./scripts/upgrade_schema_e2e.sh

# SKIPS until a release exists to test against, and says so. A skip is not a
# pass: release.EvidenceRef records the outcome and a claim proven by a skipped
# gate is rejected.
upgrade-release-e2e:
	./scripts/upgrade_release_e2e.sh $(VARIANT)

upgrade-evidence: upgrade-wire-e2e upgrade-schema-e2e upgrade-release-e2e

.PHONY: crossversion-e2e
# P2-M7 D-M7-3: local gate; requires docker + libvips headers. PAIRING=all|head-head|head-coord-old-donor|old-coord-head-donor
crossversion-e2e:
	./scripts/crossversion_e2e.sh $(PAIRING)

build-coordinator:
	go build -ldflags "$(GO_LDFLAGS)" -o bin/coordinator ./cmd/coordinator

run-coordinator:
	go run -ldflags "$(GO_LDFLAGS)" ./cmd/coordinator

.PHONY: admin admin-install admin-build admin-lint admin-test hermetic-spa

# M11 Admin SPA (web/admin). Hermetic React + Vite; no third-party runtime assets.
admin-install:
	npm ci

admin-build:
	npm run build --workspace web/admin

admin-lint:
	npm run lint --workspace web/admin

admin-test:
	npm run test --workspace web/admin -- --run

# hermetic-spa fails the build if the bundle declares any third-party asset load.
hermetic-spa:
	./scripts/hermetic-spa.sh web/admin/dist

admin: admin-install admin-lint admin-test admin-build hermetic-spa

.PHONY: widget widget-build widget-lint widget-test hermetic-widget web

# M12 Upload Widget (web/widget). Hermetic Uppy + tus; no third-party runtime assets.
widget-build:
	npm run build --workspace web/widget

widget-lint:
	npm run lint --workspace web/widget

widget-test:
	npm run test --workspace web/widget -- --run

# hermetic-widget fails the build if the widget bundle declares any third-party
# asset load. The widget inlines its CSS into the JS bundle (single <script> embed),
# so in addition to the HTML/CSS gate we scan the JS for CSS asset-load patterns
# (url(http…), @import …http) — unambiguous external asset loads, distinct from the
# harmless doc-URL string literals hermetic-spa.sh deliberately ignores.
hermetic-widget:
	./scripts/hermetic-spa.sh web/widget/dist
	@if grep -qaE 'url\(https?:|@import[^;]*https?:' web/widget/dist/nova-upload-widget.js; then \
		echo "hermetic-widget: external CSS asset URL in the inlined bundle" >&2; exit 1; \
	fi; \
	echo "hermetic-widget: inlined-CSS clean (web/widget/dist/nova-upload-widget.js)"

widget: admin-install widget-lint widget-test widget-build hermetic-widget

.PHONY: setup-spa setup-install setup-build setup-lint setup-test hermetic-setup

# M13 first-run Setup wizard (web/setup). Hermetic React + Vite; no third-party
# runtime assets. base '/setup/' so hashed assets resolve behind the coordinator's
# /setup/* mount during bootstrap.
setup-install:
	npm ci

setup-build:
	npm run build --workspace web/setup

setup-lint:
	npm run lint --workspace web/setup

setup-test:
	npm run test --workspace web/setup -- --run

# hermetic-setup fails the build if the bundle declares any third-party asset load.
hermetic-setup:
	./scripts/hermetic-spa.sh web/setup/dist

setup-spa: setup-install setup-lint setup-test setup-build hermetic-setup

# web builds + checks all web workspaces (npm ci installs all workspaces).
web: admin widget setup-spa

.PHONY: node-deps-check
node-deps-check:
	./scripts/check_node_deps.sh

.PHONY: node-build node-validate node-image node-image-inventory node-sbom

node-build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w $(GO_LDFLAGS)" -o bin/nova-node ./cmd/node

# Runs the binary's validate behavior over good + malformed fixtures (table-driven).
node-validate:
	go test -v ./cmd/node/... ./internal/node/config/... -count=1

node-image:
	docker build $(DOCKER_BUILD_ARGS) -f docker/node.Dockerfile -t nova-node:dev .

# P2-M7.3 P0-b: a generated donor bundle must actually reach Docker's `healthy`
# state. Nothing had ever started one, which is how a probe naming a path that
# does not exist in the image shipped. TUN-free — see the script's header.
.PHONY: donor-bundle-health
donor-bundle-health:
	./scripts/check-donor-bundle-health.sh

node-image-inventory: node-image
	./scripts/check_node_image.sh nova-node:dev

# Local SBOM (requires syft on PATH). CI uses the same tool on the built image.
node-sbom: node-image
	mkdir -p dist
	syft nova-node:dev -o spdx-json=dist/nova-node.sbom.spdx.json
