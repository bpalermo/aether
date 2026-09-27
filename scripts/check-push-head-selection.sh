#!/usr/bin/env bash
# Exercise select_push_heads() (scripts/push-heads-lib.sh) over the shapes that
# matter (#975).
#
# The `--recent` sweep in verify-published-artifacts.sh narrowed from "every
# commit on main" to "every push head on main". A narrowing is exactly where a
# gate goes vacuous, so this pins both directions:
#
#   - the #975 shape: a stack merge's intermediates are `skip`, its head is
#     `check` — and a head is `check` WHETHER OR NOT its artifacts exist, since
#     selection never looks at the registry. That is what keeps a push head with
#     missing artifacts red.
#   - fail-closed inputs: an empty head list or an abbreviated sha returns 2,
#     rather than classifying everything `skip` and passing on nothing (#853).
#
# No network, no git: the shas are literals.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2
# shellcheck disable=SC1091
. scripts/push-heads-lib.sh

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# The real #975 stack (gh-stack #963): one push, head 4930733.
d74="d74d77a9e03a3a075a87e2a4a7654790ca47408e"
s690="690dbb5409f31339aad2d33f981297a1ac7a96a5"
s937="937a8fd28894cd08b02e4a9e053dd6ceadd63de8"
ef4="ef444376e113eaba36762ccab328f087c869c279"
head="4930733aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" # stand-in: shape is what matters
# A single-commit push after it, and one before it.
after="e28acc2bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
before="e731d6dccccccccccccccccccccccccccccccccc"

printf '%s\n' "$after" "$head" "$before" >"$tmp/heads"

fail=0
n=0

expect() {
	local name="$1" want="$2" got="$3"
	n=$((n + 1))
	if [ "$got" = "$want" ]; then
		printf '  ok    %s\n' "$name"
	else
		printf '  FAIL  %s\n    want: %s\n    got:  %s\n' "$name" "$(echo "$want" | tr '\n' '|')" "$(echo "$got" | tr '\n' '|')"
		fail=1
	fi
}

# 1. The #975 window, newest-first as `git log` gives it.
got="$(printf '%s\n' "$after" "$head" "$ef4" "$s937" "$s690" "$d74" "$before" |
	select_push_heads "$tmp/heads")"
want="check $after
check $head
skip $ef4
skip $s937
skip $s690
skip $d74
check $before"
expect "stack intermediates skipped, every push head checked" "$want" "$got"

# 2. Order is preserved and nothing is dropped or invented.
expect "one output line per candidate" "7" "$(printf '%s\n' "$got" | grep -c .)"

# 3. A window of only intermediates yields zero checks — the caller must then
#    fall back to an older head rather than pass. Asserted so a change that
#    silently promoted intermediates would show up here.
got="$(printf '%s\n' "$ef4" "$s937" | select_push_heads "$tmp/heads")"
expect "intermediates-only window checks nothing" "0" "$(printf '%s\n' "$got" | grep -c '^check ' || true)"

# 4. Fail closed: empty head list.
: >"$tmp/empty"
printf '%s\n' "$head" | select_push_heads "$tmp/empty" >/dev/null 2>&1
expect "empty head list returns 2" "2" "$?"

# 5. Fail closed: an abbreviated sha in the head list.
printf '%s\n' "4930733" >"$tmp/abbrev"
printf '%s\n' "$head" | select_push_heads "$tmp/abbrev" >/dev/null 2>&1
expect "abbreviated head sha returns 2" "2" "$?"

# 6. An abbreviated CANDIDATE never matches a full head by prefix (grep -x).
got="$(printf '%s\n' "4930733" | select_push_heads "$tmp/heads")"
expect "prefix of a head is not a head" "skip 4930733" "$got"

if [ "$n" -ne 6 ]; then
	echo "::error::ran ${n} cases, expected 6 -- a gate that checks nothing passes" >&2
	exit 2
fi
if [ "$fail" -ne 0 ]; then
	echo "::error::push-head selection is wrong; see cases above" >&2
	exit 1
fi
echo "push-head selection: ${n}/${n} cases correct"
