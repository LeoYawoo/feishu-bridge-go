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

# mingw32-make's default shell is cmd.exe and cannot expand POSIX $$(cmd), so
# the platform loop uses literal paths: the extension is derived from GOOS
# inside the loop by a plain shell test, not by make substitution.
# Default to the developer's own platform. Override: make build GOOS=linux.
GOOS   ?= windows
GOARCH ?= amd64
# Executable suffix, per GOOS. Kept as a make variable (not shell-expanded) so
# it works under both POSIX make and mingw32-make.
SUFFIX := $(if $(filter windows,$(GOOS)),.exe,)

# Target matrix: GOOS/GOARCH. Keep in sync with .github/workflows/build.yml.
PLATFORMS := \
	windows/amd64 \
	windows/arm64 \
	linux/amd64 \
	linux/arm64 \
	darwin/amd64 \
	darwin/arm64

# Split a "goos/goarch" pair. make has no built-in split, so use two vars set
# by the caller. Avoids shell $() expansion, which mingw32-make cannot run.
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

## Build for the current host. Override: make build GOOS=linux GOARCH=arm64
build: vet
	@mkdir -p $(DIST)
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" \
		-o "$(DIST)/$(PKG)-$(GOOS)-$(GOARCH)$(SUFFIX)" $(CMD)
	@echo "==> $(DIST)/$(PKG)-$(GOOS)-$(GOARCH)$(SUFFIX)"

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./cmd/... ./internal/...

tidy:
	$(GO) mod tidy

clean:
	rm -rf $(DIST)
	rm -f $(PKG).exe $(PKG) $(PKG).exe.* *.test

## Full matrix. Each platform is an explicit $(GOOS)/$(GOARCH)/<suffix> triple
## so the output name is correct under both POSIX make and mingw32-make.
release: clean
	@mkdir -p $(DIST)
	@set -e; \
	for t in \
		"windows amd64 .exe" \
		"windows arm64 .exe" \
		"linux amd64" \
		"linux arm64" \
		"darwin amd64" \
		"darwin arm64"; do \
		set -- $$t; \
		echo "==> $$1/$$2"; \
		$(GOMODCACHE_ENV) GOOS=$$1 GOARCH=$$2 $(GO) build \
			-trimpath -ldflags "$(LDFLAGS)" \
			-o "$(DIST)/$(PKG)-$$1-$$2$$3" $(CMD) || exit 1; \
	done
	@echo ""
	@echo "artifacts:"
	@ls -1 $(DIST)/

## Windows only.
release-windows: clean
	@mkdir -p $(DIST)
	@set -e; \
	for arch in amd64 arm64; do \
		echo "==> windows/$$arch"; \
		$(GOMODCACHE_ENV) GOOS=windows GOARCH=$$arch $(GO) build \
			-trimpath -ldflags "$(LDFLAGS)" \
			-o "$(DIST)/$(PKG)-windows-$$arch.exe" $(CMD) || exit 1; \
	done

## Linux only.
release-linux: clean
	@mkdir -p $(DIST)
	@set -e; \
	for arch in amd64 arm64; do \
		echo "==> linux/$$arch"; \
		$(GOMODCACHE_ENV) GOOS=linux GOARCH=$$arch $(GO) build \
			-trimpath -ldflags "$(LDFLAGS)" \
			-o "$(DIST)/$(PKG)-linux-$$arch" $(CMD) || exit 1; \
	done
