.PHONY: all bbbench check-config clean test check check-tag package

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
# Outside a git checkout (source tarball) or without tags there is nothing to
# derive a version from: fall back to "dev" rather than an empty string.
ifeq ($(VERSION),)
VERSION := dev
endif
COMMIT  ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG     := bbbench/internal/buildinfo
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)

# bbbench and check-config are .PHONY: they are always rebuilt (the Go build
# cache makes that cheap), so make never leaves a stale binary after a source
# change.
#
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
	scripts/check-tag_test.sh
	scripts/check-tag-order_test.sh
	go vet ./...
	go test -race ./...

# Before tagging: `make check-tag TAG=v1.2.3` (format, not existing, higher than all others).
check-tag:
	scripts/check-tag.sh $(TAG)
	scripts/check-tag-order.sh --new $(TAG)

# Release archives for every supported architecture, in ./build.
package:
	scripts/package-all.sh $(VERSION)
