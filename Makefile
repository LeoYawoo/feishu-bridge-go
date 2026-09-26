# Cross-platform builds for feishubridge.
#
# The Makefile is the source of truth for the artifact matrix; CI mirrors it.
# Local single-machine builds on your Windows box use build.bat instead.

GO      ?= go
PKG     := feishubridge
CMD     := ./cmd/feishubridge
DIST    := dist

# Module cache for an offline build. Empty string is the platform default.
GOMODCACHE ?=
ifdef GOMODCACHE
	GOMODCACHE_ENV := GOMODCACHE=$(GOMODCACHE)
else
	GOMODCACHE_ENV :=
endif

# Version stamp embedded via ldflags. Override: make release VERSION=v1.2.3
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X main.Version=$(VERSION) \
	-X main.Commit=$(COMMIT) \
	-X main.BuildTime=$(BUILD_TIME)

# Target matrix: GOOS/GOARCH. Keep in sync with .github/workflows/build.yml.
PLATFORMS := \
	windows/amd64 \
	windows/arm64 \
	linux/amd64 \
	linux/arm64 \
	darwin/amd64 \
	darwin/arm64

.PHONY: all build test vet fmt tidy clean release release-windows release-linux
.DEFAULT_GOAL := help

help:
	@echo "feishubridge build targets"
	@echo ""
	@echo "  make            build for the current host"
	@echo "  make release    cross-compile the full platform matrix -> dist/"
	@echo "  make release-windows"
	@echo "  make release-linux"
	@echo "  make test       unit tests"
	@echo "  make vet        go vet"
	@echo "  make fmt        gofmt -w"
	@echo "  make tidy       go mod tidy"
	@echo "  make clean      remove binaries and dist/"
	@echo ""
	@echo "Variables: VERSION=v1.2.3 GO=your-go GOMODCACHE=/path/to/cache"

## Build for the current host.
build: vet
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(PKG).$$(go env GOOS) ./cmd/feishubridge

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./cmd/... ./internal/...
	$(GO) run golang.org/x/tools/cmd/goimports@latest -w ./cmd ./internal 2>/dev/null || true

tidy:
	$(GO) mod tidy

clean:
	rm -rf $(DIST)
	rm -f $(PKG).exe $(PKG) $(PKG).exe.* *.test

## Full matrix. Use a loop: each $(GOOS)/$(GOARCH) is a separate invocation.
release: clean
	@mkdir -p $(DIST)
	@for p in $(PLATFORMS); do \
		os=$${p%%/*}; arch=$${p##*/}; \
		echo "==> $$os/$$arch"; \
		$(GOMODCACHE_ENV) GOOS=$$os GOARCH=$$arch $(GO) build \
			-trimpath -ldflags "$(LDFLAGS)" \
			-o "$(DIST)/$(PKG)-$$os-$$arch" $(CMD) || exit 1; \
	done
	@echo ""
	@echo "artifacts:"
	@ls -1 $(DIST)/

## Windows only.
release-windows: clean
	@mkdir -p $(DIST)
	@for arch in amd64 arm64; do \
		echo "==> windows/$$arch"; \
		$(GOMODCACHE_ENV) GOOS=windows GOARCH=$$arch $(GO) build \
			-trimpath -ldflags "$(LDFLAGS)" \
			-o "$(DIST)/$(PKG)-windows-$$arch.exe" $(CMD) || exit 1; \
	done

## Linux only.
release-linux: clean
	@mkdir -p $(DIST)
	@for arch in amd64 arm64; do \
		echo "==> linux/$$arch"; \
		$(GOMODCACHE_ENV) GOOS=linux GOARCH=$$arch $(GO) build \
			-trimpath -ldflags "$(LDFLAGS)" \
			-o "$(DIST)/$(PKG)-linux-$$arch" $(CMD) || exit 1; \
	done
