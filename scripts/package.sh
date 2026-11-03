#!/bin/sh
# Build a static linux release archive of bbbench.
#
# Usage: scripts/package.sh <version> <goarch> [goarm]
#   e.g. scripts/package.sh 0.2.0 amd64
#        scripts/package.sh 0.2.0 arm 7
#
# Output: build/bbbench_<version>_linux_<arch>.tar.gz, containing a single
# top-level directory with the binaries (bbbench, check-config), README,
# LICENSE, CHANGELOG, documentation/ and the dist/ config and templates
# (so ./dist is found when bbbench runs from the extracted directory).
set -eu

if [ $# -lt 2 ]; then
	echo "usage: $0 <version> <goarch> [goarm]" >&2
	exit 2
fi
version=$1
goarch=$2
goarm=${3:-}

arch=$goarch
[ "$goarch" = arm ] && arch=armv${goarm:-7}

root=$(cd "$(dirname "$0")/.." && pwd)
name=bbbench_${version}_linux_${arch}
stage=$root/build/$name
rm -rf "$stage"
mkdir -p "$stage/dist"

# Pure Go (no cgo): the result is statically linked on every architecture.
export CGO_ENABLED=0 GOOS=linux GOARCH=$goarch
[ -n "$goarm" ] && export GOARM=$goarm
ldflags="-s -w -X main.version=$version"
(cd "$root" &&
	go build -trimpath -ldflags "$ldflags" -o "$stage/bbbench" ./cmd/bbbench &&
	go build -trimpath -ldflags "$ldflags" -o "$stage/check-config" ./cmd/check-config)

cp "$root"/README.md "$root"/LICENSE "$root"/CHANGELOG.md "$stage/"
cp -r "$root/documentation" "$stage/documentation"
cp "$root/dist/default.yml" "$stage/dist/"
cp -r "$root/dist/templates" "$stage/dist/templates"

# Reproducible archive: fixed ordering, owner and mtime.
tar -C "$root/build" --sort=name --owner=0 --group=0 --numeric-owner \
	--mtime='UTC 2020-01-01' -cf - "$name" | gzip -9n >"$root/build/$name.tar.gz"
rm -rf "$stage"
echo "$root/build/$name.tar.gz"
