BINARY_COLLECTOR := yarddog
BINARY_DAEMON    := yarddogd
CMD_COLLECTOR    := ./cmd/yarddog
CMD_DAEMON       := ./cmd/yarddogd
BUILD_DIR        := build

.PHONY: build build-arm build-mac run run-daemon test test-race lint format clean

# pure-Go build (no cgo) so the binaries cross-compile to the Pi without a C toolchain
build:
	CGO_ENABLED=0 go build -o $(BUILD_DIR)/$(BINARY_COLLECTOR) $(CMD_COLLECTOR)
	CGO_ENABLED=0 go build -o $(BUILD_DIR)/$(BINARY_DAEMON) $(CMD_DAEMON)

# cross-compile for the Raspberry Pi (Ubuntu Server, arm64)
build-arm:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o $(BUILD_DIR)/$(BINARY_COLLECTOR)-arm64 $(CMD_COLLECTOR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o $(BUILD_DIR)/$(BINARY_DAEMON)-arm64 $(CMD_DAEMON)

# cross-compile for a MacBook (Apple Silicon) — CGO stays off here too, so the
# darwin-only temperature/fan collectors never grow a cgo/SMC dependency. This
# is a local build target only: the release pipeline's staging drop is pi5-only.
build-mac:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o $(BUILD_DIR)/$(BINARY_COLLECTOR)-darwin-arm64 $(CMD_COLLECTOR)
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o $(BUILD_DIR)/$(BINARY_DAEMON)-darwin-arm64 $(CMD_DAEMON)

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
