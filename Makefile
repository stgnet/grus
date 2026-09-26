# Grus: build, check and install.
#
#   make install         everything: from a fresh machine (or over a running
#                        node) to a running node; see deploy/install.sh
#   make                 build ./grus for this machine (for development)
#   make check           what CI runs: gofmt, vet, race tests, static build
#   make dist            binaries for the starting setup's two machines
#
# make install is the only command an operator needs. It installs Go if
# it's missing, builds as you (using sudo only for the steps that need
# root), and asks what it needs the first time.

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

install:
	deploy/install.sh

# Build on one machine, copy to the others: the VPS is Linux on x86-64,
# the Studio is macOS on Apple silicon.
dist:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 $(GO) build -o dist/grus-linux-amd64  ./cmd/grus
	CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 $(GO) build -o dist/grus-linux-arm64  ./cmd/grus
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -o dist/grus-darwin-arm64 ./cmd/grus

clean:
	rm -rf $(BIN) dist
