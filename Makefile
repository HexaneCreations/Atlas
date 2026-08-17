# Atlas — developer and CI entry points.
#
# `make help` lists every target. The same commands run locally and in CI, so
# a green pipeline is reproducible on a laptop and a red one is debuggable
# without reading the workflow file.

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

BINARY      := atlas-server
BIN_DIR     := bin
MODULE      := github.com/hexane/atlas
BUILD_PKG   := $(MODULE)/internal/platform/build

VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILDTIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# Build identity is stamped in rather than read at runtime, so a deployed
# binary can always name the source it came from.
LDFLAGS := -s -w \
	-X '$(BUILD_PKG).Version=$(VERSION)' \
	-X '$(BUILD_PKG).Commit=$(COMMIT)' \
	-X '$(BUILD_PKG).BuildTime=$(BUILDTIME)'

# The development database created by `make db-up`.
TEST_DATABASE_URL ?= postgres://atlas:atlas_dev_password@127.0.0.1:5432/atlas?sslmode=disable

# Some npm packages vendor Go source, so `./...` would pick up third-party
# packages under web/node_modules. Resolve the package list explicitly.
GO_PACKAGES = $(shell go list ./... | grep -v '/node_modules/')

# Local dev config (see .env.example). Optional: absent, targets fall back
# to Go's own defaults (internal/platform/config) — only server/dev/env-check
# actually need a real database and Fleet config. `include` (not a shell
# `source`) is what makes this safe for values containing slashes/colons,
# like a libp2p multiaddr, with no extra parsing.
ifneq (,$(wildcard .env))
include .env
export
endif

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

# ---------------------------------------------------------------- build ----

.PHONY: build
build: ## Build the atlas-server binary into bin/
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/$(BINARY)
	@echo "built $(BIN_DIR)/$(BINARY) ($(VERSION))"

.PHONY: build-agent
build-agent: ## Build the atlas-agent binary into bin/
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/atlas-agent ./cmd/atlas-agent
	@echo "built $(BIN_DIR)/atlas-agent ($(VERSION))"

.PHONY: build-relay
build-relay: ## Build the atlas-relay binary into bin/
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/atlas-relay ./cmd/atlas-relay
	@echo "built $(BIN_DIR)/atlas-relay ($(VERSION))"

.PHONY: build-all
build-all: build build-agent build-relay ## Build every binary

.PHONY: run
run: ## Run Atlas against the local development database
	ATLAS_DATABASE_PASSWORD=atlas_dev_password \
	ATLAS_DATABASE_SSL_MODE=disable \
	ATLAS_LOGGING_FORMAT=text \
	ATLAS_LOGGING_LEVEL=debug \
	go run ./cmd/$(BINARY) serve

.PHONY: require-env
require-env:
	@test -f .env || { echo "no .env found — run: cp .env.example .env, then edit it"; exit 1; }

.PHONY: server
server: require-env build ## Run the full Atlas server (Fleet + libp2p) using .env
	$(BIN_DIR)/$(BINARY) serve

.PHONY: dev
dev: require-env build ## Run the Atlas server and the frontend dev server together
	@trap 'kill 0' EXIT; \
	$(BIN_DIR)/$(BINARY) serve & \
	$(MAKE) web-dev; \
	wait

.PHONY: env-check
env-check: ## Verify required server environment is set, without printing secrets
	@missing=0; \
	for var in ATLAS_ENVIRONMENT ATLAS_DATABASE_HOST ATLAS_DATABASE_PORT ATLAS_DATABASE_NAME \
		ATLAS_DATABASE_USER ATLAS_DATABASE_PASSWORD ATLAS_DATABASE_SSL_MODE ATLAS_FLEET_ENABLED \
		ATLAS_FLEET_DATA_DIR ATLAS_FLEET_LIBP2P_ENABLED ATLAS_FLEET_LIBP2P_LISTEN_ADDRS \
		ATLAS_FLEET_LIBP2P_RELAY_ADDR; do \
		val="$${!var:-}"; \
		if [ -z "$$val" ]; then \
			echo "missing  $$var"; missing=1; \
		elif echo "$$var" | grep -qiE 'PASSWORD|TOKEN|SECRET'; then \
			echo "ok       $$var (set, hidden)"; \
		else \
			echo "ok       $$var=$$val"; \
		fi; \
	done; \
	if [ "$$missing" = 1 ]; then \
		echo; echo "missing variables above — run: cp .env.example .env, then edit it"; exit 1; \
	fi; \
	echo; echo "all required variables set"

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN_DIR) web/dist

# ----------------------------------------------------------------- test ----

.PHONY: test
test: ## Run unit tests with the race detector
	go test -race -count=1 $(GO_PACKAGES)

