# agentflow — build, test, install.
#
# `make install` puts the binary in ~/.local/bin (where the service expects it)
# and `make service` brings it up in the background. `make build` just produces
# ./bin/agentflow for local use.
#
# `make test` runs under a sandboxed HOME so the real Claude/OpenCode config
# (~/.claude, ~/.config/agentflow, ~/.local/share/agentflow) is never touched
# by tests. A test that reached the real ~/.claude could rewrite live sessions.

BIN        := agentflow
PKG        := ./cmd/agentflow
BIN_DIR    := $(HOME)/.local/bin
VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE       := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.buildDate=$(DATE)

# Go packages, minus anything npm dropped under web/node_modules.
GO_PKGS    = $$(go list ./... | grep -v '/web/')

# Pinned tool versions (CI uses the same).
GOLANGCI_LINT_VERSION := v2.13.2
GOVULNCHECK_VERSION   := v1.8.0

.PHONY: build build-go web dev dev-web test vet fmt lint vuln darwin-check release-snapshot install service uninstall clean

## build: build ./bin/agentflow with the embedded web UI
build: web
	@mkdir -p bin
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BIN) $(PKG)

## build-go: build only the Go binary (no web UI; for fast Go iteration)
build-go:
	@mkdir -p bin
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BIN) $(PKG)

## web: build the Next.js frontend and embed it for the Go binary
web:
	cd web && npm ci && npm run build && rm -rf ../internal/webui/dist/* && cp -r out/. ../internal/webui/dist/

## dev: run the daemon against an isolated, gitignored ./.dev dir (own port,
## remote off) so local development never touches your real install's data,
## port or service.
dev:
	AF_DATA_DIR=./.dev AF_DB=./.dev/agentflow.db AF_ADDR=127.0.0.1:4400 AF_REMOTE=off go run ./cmd/agentflow serve

## dev-web: run the Next.js dev server (hot reload). Point it at the dev daemon
## by allowing its origin: AF_DEV_CORS_ORIGIN=http://localhost:3000 make dev
dev-web:
	cd web && npm run dev

## test: run the test suite in a sandboxed HOME (with -race)
test:
	@set -e; \
	export GOMODCACHE="$$(go env GOMODCACHE)" GOCACHE="$$(go env GOCACHE)" GOPATH="$$(go env GOPATH)"; \
	sandbox="$$(mktemp -d -t agentflow-test.XXXXXX)"; \
	trap 'rm -rf "$$sandbox"' EXIT; \
	HOME="$$sandbox" XDG_CONFIG_HOME="$$sandbox/.config" XDG_DATA_HOME="$$sandbox/.local/share" AF_REMOTE=off \
		go test -race -count=1 $(GO_PKGS)

## vet: run go vet
vet:
	go vet $(GO_PKGS)

## lint: run golangci-lint (the pinned version, installed with go install if missing)
lint:
	@command -v golangci-lint >/dev/null 2>&1 || go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	golangci-lint config verify
	golangci-lint run ./...

## vuln: report known vulnerabilities reachable from the code
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

## darwin-check: build and vet for macOS (catches Linux-only syscalls)
darwin-check:
	GOOS=darwin GOARCH=arm64 go build $(GO_PKGS)
	GOOS=darwin GOARCH=arm64 go vet $(GO_PKGS)
	GOOS=darwin GOARCH=amd64 go build $(GO_PKGS)

## release-snapshot: build every release archive locally into dist/ (needs goreleaser)
release-snapshot:
	@command -v goreleaser >/dev/null 2>&1 || { echo "goreleaser is not installed: https://goreleaser.com/install/"; exit 1; }
	goreleaser release --snapshot --clean

## fmt: format the tree
fmt:
	gofmt -s -w $$(git ls-files '*.go')

## install: build and install the binary to ~/.local/bin
install: build
	@mkdir -p $(BIN_DIR)
	install -m 0755 bin/$(BIN) $(BIN_DIR)/$(BIN)
	@echo "installed $(BIN_DIR)/$(BIN)"
	@echo "start it in the background with: make service   (or: agentflow start)"

## service: install the binary, then register + start the background service
service: install
	$(BIN_DIR)/$(BIN) start

## uninstall: stop the service and remove the binary (keeps your data)
uninstall:
	-$(BIN_DIR)/$(BIN) uninstall
	@echo "run 'agentflow uninstall --purge' to remove data too"

## clean: remove build output
clean:
	rm -rf bin