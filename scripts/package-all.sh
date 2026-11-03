#!/bin/sh
# Build the archives for every supported architecture plus SHA256SUMS.
# Usage: scripts/package-all.sh <version>
set -eu
[ $# -eq 1 ] || { echo "usage: $0 <version>" >&2; exit 2; }
root=$(cd "$(dirname "$0")/.." && pwd)
for target in "amd64" "arm64" "386"; do
	# shellcheck disable=SC2086
	"$root/scripts/package.sh" "$1" $target
done
(cd "$root/build" && sha256sum ./*.tar.gz | sed 's# \./# #' >SHA256SUMS)
