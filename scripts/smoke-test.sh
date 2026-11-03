#!/bin/sh
# Smoke-test a release archive: it must contain everything and the binary
# must be static, report the expected version and find its bundled templates.
# Only meaningful for the host architecture (the binary is executed).
# Usage: scripts/smoke-test.sh <archive.tar.gz> <version>
set -eu
[ $# -eq 2 ] || { echo "usage: $0 <archive.tar.gz> <version>" >&2; exit 2; }
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
tar -C "$tmp" -xzf "$1"
cd "$tmp"/*/
for f in bbbench check-config README.md LICENSE CHANGELOG.md documentation/web-ui.md dist/default.yml dist/templates/disk.fio.gotmpl; do
	[ -e "$f" ] || { echo "missing from archive: $f" >&2; exit 1; }
done
if ldd ./bbbench >/dev/null 2>&1; then
	echo "bbbench is not statically linked" >&2
	exit 1
fi
[ "$(./bbbench version)" = "bbbench $2" ] || { echo "unexpected version: $(./bbbench version)" >&2; exit 1; }
./bbbench validate-templates
echo "smoke test ok: $1"
