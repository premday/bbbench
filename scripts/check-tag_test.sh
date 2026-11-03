#!/bin/sh
# Table test for scripts/check-tag.sh.
set -u
dir=$(cd "$(dirname "$0")" && pwd)
fail=0
expect() { # expect <accept|reject> <tag>
	if "$dir/check-tag.sh" "$2" >/dev/null 2>&1; then got=accept; else got=reject; fi
	if [ "$got" != "$1" ]; then
		echo "FAIL: '$2' should be ${1}ed, was ${got}ed" >&2
		fail=1
	fi
}
for t in v0.0.0 v0.1.0 v1.2.3 v10.20.30 v1.0.0 v0.0.1; do expect accept "$t"; done
for t in 1.2.3 v1 v1.2 v1.2.3.4 v1.2.3-rc1 v1.2.3+build V1.2.3 v01.2.3 v1.02.3 v1.2.03 \
	v1.2.x va.b.c v1..3 v.1.2 "v1.2.3 " "" release-1 latest v1.2.3/foo; do expect reject "$t"; done
expect reject "v1.2.3
foo"
[ $fail -eq 0 ] && echo "check-tag tests ok"
exit $fail
