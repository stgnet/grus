# Grus: build, check and install.
#
#   make                 build ./grus for this machine
#   make check           what CI runs: gofmt, vet, race tests, static build
#   sudo make install    install the binary and the service for this OS
#   make dist            binaries for the starting setup's two machines
#
# Build as yourself and install with sudo, as two steps. Under sudo, root's
# PATH usually has no Go, and a build as root would leave root-owned files
# in your module cache. So `install` never builds; it uses the ./grus that
# `make` left.

GO    ?= go
BIN   := grus
OS    := $(shell uname -s)

.PHONY: build check install dist clean

# CGO off: SQLite is pure Go (modernc), so the binary is fully static and
# copies to any machine of the same OS and architecture with nothing else
# installed.
build:
	CGO_ENABLED=0 $(GO) build -o $(BIN) ./cmd/grus

# The same steps, in the same order, as .github/workflows/ci.yml, so a
# green `make check` means a green CI run.
check:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	$(GO) vet ./...
	$(GO) test -race ./...
	CGO_ENABLED=0 $(GO) build -o /dev/null ./cmd/grus

# The OS decides the service manager: systemd on the Linux VPS, launchd on
# the Studio. Each script is safe to re-run: it upgrades the binary and
# service, keeps an existing config, and restarts the service.
install:
	@test -x $(BIN) || (echo "No ./$(BIN) yet: run 'make' first, as yourself (not with sudo)."; exit 1)
ifeq ($(OS),Linux)
	deploy/install-service.sh
else ifeq ($(OS),Darwin)
	deploy/install-launchd.sh
else
	@echo "No service setup for $(OS); copy ./$(BIN) to /usr/local/bin by hand."; exit 1
endif

# Build on one machine, copy to the others: the VPS is Linux on x86-64,
# the Studio is macOS on Apple silicon.
dist:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 $(GO) build -o dist/grus-linux-amd64  ./cmd/grus
	CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 $(GO) build -o dist/grus-linux-arm64  ./cmd/grus
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -o dist/grus-darwin-arm64 ./cmd/grus

clean:
	rm -rf $(BIN) dist
