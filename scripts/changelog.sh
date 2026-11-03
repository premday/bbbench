#!/bin/sh
# Generate release notes from git history.
#
# Usage: scripts/changelog.sh <version> [ref]
#   version  label used in the title (e.g. 0.2.0)
#   ref      commit or tag being released (default HEAD)
#
# Lists the non-merge commits since the previous tag reachable from <ref>
# (or the whole history if there is none), newest first, as markdown.
set -eu
[ $# -ge 1 ] || { echo "usage: $0 <version> [ref]" >&2; exit 2; }
version=$1
ref=${2:-HEAD}

prev=$(git describe --tags --abbrev=0 "$ref^" 2>/dev/null || true)
if [ -n "$prev" ]; then
	range="$prev..$ref"
else
	range=$ref
fi

echo "## $version"
echo
if [ -n "$prev" ]; then
	echo "Changes since $prev."
else
	echo "First release: all changes."
fi
echo
git log --no-merges --format='- %s (%h)' "$range" |
	grep -v '^- fixup! \|^- amend! \|^- squash! ' || true
