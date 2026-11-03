#!/bin/sh
# Accept only release tags of the form vMAJOR.MINOR.PATCH, e.g. v1.2.3:
# MAJOR = major release, MINOR = minor release, PATCH = fix. Each is a
# non-negative integer without leading zeros; no pre-release or build
# suffix (v1.2.3-rc1), no missing parts (v1.2), no missing "v" (1.2.3).
# Usage: scripts/check-tag.sh <tag>
set -eu
[ $# -eq 1 ] || { echo "usage: $0 <tag>" >&2; exit 2; }
case $1 in
*'
'*) echo "rejected tag: contains a newline" >&2; exit 1 ;;
esac
if printf '%s\n' "$1" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
	exit 0
fi
echo "rejected tag '$1': release tags must look like v1.2.3 (v<major>.<minor>.<fix>, numbers without leading zeros, no suffix)" >&2
exit 1
