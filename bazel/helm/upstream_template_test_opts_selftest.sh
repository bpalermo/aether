#!/usr/bin/env bash
# //bazel/helm:upstream_template_test_opts_selftest — the option guard
# (upstream_template_test_opts_test.sh) accepts a helm_template_test only when
# the option is an element of its `opts` list that directly follows `"--set",`.
#
# Two inputs under testdata/opts_guard: one in which every call passes the
# option (the guard must pass), and one in which exactly one does and the rest
# hold the option's text where it proves nothing: in a comment, in another
# attribute, after another flag, after another --set value (the guard must fail
# and name each of those, and only those).
#
# Usage: upstream_template_test_opts_selftest.sh <guard> <good input> <bad input>
set -uo pipefail

guard="$1" good="$2" bad="$3"
opt="controller.webhook.spire=true"
fail=0

ok() { printf '  ok    %s\n' "$1"; }
no() {
	printf '  FAIL  %s\n' "$1"
	fail=1
}

out="$("$guard" "$good" "$opt" 2>&1)"
status=$?
if [[ "$status" -eq 0 && "$out" == *"PASS: all 2 helm_template_test targets"* ]]; then
	ok "every call passes the option: the guard passes, having read 2 calls"
else
	no "the good input: exit $status, output: $out"
fi

out="$("$guard" "$bad" "$opt" 2>&1)"
status=$?
if [[ "$status" -ne 0 ]]; then
	ok "the bad input fails the guard"
else
	no "the bad input passed the guard: $out"
fi
named="$(sed -n 's/^  \([a-z_]*\)$/\1/p' <<<"$out" | sort | tr '\n' ' ')"
want="after_another_flag comment_before_opts in_another_attribute no_opts only_in_a_comment set_of_something_else "
if [[ "$named" == "$want" ]]; then
	ok "it names exactly the six calls that do not pass the option"
else
	no "named: ${named:-nothing}; want: $want"
fi

# An input with no call at all is a guard that reads nothing.
empty="${TEST_TMPDIR:?}/empty.BUILD.txt"
: >"$empty"
if "$guard" "$empty" "$opt" >/dev/null 2>&1; then
	no "an input with no helm_template_test passed the guard"
else
	ok "an input with no helm_template_test fails the guard"
fi

[[ "$fail" -eq 0 ]] || exit 1
echo "PASSED"
