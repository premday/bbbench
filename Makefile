.PHONY: all clean test check package

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
COMMIT  ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG     := bbbench/internal/buildinfo
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)

# Default target: static binaries. The code is pure Go, so CGO is disabled and
# the result is statically linked without needing a musl toolchain.
all: bbbench check-config

bbbench:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" ./cmd/bbbench

check-config:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" ./cmd/check-config

clean:
	rm -f bbbench check-config
	rm -rf build

test:
	go test ./...

# Same checks as the CI "test" job.
check:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "gofmt needed"; exit 1; }
	go vet ./...
	go test -race ./...

# Release archives for every supported architecture, in ./build.
package:
	scripts/package-all.sh $(VERSION)
