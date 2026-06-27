.PHONY: build vmm krucible test clean release

VERSION ?= $(shell git describe --tags --always --dirty)

# libkrucible (our libkrun fork). KRUCIBLE_PREFIX is the link prefix `make vmm`
# points at; if unbuilt, vmm falls back to the system (Homebrew) libkrun.
LIBKRUCIBLE ?= libkrucible
KRUCIBLE_PREFIX ?= $(abspath $(LIBKRUCIBLE)/_install)

# Build the bhatti binary with version injection
build:
	go build -ldflags="-s -w -X main.version=$(VERSION)" -o bhatti ./cmd/bhatti/

# Build lohar (guest agent) for Linux
lohar:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o lohar ./cmd/lohar/

# Build the per-VM libkrun helper (krucible engine). cgo + libkrun via
# pkg-config; on macOS it must be codesigned with the hypervisor entitlement
# to use HVF. The daemon spawns this binary per sandbox.
# Build libkrucible (our libkrun fork) + assemble the link prefix for `make vmm`.
krucible:
	scripts/krucible-build-lib.sh "$(LIBKRUCIBLE)" "$(KRUCIBLE_PREFIX)"

vmm:
	PKG_CONFIG_PATH="$(KRUCIBLE_PREFIX)/lib/pkgconfig:$$PKG_CONFIG_PATH" \
		CGO_ENABLED=1 go build -tags krucible -ldflags="-X main.version=$(VERSION)" -o bhatti-vmm ./cmd/vmm/
	@if [ "$$(uname -s)" = "Darwin" ]; then \
		codesign --force --entitlements cmd/vmm/hvf-entitlements.plist -s - bhatti-vmm && \
		echo "codesigned bhatti-vmm for HVF"; \
	fi
	@echo "Built bhatti-vmm (links libkrucible if built, else system libkrun)"

test:
	go test ./... -count=1 -timeout 120s

# Cross-compile for all platforms
release:
	@mkdir -p dist
	GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w -X main.version=$(VERSION)" \
		-o dist/bhatti-darwin-arm64 ./cmd/bhatti/
	GOOS=darwin GOARCH=amd64 go build -ldflags="-s -w -X main.version=$(VERSION)" \
		-o dist/bhatti-darwin-amd64 ./cmd/bhatti/
	GOOS=linux GOARCH=amd64 go build -ldflags="-s -w -X main.version=$(VERSION)" \
		-o dist/bhatti-linux-amd64 ./cmd/bhatti/
	GOOS=linux GOARCH=arm64 go build -ldflags="-s -w -X main.version=$(VERSION)" \
		-o dist/bhatti-linux-arm64 ./cmd/bhatti/
	@echo "Built $(VERSION) for 4 platforms in dist/"

clean:
	rm -f bhatti lohar bhatti-vmm
	rm -rf dist/
