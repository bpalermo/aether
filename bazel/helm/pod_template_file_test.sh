#!/usr/bin/env bash
# //bazel/helm:pod_template_file_test — helm_pod_template_file_test fails for
# each thing it exists to catch, and says which.
#
# The rule is a test rule, so "does it fail" can only be asked of a run that
# fails. //bazel/helm/testdata/filechart holds such runners (the rule as a
# plain executable: a target that fails by design cannot be a test) beside
# :file_test, the same chart and file passing. This runs each runner, requires
# it to fail, and reads what it said.
#
# Usage: pod_template_file_test.sh <directory of the filechart runners>
set -uo pipefail

dir="$1"
scratch="${TEST_TMPDIR:?}/pod_template_file"
mkdir -p "$scratch"
fail=0

ok() { printf '  ok    %s\n' "$1"; }
bad() {
	printf '  FAIL  %s\n' "$1"
	fail=1
}

# fails <target> <literal>...: the runner exits non-zero and its output holds
# each text.
fails() {
	local target="$1" out="$scratch/$1.out" status want
	shift
	"$dir/$target.sh" >"$out" 2>&1
	status=$?
	if [[ "$status" -ne 0 ]]; then
		ok "$target fails"
	else
		bad "$target: exit 0, want a failure"
	fi
	for want in "$@"; do
		if grep -qF -- "$want" "$out"; then
			ok "$target says: $want"
		else
			bad "$target does not say: $want"
			head -n 30 "$out"
		fi
	done
}

fails failing_pod_template \
	"FAIL: a pod template changes with other content in files/stamp, so a change of that file alone rolls the workload's pods:" \
	"FAIL: a pod template changes without files/stamp, so a change of that file alone rolls the workload's pods:" \
	"  DaemonSet/release-filechart" \
	"PASS: 2 rendered lines change with the content of files/stamp."
fails failing_changed_lines \
	"FAIL: 2 rendered lines change with the content of files/stamp (and 2 in the other direction), want 1 (the lines are not shown)." \
	"PASS: the 1 pod templates are byte-identical with other content in files/stamp."
fails failing_not_optional \
	"FAIL: the render without files/stamp holds 1 lines the packaged chart's render does not: the file is not optional, it replaces something (the lines are not shown)." \
	"PASS: the 1 pod templates are byte-identical without files/stamp."
fails failing_unread_file \
	"FAIL: the render is the same with other content in files/unread: no template reads it, so this test compares a chart with itself"
fails failing_missing_file \
	"FAIL: the packaged chart holds no files/absent"
fails failing_workloads \
	"FAIL: expected 2 workloads; render a has 1 workload objects and 1 pod templates were extracted."

# The whole-render comparisons count lines and print none: the substituted
# content is in no output but a pod template diff.
for target in failing_changed_lines failing_not_optional failing_unread_file failing_missing_file failing_workloads; do
	if grep -qF -- 424242 "$scratch/$target.out"; then
		bad "$target prints the content of the file"
	else
		ok "$target prints no rendered line"
	fi
done

exit "$fail"