.PHONY: test-integration
test-integration: ## Run integration tests (needs `make db-up`)
	ATLAS_TEST_DATABASE_URL="$(TEST_DATABASE_URL)" \
		go test -race -count=1 -tags=integration $(GO_PACKAGES)

.PHONY: cover
cover: ## Produce a coverage report at coverage.html
	@# Merging a profile across packages needs the `covdata` tool. A toolchain
	@# fetched automatically by the `go` directive is minimal and omits it, so
	@# report that clearly and fall back to a per-package summary rather than
	@# failing with an unexplained "no such tool".
	@if ! go tool covdata help >/dev/null 2>&1; then \
		echo "note: this Go toolchain has no 'covdata' tool, so a merged profile"; \
		echo "      cannot be produced. Install a full Go $(shell go env GOVERSION | sed 's/go//') distribution for coverage.html."; \
		echo "      Falling back to per-package coverage:"; \
		echo; \
		go test -race -cover $(GO_PACKAGES); \
		exit 0; \
	fi; \
	go test -race -coverprofile=coverage.out -covermode=atomic $(GO_PACKAGES) && \
	go tool cover -html=coverage.out -o coverage.html && \
	go tool cover -func=coverage.out | tail -1

.PHONY: bench
bench: ## Run benchmarks
	go test -run=^$$ -bench=. -benchmem $(GO_PACKAGES)

# ---------------------------------------------------------------- checks ----

.PHONY: fmt
fmt: ## Format Go source
	gofmt -w -s $$(git ls-files '*.go')

.PHONY: fmt-check
fmt-check: ## Fail if any Go file is unformatted
	@unformatted=$$(gofmt -l -s $$(git ls-files '*.go')); \
	if [ -n "$$unformatted" ]; then \
		echo "these files are not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: vet
vet: ## Run go vet
	go vet $(GO_PACKAGES)

.PHONY: lint
lint: ## Run golangci-lint (install: https://golangci-lint.run)
	@command -v golangci-lint >/dev/null || { echo "golangci-lint is not installed"; exit 1; }
	golangci-lint run

.PHONY: tidy-check
tidy-check: ## Fail if go.mod or go.sum are not tidy
	@cp go.mod go.mod.bak && cp go.sum go.sum.bak
	@go mod tidy
	@if ! diff -q go.mod go.mod.bak >/dev/null || ! diff -q go.sum go.sum.bak >/dev/null; then \
		mv go.mod.bak go.mod; mv go.sum.bak go.sum; \
		echo "go.mod/go.sum are not tidy; run 'go mod tidy'"; exit 1; \
	fi
	@rm -f go.mod.bak go.sum.bak

.PHONY: check
check: fmt-check vet lint test ## Everything CI runs, minus integration tests

# ------------------------------------------------------------------- db ----

.PHONY: db-up
db-up: ## Start the development database and wait for it to be ready
	docker compose up -d postgres
	@echo -n "waiting for postgres"
	@for i in $$(seq 1 60); do \
		if docker compose exec -T postgres pg_isready -U atlas -d atlas >/dev/null 2>&1; then \
			echo " ready"; exit 0; \
		fi; \
		echo -n "."; sleep 1; \
	done; \
	echo " timed out"; exit 1

.PHONY: db-down
db-down: ## Stop the development database, preserving data
	docker compose down

.PHONY: db-reset
db-reset: ## Destroy the development database and its data, then recreate it
	docker compose down -v
	$(MAKE) db-up

.PHONY: db-shell
db-shell: ## Open a psql shell on the development database
	docker compose exec postgres psql -U atlas -d atlas

.PHONY: migrate
migrate: ## Apply pending migrations to the development database
	ATLAS_DATABASE_PASSWORD=atlas_dev_password \
	ATLAS_DATABASE_SSL_MODE=disable \
	go run ./cmd/$(BINARY) migrate

# ------------------------------------------------------------------ web ----

.PHONY: web-install
web-install: ## Install frontend dependencies
	cd web && npm ci

.PHONY: web-dev
web-dev: ## Run the frontend dev server against a local Atlas
	cd web && npm run dev

.PHONY: ui
ui: web-dev ## Alias for web-dev

.PHONY: web-build
web-build: ## Build the production frontend bundle
	cd web && npm run build

.PHONY: web-check
web-check: ## Type-check, lint and test the frontend
	cd web && npm run typecheck && npm run lint && npm run test

.PHONY: web-test
web-test: ## Run the frontend unit tests
	cd web && npm run test

# ---------------------------------------------------------------- docker ----

.PHONY: image
image: ## Build the production container image
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILDTIME=$(BUILDTIME) \
		-t atlas:$(VERSION) -t atlas:latest .
