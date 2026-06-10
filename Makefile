SHELL := /bin/bash

GO ?= mise exec -- go
NPM ?= npm
GOCACHE ?= /tmp/agentmux-gocache
CROSS_CGO_ENABLED ?= 0
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || printf dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || printf unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

BIN_DIR ?= bin
DIST_DIR ?= dist
WEB_DIR := web/control
WEB_DIST := internal/hub/webdist

GOFLAGS ?= -trimpath
LDFLAGS ?= -s -w -X private/agentmux/internal/version.Version=$(VERSION) -X private/agentmux/internal/version.Commit=$(COMMIT) -X private/agentmux/internal/version.BuildTime=$(BUILD_TIME)

WORKER_CONTROL_PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
HUB_PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: help
help:
	@printf "AgentMux targets:\n"
	@printf "  make web              Build Web Control and sync embedded webdist\n"
	@printf "  make build            Build local agentmux and agentmux-hub binaries\n"
	@printf "  make test             Run all Go tests\n"
	@printf "  make test-hub         Run hub and hub-only entry tests\n"
	@printf "  make check            Run web build, Go tests, and local builds\n"
	@printf "  make check-hub        Run hub-focused checks including Windows hub build\n"
	@printf "  make release-assets   Build role-specific release tarballs into dist/\n"
	@printf "  make clean            Remove local build outputs\n"

.PHONY: web-deps
web-deps:
	cd $(WEB_DIR) && $(NPM) ci

.PHONY: web-build
web-build:
	cd $(WEB_DIR) && $(NPM) run build

.PHONY: web-sync
web-sync:
	rm -rf $(WEB_DIST)
	mkdir -p $(WEB_DIST)
	cp -a $(WEB_DIR)/dist/. $(WEB_DIST)/

.PHONY: web
web: web-build web-sync

.PHONY: build build-agentmux build-hub
build: build-agentmux build-hub

build-agentmux:
	mkdir -p $(BIN_DIR)
	GOCACHE=$(GOCACHE) $(GO) build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/agentmux ./cmd/agentmux

build-hub:
	mkdir -p $(BIN_DIR)
	GOCACHE=$(GOCACHE) $(GO) build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/agentmux-hub ./cmd/agentmux-hub

.PHONY: build-hub-windows build-hub-linux
build-hub-windows:
	mkdir -p $(BIN_DIR)
	GOOS=windows GOARCH=amd64 CGO_ENABLED=$(CROSS_CGO_ENABLED) GOCACHE=$(GOCACHE) $(GO) build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/agentmux-hub-windows-amd64.exe ./cmd/agentmux-hub

build-hub-linux:
	mkdir -p $(BIN_DIR)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=$(CROSS_CGO_ENABLED) GOCACHE=$(GOCACHE) $(GO) build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/agentmux-hub-linux-amd64 ./cmd/agentmux-hub

.PHONY: test test-hub
test:
	GOCACHE=$(GOCACHE) $(GO) test ./...

test-hub:
	GOCACHE=$(GOCACHE) $(GO) test ./internal/hub ./cmd/agentmux-hub

.PHONY: check check-hub
check: web-build test build

check-hub: web-build test-hub build-hub-linux build-hub-windows

.PHONY: release-assets release-binaries release-worker-control release-hub
release-assets: web release-binaries

release-binaries: release-worker-control release-hub

release-worker-control:
	mkdir -p $(DIST_DIR)
	for role in worker control; do \
	  for platform in $(WORKER_CONTROL_PLATFORMS); do \
	    os="$${platform%/*}"; \
	    arch="$${platform#*/}"; \
	    name="agentmux-$${role}-$${os}-$${arch}"; \
	    echo "building $${name}"; \
	    GOOS="$${os}" GOARCH="$${arch}" CGO_ENABLED=$(CROSS_CGO_ENABLED) GOCACHE=$(GOCACHE) $(GO) build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o "$(DIST_DIR)/$${name}" ./cmd/agentmux; \
	    tar -C "$(DIST_DIR)" -czf "$(DIST_DIR)/$${name}.tar.gz" "$${name}"; \
	    sha256sum "$(DIST_DIR)/$${name}.tar.gz" > "$(DIST_DIR)/$${name}.tar.gz.sha256"; \
	  done; \
	done

release-hub:
	mkdir -p $(DIST_DIR)
	for platform in $(HUB_PLATFORMS); do \
	  os="$${platform%/*}"; \
	  arch="$${platform#*/}"; \
	  name="agentmux-hub-$${os}-$${arch}"; \
	  ext=""; \
	  if [ "$${os}" = "windows" ]; then ext=".exe"; fi; \
	  echo "building $${name}"; \
	  GOOS="$${os}" GOARCH="$${arch}" CGO_ENABLED=$(CROSS_CGO_ENABLED) GOCACHE=$(GOCACHE) $(GO) build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o "$(DIST_DIR)/$${name}$${ext}" ./cmd/agentmux-hub; \
	  tar -C "$(DIST_DIR)" -czf "$(DIST_DIR)/$${name}.tar.gz" "$${name}$${ext}"; \
	  sha256sum "$(DIST_DIR)/$${name}.tar.gz" > "$(DIST_DIR)/$${name}.tar.gz.sha256"; \
	done

.PHONY: docker-build dev-tmux clean
docker-build:
	docker build -t agentmux:local .

dev-tmux:
	./scripts/dev-tmux.sh

clean:
	rm -rf $(BIN_DIR) $(DIST_DIR) $(WEB_DIR)/dist
