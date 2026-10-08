#!/usr/bin/env bash
# //bazel/helm:template_rules_test — a failing chart test tells its reader what
# did not match and where, and never prints Secret data (#1382).
#
# The rules under test are test rules, so "what does it print when it fails"
# can only be asked of a run that fails. //bazel/helm/testdata/secretchart
# holds such runners (the rules as plain executables: a target that fails by
# design cannot be a test); this runs each one, requires it to
# fail, and reads its output:
#   - no FORBIDDEN byte: every value of every Secret in that chart starts with
#     FORBIDDEN, in each shape a Secret's data takes (plain keys, stringData, a
#     block scalar, a flow mapping, a Secret inside a List, a `kind:` line with
#     a trailing comment, flow mappings and JSON, a kind that is anchored,
#     tagged, aliased or missing, explicit and block-scalar keys, a merge key). The control is
#     :documents_test, which matches those values in the same render;
#   - the pattern, the document searched, and an excerpt in which a document
#     that might be a Secret shows its identity lines and `<masked>`;
#   - bounded: a failure is a screenful, not the render.
#
# The masking is awk, so the whole thing is repeated with every awk this host
# has (gawk, mawk, busybox) besides the default one.
#
# Usage: template_rules_test.sh <directory of the secretchart runners> <render_lib.sh>
set -uo pipefail

dir="$1" lib="$2"
scratch="${TEST_TMPDIR:?}/template_rules"
mkdir -p "$scratch"
fail=0

ok() { printf '  ok    %s\n' "$1"; }
bad() {
	printf '  FAIL  %s\n' "$1"
	fail=1
}

# run <target> <want status: zero|nonzero>: leaves the combined output in $out.
run() {
	local status
	out="$scratch/$1.out"
	"$dir/$1.sh" >"$out" 2>&1
	status=$?
	if [[ "$2" == zero && "$status" -eq 0 ]] || [[ "$2" == nonzero && "$status" -ne 0 ]]; then
		ok "$1 exits $2"
	else
		bad "$1: exit $status, want $2"
		# Safe to show only because the next check is that it holds no Secret.
		grep -v FORBIDDEN "$out" | head -n 30
	fi
	if grep -q FORBIDDEN "$out"; then
		bad "$1 prints Secret data ($(grep -c FORBIDDEN "$out") lines hold it)"
	else
		ok "$1 prints no Secret data"
	fi
}

# says <target> <literal>: the output holds that text.
says() {
	if grep -qF -- "$2" "$out"; then
		ok "$1 says: $2"
	else
		bad "$1 does not say: $2"
	fi
}

# silent_about <target> <literal>: the output does not hold that text.
silent_about() {
	if grep -qF -- "$2" "$out"; then
		bad "$1 says: $2"
	else
		ok "$1 does not say: $2"
	fi
}

# excerpt_silent_about <target> <literal>: no excerpt line (the numbered lines
# taken from the render) holds that text. The pattern a runner echoes may.
excerpt_silent_about() {
	if grep -E '^ +[0-9]+[> ] ' "$out" | grep -qF -- "$2"; then
		bad "$1 shows in an excerpt: $2"
	else
		ok "$1 shows in no excerpt: $2"
	fi
}

# at_most <target> <lines>
at_most() {
	local lines
	lines="$(wc -l <"$out")"
	if [[ "$lines" -le "$2" ]]; then
		ok "$1 prints $lines lines (at most $2)"
	else
		bad "$1 prints $lines lines, want at most $2"
	fi
}

