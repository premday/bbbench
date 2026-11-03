#!/bin/sh
# Table test for scripts/check-tag-order.sh, in a throwaway git repository.
set -u
script=$(cd "$(dirname "$0")" && pwd)/check-tag-order.sh
repo=$(mktemp -d)
trap 'rm -rf "$repo"' EXIT
cd "$repo" || exit 1
git init -q . && git -c user.name=t -c user.email=t@t commit -q --allow-empty -m init
fail=0
expect() { # expect <accept|reject> <args...>
	want=$1; shift
	if "$script" "$@" >/dev/null 2>&1; then got=accept; else got=reject; fi
	[ "$got" = "$want" ] || { echo "FAIL: $* should be ${want}ed, was ${got}ed" >&2; fail=1; }
}

# No tags yet: anything valid goes.
expect accept --new v0.1.0
for t in v0.1.0 v0.9.0 v0.10.0 v1.2.3; do git tag "$t"; done
# Not tags of the release form, ignored by the comparison.
git tag v9.9.9-rc1; git tag vfoo

expect accept --new v1.2.4
expect accept --new v1.3.0
expect accept --new v2.0.0
expect reject --new v1.2.3   # exists
expect reject --new v1.2.2   # lower than latest
expect reject --new v1.1.9
expect reject --new v0.10.1  # lower than v1.2.3
expect reject --new v0.2.0   # alphabetical order would wrongly accept after v0.10.0
# Without --new the tag itself is present (as in the release workflow).
expect accept v1.2.3
git tag v1.2.4
expect reject v1.2.3         # a higher tag now exists
expect accept v1.2.4
[ $fail -eq 0 ] && echo "check-tag-order tests ok"
exit $fail
