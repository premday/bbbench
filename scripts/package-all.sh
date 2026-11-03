#!/bin/sh
# Build the archives for every supported architecture plus SHA256SUMS and
# build/RELEASE_NOTES.md (generated from git history, also shipped in each archive).
# Usage: [REF=<tag>] scripts/package-all.sh <version>
set -eu
[ $# -eq 1 ] || { echo "usage: $0 <version>" >&2; exit 2; }
root=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$root/build"
"$root/scripts/changelog.sh" "$1" "${REF:-HEAD}" >"$root/build/RELEASE_NOTES.md"
for target in "amd64" "arm64" "386"; do
	# shellcheck disable=SC2086
	"$root/scripts/package.sh" "$1" $target
done
(cd "$root/build" && sha256sum ./*.tar.gz | sed 's# \./# #' >SHA256SUMS)
