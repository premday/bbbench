#!/bin/sh
# Check that a release tag moves the version forward.
#
# Usage: scripts/check-tag-order.sh [--new] <tag>
#
# The tag must be strictly higher (numeric major.minor.fix comparison, not
# alphabetical or by date) than every other vMAJOR.MINOR.FIX tag in the
# repository. With --new (use it before creating a tag) the tag must also
# not exist yet. The release workflow runs without --new, because the pushed
# tag is what triggers it; it checks for an existing GitHub release instead.
# Needs the tags fetched (checkout with fetch-depth: 0).
set -eu
new=0
if [ "${1:-}" = --new ]; then new=1; shift; fi
[ $# -eq 1 ] || { echo "usage: $0 [--new] <tag>" >&2; exit 2; }
tag=$1
re='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'

if [ $new -eq 1 ] && git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
	echo "rejected tag '$tag': it already exists" >&2
	exit 1
fi

latest=$(git tag -l 'v*' | grep -E "$re" | grep -vxF "$tag" |
	sort -t. -k1.2,1n -k2,2n -k3,3n | tail -n 1 || true)
[ -n "$latest" ] || exit 0 # first release

highest=$(printf '%s\n%s\n' "$latest" "$tag" |
	sort -t. -k1.2,1n -k2,2n -k3,3n | tail -n 1)
if [ "$highest" != "$tag" ]; then
	echo "rejected tag '$tag': it must be higher than the latest existing release tag '$latest'" >&2
	exit 1
fi
