GO ?= go
VERSION ?= dev

.PHONY: build test check release
build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' -o bin/nodebridge ./cmd/nodebridge
test:
	$(GO) test -race ./...
check:
	$(GO) vet ./...
	$(GO) test -race ./...
	@if command -v node >/dev/null; then node --check internal/web/app.js; fi
	bash -n scripts/install.sh scripts/uninstall.sh scripts/release.sh
release:
	GO=$(GO) VERSION=$(VERSION) bash scripts/release.sh
