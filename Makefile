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