checks() {
	# The control: the Secret values ARE in the render these targets search.
	run documents_test zero
	says documents_test "PASS: all 20 patterns match."

	run failing_whole_render nonzero
	says failing_whole_render "FAIL: the whole render does not match the pattern:"
	says failing_whole_render '  kind: Secret\nmetadata:\n  name: release-name-plain\ntype: Opaque\ndata:\n  token: not-the-token\n'
	says failing_whole_render "the first 5 of the pattern's 6 lines match (lines "
	# Of a possible Secret only its identity is printed: no key, no value.
	says failing_whole_render ">   <masked>"
	excerpt_silent_about failing_whole_render "token"
	excerpt_silent_about failing_whole_render "tls.key"
	says failing_whole_render "(it matches nowhere in the whole render)"
	says failing_whole_render "the whole render is never printed)"
	# The excerpt stops: the Deployment is 80 lines further down.
	silent_about failing_whole_render "kind: Deployment"
	# A Secret in flow style is masked from its opening brace.
	says failing_whole_render "the first 1 of the pattern's 2 lines match"
	silent_about failing_whole_render "flow-document"
	at_most failing_whole_render 45

	run failing_documents nonzero
	says failing_documents "FAIL: Secret/release-name-block (secretchart/templates/multi.yaml, document 2) does not match the pattern:"
	says failing_documents "  <masked>"
	excerpt_silent_about failing_documents "password"
	excerpt_silent_about failing_documents "quoted.key"
	excerpt_silent_about failing_documents "token"
	says failing_documents "FAIL: Secret/release-name-flow (secretchart/templates/multi.yaml, document 3) does not match the pattern:"
	says failing_documents "FAIL: List/release-name-list (secretchart/templates/list.yaml, document 1) does not match the pattern:"
	# A ConfigMap is not a Secret: its data is what the reader needs to see.
	says failing_documents "FAIL: ConfigMap/release-name-config (secretchart/templates/multi.yaml, document 4) does not match the pattern:"
	says failing_documents "  greeting: VISIBLE-config-value"
	says failing_documents "FAIL: Deployment/release-name-app (secretchart/templates/multi.yaml, document 7) does not match the pattern:"
	says failing_documents "(not even the pattern's first line matches anywhere in Deployment/release-name-app (secretchart/templates/multi.yaml, document 7):   replicas: 3)"
	says failing_documents "FAIL: Secret/release-name-spaced (secretchart/templates/spaced.yaml, document 1) does not match the pattern:"
	at_most failing_documents 85

	run failing_missing_document nonzero
	says failing_missing_document "FAIL: the render has no document Role/no-such-role (a document is found by"
	says failing_missing_document "  Role/release-name-reader (secretchart/templates/multi.yaml, document 5)"
	says failing_missing_document "  Secret/release-name-plain (secretchart/templates/multi.yaml, document 1)"
	at_most failing_missing_document 23

	run failing_flow_document nonzero
	says failing_flow_document "FAIL: the render has no document Secret/flow-document (a document is found by its 'kind:' line"
	says failing_flow_document "use patterns for it). It has:"
	at_most failing_flow_document 24

	# The derived-value test names the key and counts; it shows no rendered line.
	run failing_value_changes_count nonzero
	says failing_value_changes_count "FAIL: expected exactly one rendered, non-comment line holding 'token' (opts), found "
	silent_about failing_value_changes_count "token:"
	at_most failing_value_changes_count 2
	run failing_value_changes_same nonzero
	says failing_value_changes_same "FAIL: the line holding 'tls.key:' is the same with and without the changed values (the line is not shown)"
	at_most failing_value_changes_same 2

	# Fail closed: a document that is not plainly something else is masked,
	# whatever its kind line looks like; a plain ConfigMap is not.
	run failing_unreadable nonzero
	for doc in anchored tagged aliased kindless; do
		says failing_unreadable "  name: release-name-$doc"
	done
	if [[ "$(grep -c '^ *[0-9]*> *<masked>$' "$out")" -eq 4 ]]; then
		ok "failing_unreadable masks what follows the name in all four"
	else
		bad "failing_unreadable: $(grep -c '^ *[0-9]*> *<masked>$' "$out") of 4 shown as masked"
	fi
	excerpt_silent_about failing_unreadable "token"
	says failing_unreadable ">   greeting: VISIBLE-plain-config-value"
	says failing_unreadable ">   blob: VISIBLE-binary-config-value"
	at_most failing_unreadable 120

	# Values no `data:` line leads to (an explicit key, a block-scalar key, a
	# merge key): nothing after the name is printed.
	run failing_explicit nonzero
	for doc in explicit-key multiline-key merge typed-list nested-anchored nested-explicit; do
		says failing_explicit "  name: release-name-$doc"
	done
	excerpt_silent_about failing_explicit "token"
	excerpt_silent_about failing_explicit "payload"
	at_most failing_explicit 130

	run failing_invalid_pattern nonzero
	says failing_invalid_pattern "FAIL: the pattern is not a valid extended regular expression:"
	at_most failing_invalid_pattern 5

	# A failed render: helm's error is there, the render --debug printed is not.
	for t in failing_render failing_fail_test_other_reason failing_absent_test_render failing_version_test_render failing_reproducible_test_render; do
		run "$t" nonzero
		says "$t" "Error: YAML parse error on secretchart/templates/broken.yaml"
		silent_about "$t" "kind: "
		at_most "$t" 12
		case "$t" in
		failing_fail_test_other_reason) says "$t" "FAIL: 'helm template' failed as expected, but not for the expected reason." ;;
		*) says "$t" "FAIL: 'helm template' did not render" ;;
		esac
	done

	run failing_fail_test_rendered nonzero
	says failing_fail_test_rendered "FAIL: expected 'helm template' to be rejected, but it rendered successfully"
	silent_about failing_fail_test_rendered "kind:"
	at_most failing_fail_test_rendered 3
}

