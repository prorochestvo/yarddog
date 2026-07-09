BINARY_COLLECTOR := yarddog
BINARY_DAEMON    := yarddogd
CMD_COLLECTOR    := ./cmd/yarddog
CMD_DAEMON       := ./cmd/yarddogd
BUILD_DIR        := build

# OS selects the build target; empty builds natively for this host.
#   make build          -> native host binaries  (build/yarddog,  build/yarddogd)
#   make build OS=arm64 -> linux/arm64 for the Pi (build/yarddog-arm64, build/yarddogd-arm64)
OS ?=
ifeq ($(OS),)
  BUILD_ENV :=
  SUFFIX    :=
else ifeq ($(OS),arm64)
  BUILD_ENV := GOOS=linux GOARCH=arm64
  SUFFIX    := -arm64
else
  $(error unsupported OS='$(OS)' — use OS=arm64 or omit for a native build)
endif

# VERSION is embedded into the daemon (main.version) so /health/check reports it;
# `make deploy` overrides it with the release VERSION_ID.
VERSION ?= dev

.PHONY: build deploy run run-daemon test test-race lint format clean

# pure-Go build (no cgo) so the binaries cross-compile to the Pi without a C toolchain
build:
	CGO_ENABLED=0 $(BUILD_ENV) go build -o $(BUILD_DIR)/$(BINARY_COLLECTOR)$(SUFFIX) $(CMD_COLLECTOR)
	CGO_ENABLED=0 $(BUILD_ENV) go build -ldflags="-X main.version=$(VERSION)" -o $(BUILD_DIR)/$(BINARY_DAEMON)$(SUFFIX) $(CMD_DAEMON)

# build the arm64 binaries and deploy them to pi5 over a direct `ssh pi5`, mirroring
# the CI release-layout: upload into an immutable artifacts/<VERSION_ID> dir, flip
# bin/release, restart yarddogd, gate on /ping, roll back on failure, prune old ones.
DEPLOY_VERSION := $(shell date -u +%Y%m%d%H%M%S)-r_$(shell git rev-parse --short HEAD 2>/dev/null || echo manual)
deploy:
	$(MAKE) build OS=arm64 VERSION=$(DEPLOY_VERSION)
	bash deploy/pi5-deploy.sh $(DEPLOY_VERSION)

# collector stays the default `make run` (the daemon is opt-in: make run-daemon)
run:
	CGO_ENABLED=0 go run $(CMD_COLLECTOR)

run-daemon:
	CGO_ENABLED=0 go run $(CMD_DAEMON)

# completion gate (matches README/PLAN: fmt + vet + test, no -race)
test:
	go fmt ./...
	CGO_ENABLED=0 go vet ./...
	CGO_ENABLED=0 go test ./...

# opt-in race detector — needs a C toolchain (the race runtime uses cgo)
test-race:
	go test -race ./...

# vet + guard against forbidden dependencies (stdlib-only bar modernc.org/sqlite)
lint:
	CGO_ENABLED=0 go vet ./...
	@if grep -Eq 'mattn/go-sqlite3|stretchr/testify' go.mod; then \
		echo "forbidden dependency in go.mod (cgo sqlite driver / testify)"; exit 1; \
	fi

format:
	go fmt ./...

clean:
	rm -rf $(BUILD_DIR)
	go mod tidy
