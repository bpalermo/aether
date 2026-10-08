#!/usr/bin/env bash
# Every rules_helm `helm_template_test` in one BUILD file passes a given
# `helm template` option (#1382).
#
# rules_helm's runner prints the whole render into the test log, whether the
# test passes or fails. A chart whose default render holds generated key
# material (charts/aether: the self-signed webhook certificate) therefore names
# the option that renders none, and this fails a target that leaves it out. The
# chart's BUILD file proves separately that the option does what it is for
# (an absence test for `kind: Secret`).
#
# It reads the BUILD file as text, which is sound because buildifier (a
# required check) keeps it in one shape: a top-level call opens with
# `helm_template_test(` at column 0, closes with `)` at column 0, and holds one
# list element per line. //bazel/helm:upstream_template_test_opts_selftest runs
# it over a file of calls that must and must not satisfy it.
#
# Usage: upstream_template_test_opts_test.sh <BUILD file> <option>
set -euo pipefail

build="$1" opt="$2"

# The option counts only as an element of the target's `opts` list (from the
# `    opts = ` line to the `    ],` that closes it) that directly follows a
# `"--set",` element. The same text in a comment, in another attribute, or after
# another flag proves nothing about what helm is handed.
report="$(awk -v opt="\"$opt\"," '
	function trimmed(s) {
		gsub(/^[ \t]+|[ \t]+$/, "", s)
		return s
	}
	/^helm_template_test\(/ { inside = 1; name = "?"; found = 0; in_opts = 0; next }
	inside && /^    name = / { name = $3; gsub(/[",]/, "", name) }
	inside && /^    opts = / { in_opts = 1; previous = ""; next }
	inside && in_opts && /^    \],/ { in_opts = 0 }
	inside && in_opts {
		item = trimmed($0)
		if (item == opt && previous == "\"--set\",") found = 1
		previous = item
	}
	inside && /^\)/ {
		inside = 0
		total++
		if (!found) print "MISSING " name
	}
	END { print "TOTAL " total + 0 }
' "$build")"

total="$(sed -n 's/^TOTAL //p' <<<"$report")"
missing="$(sed -n 's/^MISSING //p' <<<"$report")"

if [[ "$total" -eq 0 ]]; then
	echo "FAIL: $build has no helm_template_test( call at column 0: either the file changed shape and this check reads nothing, or the last such test is gone and this target should go with it."
	exit 1
fi
if [[ -n "$missing" ]]; then
	echo "FAIL: these helm_template_test targets in $build do not pass $opt:"
	# shellcheck disable=SC2001 # one substitution per line of a list.
	sed 's/^/  /' <<<"$missing"
	echo "rules_helm's runner prints the whole render into the test log. Add"
	echo "  \"--set\", \"$opt\","
	echo "to the target's opts, or, when the test is about what that option changes,"
	echo "write it with a //bazel/helm:defs.bzl rule instead (helm_template_match_test"
	echo "prints a bounded excerpt with Secret data masked)."
	exit 1
fi
echo "PASS: all $total helm_template_test targets in $build pass $opt."