# The pattern dialect: `\n`, `\s` and `\S` leave as a line break and POSIX
# classes, so nothing reaches regcomp that only glibc's takes.
echo "== pattern escapes"
# shellcheck source=/dev/null
source "$lib"
ere=""
translate_pattern 'a\S+\sb\nc'
want=$'a[^[:space:]]+[[:space:]]b\nc'
if [[ "$ere" == "$want" ]]; then
	ok 'translate_pattern spells \S, \s and \n out'
else
	bad "translate_pattern: got '$ere', want '$want'"
fi

# What helm says on stderr is printed as it is, unless it is a manifest: a JSON
# or flow-style Secret there has no `kind:` line to give it away, and must not
# print. An ordinary error line, and one opening with a bracketed tag, must.
stderr_checks() {
	local shown
	shown="$(show_helm_failure '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"x"},"data":{"token":"FORBIDDEN-stderr-json"}}' 2>&1)"
	if [[ "$shown" == *FORBIDDEN* ]]; then
		bad "show_helm_failure prints a JSON Secret that helm put on stderr"
	else
		ok "show_helm_failure masks a JSON Secret on stderr"
	fi
	shown="$(show_helm_failure 'Error: template: chart/templates/a.yaml:3:7: nil pointer' 2>&1)"
	if [[ "$shown" == *"nil pointer"* ]]; then
		ok "show_helm_failure prints an error message as it is"
	else
		bad "show_helm_failure hides an ordinary error message: $shown"
	fi
	shown="$(show_helm_failure '[ERROR] templates/: boom' 2>&1)"
	if [[ "$shown" == *boom* ]]; then
		ok "show_helm_failure prints a message that opens with a bracketed tag"
	else
		bad "show_helm_failure hides a bracketed message: $shown"
	fi
}

echo "== awk: the default"
checks
stderr_checks
for candidate in gawk mawk "busybox awk"; do
	command -v "${candidate%% *}" >/dev/null 2>&1 || continue
	echo "== awk: $candidate"
	printf '#!/bin/sh\nexec %s "$@"\n' "$candidate" >"$scratch/awk"
	chmod +x "$scratch/awk"
	RENDER_LIB_AWK="$scratch/awk"
	export RENDER_LIB_AWK
	checks
	stderr_checks
done

if [[ "$fail" -ne 0 ]]; then
	echo "FAILED"
	exit 1
fi
echo "PASSED"
