.PHONY: help test test-unit test-integration tidy build lint smoke migrate-up migrate-down migrate-status clean docker-build migrations-frozen

GOTEST    := go test ./...
GOTESTV   := go test -v ./...
DC        := docker compose -f docker/docker-compose.yml --env-file docker/.env

# Unique per-build version stamp (see docs/VERSIONING.md). Tagged build => tag;
# untagged => nearest tag + commits + short SHA; dirty tree => -dirty suffix.
VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO_LDFLAGS := -X main.buildVersion=$(VERSION)

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
	@echo "  migrate-down      Roll back one migration"
	@echo "  migrate-status    Show migration status"
	@echo "  clean             Remove build artifacts"
	@echo "  docker-build      Build the multi-stage Docker image (no push)"
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
	go build -trimpath -ldflags="-s -w" -o bin/migrate ./cmd/migrate

lint:
	golangci-lint run

smoke:
	./scripts/smoke.sh

m2-exit:
	$(GOTESTV) ./internal/integration/... -run TestIntegrationM2 -count=1

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

# M13 Docker image build. Builds the multi-stage image locally (no push).
# Requires Docker 29+ with BuildKit enabled (the default).
docker-build:
	docker build -f docker/coordinator.Dockerfile -t nova-coordinator:dev .

migrations-frozen:
	./scripts/check-migrations-frozen.sh

.PHONY: sqlc-generate codegen-check build-coordinator run-coordinator

sqlc-generate:
	cd internal/db && go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate

codegen-check: sqlc-generate
	git diff --exit-code -- internal/db/gen || (echo "sqlc drift: run 'make sqlc-generate' and commit" && exit 1)

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
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/nova-node ./cmd/node

# Runs the binary's validate behavior over good + malformed fixtures (table-driven).
node-validate:
	go test -v ./cmd/node/... ./internal/node/config/... -count=1

node-image:
	docker build -f docker/node.Dockerfile -t nova-node:dev .

node-image-inventory: node-image
	./scripts/check_node_image.sh nova-node:dev

# Local SBOM (requires syft on PATH). CI uses the same tool on the built image.
node-sbom: node-image
	mkdir -p dist
	syft nova-node:dev -o spdx-json=dist/nova-node.sbom.spdx.json
