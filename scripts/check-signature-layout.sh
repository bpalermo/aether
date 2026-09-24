#!/usr/bin/env bash
# Exercise ghcr_signature_layout() over every state it can report.
#
# WHY THIS IS A SCRIPT AND NOT A COMMENT. The verifier's signature check accepts
# two tag shapes now -- cosign 2's `sha256-<digest>.sig` and cosign 3's bare
# `sha256-<digest>` fallback index -- because everything published before the v3
# migration carries the former and must stay verifiable. "Accept either" is one
# `grep -q` away from "accept anything", and the state that matters most is the
# one nobody thinks to test: BOTH tags present, which is what a double-write or a
# half-finished migration looks like and which presence-only checking reports as
# a healthy signature.
#
# It also pins a subtlety that is easy to lose in a refactor: `sha256-<d>.sig`
# must NOT satisfy the bare-tag check. `grep -qxF` matches whole lines, so it
# does not today; a change to `grep -qF` would silently make every legacy
# signature also look like a bundle signature, i.e. report `both` for everything.
#
# No registry access: the tag lists are literals.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2
# shellcheck disable=SC1091
. scripts/ghcr-lib.sh

digest="sha256:abc123"
legacy="sha256-abc123.sig"
bundle="sha256-abc123"
fail=0
n=0

check() {
	local want="$1" got
	shift
	got="$(ghcr_signature_layout "$digest" "$(printf '%s\n' "$@")")"
	n=$((n + 1))
	if [ "$got" = "$want" ]; then
		printf '  ok    %-6s <- [%s]\n' "$got" "$*"
	else
		printf '  FAIL  want=%s got=%s <- [%s]\n' "$want" "$got" "$*"
		fail=1
	fi
}

check legacy "$legacy" other-tag
check bundle "$bundle" other-tag
check both "$legacy" "$bundle"
check none other-tag dev-deadbeef
# The subtlety above, asserted directly rather than implied.
check legacy "$legacy"

if [ "$n" -ne 5 ]; then
	echo "::error::ran ${n} cases, expected 5 -- a gate that checks nothing passes" >&2
	exit 2
fi
if [ "$fail" -ne 0 ]; then
	echo "::error::signature layout resolution is wrong; see cases above" >&2
	exit 1
fi
echo "signature layout: ${n}/${n} cases correct"
