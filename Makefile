.PHONY: help test build clean tidy

help:
	@echo "Targets:"
	@echo "  test            Run all Go and web tests"
	@echo "  tidy            Tidy Go module files"
	@echo "  build           Build all binaries (Phase 1+)"
	@echo "  clean           Remove build artifacts"

test:
	go test ./...
	@if [ -f web/widget/package.json ]; then npm -w web/widget test --if-present; fi
	@if [ -f web/admin/package.json ]; then npm -w web/admin test --if-present; fi

tidy:
	go mod tidy

build:
	@echo "Phase 0: no buildable targets yet. See docs/ROADMAP.md"

clean:
	rm -rf bin dist build coverage.out coverage.html
