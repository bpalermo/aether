#!/usr/bin/env bash
# scripts/check-proxy-stats-tags.sh over the repository's own files, and over
# changed copies of them, so that the check is known to be able to fail.
#
#   1. the chart template and the two proxy C++ tests agree          -> exit 0
#   2. the chart's aether.pod regex changes, the tests do not        -> exit 1,
#      naming aether.pod (what chart 2.6.0 did)
#   3. a test loses one extractor                                    -> exit 1
#   4. a test gains an extractor the chart does not have             -> exit 1
#   5. a template with no stats_tags                                 -> exit 2
#   6. a C++ file the check reads no extractor from                  -> exit 2
#
# The inputs come as arguments: the template, then the C++ files. Under Bazel
# the C++ files are //scripts/proxy_stats_tags/*, symlinks into the proxy
# workspace (//.bazelignore hides `proxy`, so no label can name a file there).
#
# Run: bazel test //scripts:proxy_stats_tags_test, or
#      bash scripts/proxy_stats_tags_test.sh \
#        charts/aether/templates/agent-proxy-configmap.yaml \
#        proxy/source/extensions/filters/http/aether_stats/{tag_extraction_test,aether_stats_integration_test}.cc
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
CHECK="$HERE/check-proxy-stats-tags.sh"

if [ "$#" -lt 2 ]; then
	echo "usage: $0 <chart template> <c++ file>..." >&2
	exit 2
fi
template="$1"
shift
first_cc="$1"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FAILS=0
pass() { echo "PASS  $*"; }
fail() {
	echo "FAIL  $*"
	FAILS=$((FAILS + 1))
}

# expect <what> <status> <text the output must hold, or ""> <check arguments>...
expect() {
	local what="$1" want_rc="$2" want_text="$3" out rc
	shift 3
	out="$("$CHECK" "$@" 2>&1)"
	rc=$?
	if [ "$rc" -ne "$want_rc" ]; then
		fail "$what: exit $rc, want $want_rc"
		printf '%s\n' "$out" | sed 's/^/      /'
		return
	fi
	if [ -n "$want_text" ] && ! grep -qF -- "$want_text" <<<"$out"; then
		fail "$what: the output does not hold '$want_text'"
		printf '%s\n' "$out" | sed 's/^/      /'
		return
	fi
	pass "$what"
}

# changed <file> <copy> — fails the test when an edit meant to change a copy
# left it equal to the original: the case it was written for would be vacuous.
changed() {
	if cmp -s "$1" "$2"; then
		fail "the edit that makes $(basename "$2") did not change it: the case is vacuous"
	fi
}

expect "the chart and the proxy tests agree" 0 "" "$template" "$@"

# 2. The chart side moves alone. `[^_.]+` is the namespace part of the strict
# aether.pod regex; making it optional is the 2.5.x form.
sed -E '/tag_name: aether\.pod$/,/regex:/ s/out_http\)_\[\^_\.\]\+\(/out_http)(?:_[^_.]+)?(/' "$template" >"$TMP/chart-pod.yaml"
changed "$template" "$TMP/chart-pod.yaml"
expect "a chart regex the tests do not have" 1 "aether.pod" "$TMP/chart-pod.yaml" "$@"

# 3. A test drops the aether.namespace extractor (either statement form).
grep -v 'aether\.namespace' "$first_cc" >"$TMP/dropped.cc"
changed "$first_cc" "$TMP/dropped.cc"
expect "a test without one of the chart's extractors" 1 "aether.namespace" "$template" "$TMP/dropped.cc"

# 4. A test adds one the chart does not ship.
{
	cat "$first_cc"
	printf '%s\n' '  add("reporter", "(\\.reporter\\.([^.]*))");'
} >"$TMP/extra.cc"
expect "a test with an extractor the chart does not ship" 1 "reporter" "$template" "$TMP/extra.cc"

# 5 and 6. Nothing to compare is an error of the check, not a pass.
grep -v 'stats_tags:' "$template" >"$TMP/no-tags.yaml"
changed "$template" "$TMP/no-tags.yaml"
expect "a template with no stats_tags" 2 "found no stats_tags extractor" "$TMP/no-tags.yaml" "$@"
echo 'int main() { return 0; }' >"$TMP/none.cc"
expect "a C++ file with no extractor" 2 "found no stats tag extractor" "$template" "$TMP/none.cc"

if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS case(s) failed"
	exit 1
fi
echo "all cases passed"
