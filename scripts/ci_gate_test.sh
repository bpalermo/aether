#!/usr/bin/env bash
# Hermetic test of scripts/ci-gate.sh, the decision of the `ci` job of
# .github/workflows/ci.yaml (#1460), of the `proxy` job of
# .github/workflows/proxy.yml (#1489) and of the `main` job of
# .github/workflows/main.yaml (#1501). No network, no GitHub: each case is a
# `needs` context written here, as GitHub renders it with toJSON(needs).
#
# Two halves, first for ci.yaml (sections 1 to 6), then the same two for
# proxy.yml (7 and 8), then how main.yaml's `test` job gets the impacted lists
# (9, #1488), then the same two for main.yaml (10 and 11):
#
#   1. the decision. A run passes when every job ended the way the workflow's
#      conditions say it must, and "nothing to do" is something a job wrote:
#      control_plane=false from `changes`, or four times `false` from `diff`.
#      It fails when an output was never set (the empty string skips a leg
#      exactly as `false` does), when a job that had to run was skipped, failed
#      or was cancelled, and when the context is missing a job or is not JSON.
#   2. the rules against the workflow file. The script repeats the `if:` of
#      every job, so the job list, the `needs` of the `ci` job and each `if:`
#      are compared with ci.yaml here: a job added to the workflow and not to
#      the gate, or a condition changed in one place only, fails this test.
#      It also holds what #1459 needs from the file: no job checks out another
#      ref than the run's own, `diff` takes its range from that checkout, and a
#      job reads the impacted lists only after .github/actions/impacted-lists
#      has checked them against its checkout.
#
# Run: bazel test //scripts:ci_gate_test (jq is the Bazel-pinned one), or
#      bash scripts/ci_gate_test.sh with jq on PATH.
# shellcheck disable=SC2016 # single-quoted $names here are jq variables or
# workflow expressions, never shell expansions.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/ci-gate.sh"
WORKFLOW="${TEST_SRCDIR:-}/${TEST_WORKSPACE:-_main}/.github/workflows/ci.yaml"
[ -f "$WORKFLOW" ] || WORKFLOW="$HERE/../.github/workflows/ci.yaml"
ACTION="$(dirname -- "$(dirname -- "$WORKFLOW")")/actions/impacted-lists/action.yml"
CI_WORKFLOW="$WORKFLOW"
PROXY_WORKFLOW="$(dirname -- "$WORKFLOW")/proxy.yml"
MAIN_WORKFLOW="$(dirname -- "$WORKFLOW")/main.yaml"
for f in "$SCRIPT" "$WORKFLOW" "$PROXY_WORKFLOW" "$MAIN_WORKFLOW"; do
	[ -f "$f" ] || {
		echo "FAIL: $f not found"
		exit 1
	}
done

# Under Bazel, JQ_RLOCATIONPATH names the pinned jq in the runfiles.
if [ -n "${JQ_RLOCATIONPATH:-}" ]; then
	JQ="${TEST_SRCDIR:-${RUNFILES_DIR:-$PWD/..}}/${JQ_RLOCATIONPATH}"
fi
JQ="${JQ:-$(command -v jq)}"
[ -x "$JQ" ] || {
	echo "FAIL: no jq (JQ=${JQ})"
	exit 1
}
export JQ

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FAILS=0
pass() { echo "PASS  $*"; }
fail() {
	echo "FAIL  $*"
	FAILS=$((FAILS + 1))
}

# --- the contexts ---------------------------------------------------------------
# FULL: a pull request that impacts everything, every job green. The other
# cases are this one with a jq edit, so each names only what differs.
FULL='{
  "changes":            {"result": "success", "outputs": {"control_plane": "true"}},
  "diff":               {"result": "success", "outputs": {"has_any": "true", "has_unit": "true", "has_integration": "true", "has_e2e": "true"}},
  "chart-version-bump": {"result": "success", "outputs": {}},
  "deps-audit":         {"result": "success", "outputs": {}},
  "envoy-api-parity":   {"result": "success", "outputs": {}},
  "proxy-pin":          {"result": "success", "outputs": {}},
  "actionlint":         {"result": "success", "outputs": {}},
  "shell":              {"result": "success", "outputs": {}},
  "format":             {"result": "success", "outputs": {}},
  "test":               {"result": "success", "outputs": {}},
  "race":               {"result": "success", "outputs": {}},
  "netns":              {"result": "success", "outputs": {}},
  "integration":        {"result": "success", "outputs": {}},
  "e2e":                {"result": "success", "outputs": {}}
}'
# jq helpers the edits below use: skip(jobs) and has(any; unit; integration; e2e).
DEFS='
def skip($jobs): reduce ($jobs | split(" "))[] as $j (.; .[$j] = {result: "skipped", outputs: {}});
def has($a; $u; $i; $e): .diff.outputs = {has_any: $a, has_unit: $u, has_integration: $i, has_e2e: $e};
'
LEGS='test race netns integration e2e'
CP_JOBS='diff chart-version-bump deps-audit shell format'

RC=0
# The context the edits start from, and the rules the gate is asked for (none:
# ci.yaml's). Section 7 sets both for proxy.yml.
CTX="$FULL"
GATE=()
# decide <jq edit of CTX>: runs the gate on the edited context; status in RC,
# output in $TMP/log.
decide() {
	local ctx
	ctx="$("$JQ" -c "$DEFS $1" <<<"$CTX")" || {
		echo "FAIL: the test's own jq edit does not compile: $1"
		exit 1
	}
	NEEDS_JSON="$ctx" bash "$SCRIPT" "${GATE[@]}" >"$TMP/log" 2>&1
	RC=$?
}
# green <what> <edit> <a line the log must have>
green() {
	decide "$2"
	if [ "$RC" -eq 0 ] && grep -qF -- "$3" "$TMP/log"; then
		pass "$1"
	else
		fail "$1: expected a pass saying '$3', got exit $RC"
		sed 's/^/    /' "$TMP/log"
	fi
}
# red <what> <edit> <a piece of the ::error:: line the log must have>
red() {
	decide "$2"
	if [ "$RC" -ne 0 ] && grep -F -- '::error::' "$TMP/log" | grep -qF -- "$3"; then
		pass "$1"
	else
		fail "$1: expected a failure saying '$3', got exit $RC"
		sed 's/^/    /' "$TMP/log"
	fi
}

# --- 1. runs that pass ------------------------------------------------------------
green "everything impacted, everything green" '.' \
	'decision: impacted; had to run: test race netns integration e2e'
green "proxy-only change: control_plane=false, everything else skipped" \
	".changes.outputs.control_plane = \"false\" | skip(\"$CP_JOBS $LEGS\")" \
	'decision: the control plane is untouched (changes wrote control_plane=false)'
green "nothing impacted: diff wrote false four times, the legs are skipped" \
	"has(\"false\"; \"false\"; \"false\"; \"false\") | skip(\"$LEGS\")" \
	'decision: nothing impacted (diff wrote false to has_any, has_unit, has_integration and has_e2e)'
green "only unit tests impacted" \
	'has("true"; "true"; "false"; "false") | skip("integration e2e")' \
	'decision: impacted; had to run: test race netns'
green "only an integration test impacted" \
	'has("true"; "false"; "true"; "false") | skip("netns e2e")' \
	'decision: impacted; had to run: test race integration'
green "only the e2e target impacted" \
	'has("true"; "false"; "false"; "true") | skip("race netns integration")' \
	'decision: impacted; had to run: test e2e'
green "a library with no test impacted: only the build leg" \
	'has("true"; "false"; "false"; "false") | skip("race netns integration e2e")' \
	'decision: impacted; had to run: test'

# --- 2. an output that was never set (#1460) ---------------------------------------
# What a `diff` that wrote nothing looks like from the `ci` job: it succeeded,
# its outputs are empty, and every leg skipped on `'' == 'true'`.
red "diff succeeded and set no output; the legs skipped" \
	".diff.outputs = {} | skip(\"$LEGS\")" 'its output has_any is not set'
red "diff succeeded and has_any is the empty string" \
	".diff.outputs.has_any = \"\" | skip(\"$LEGS\")" 'its output has_any is ""'
for out in has_any has_unit has_integration has_e2e; do
	red "diff succeeded and $out is missing, the others say false" \
		"has(\"false\"; \"false\"; \"false\"; \"false\") | del(.diff.outputs.$out) | skip(\"$LEGS\")" \
		"its output $out is not set"
done
for value in TRUE True 1 yes ' true' null; do
	red "has_any is '$value', which is not true or false" \
		".diff.outputs.has_any = \"$value\"" 'its output has_any is'
done
red "changes succeeded and set no control_plane; everything else skipped" \
	".changes.outputs = {} | skip(\"$CP_JOBS $LEGS\")" 'its output control_plane is not set'
red "control_plane is the empty string" \
	".changes.outputs.control_plane = \"\" | skip(\"$CP_JOBS $LEGS\")" 'its output control_plane is ""'

# --- 3. a job that had to run did not ------------------------------------------------
red "diff skipped though control_plane=true" \
	".diff = {result: \"skipped\", outputs: {}} | skip(\"$LEGS\")" 'job diff was skipped, and changes says control_plane=true'
for job in chart-version-bump deps-audit shell format; do
	red "$job skipped though control_plane=true" "skip(\"$job\")" "job $job was skipped, and changes says control_plane=true"
done
for job in changes envoy-api-parity proxy-pin actionlint; do
	red "$job skipped though it has no condition" "skip(\"$job\")" "job $job was skipped, and it has no condition"
done
for leg in $LEGS; do
	red "$leg skipped though diff says it had to run" "skip(\"$leg\")" "job $leg was skipped, and diff says it had to run"
done
red "race skipped with only an integration test impacted" \
	'has("true"; "false"; "true"; "false") | skip("race netns e2e")' 'job race was skipped, and diff says it had to run'
red "has_any=false and has_unit=true: the outputs contradict each other" \
	"has(\"false\"; \"true\"; \"false\"; \"false\") | skip(\"$LEGS\")" 'a test cannot be impacted when no target is'

# --- 4. failed and cancelled ---------------------------------------------------------
for job in changes diff shell actionlint test race netns integration e2e; do
	for result in failure cancelled; do
		red "$job ended as $result" ".[\"$job\"].result = \"$result\"" "job $job ended as $result"
	done
done
# The shape of a real red run: `test` failed, so the legs that need it skipped.
red "test failed and the legs that need it skipped" \
	'.test.result = "failure" | skip("race integration e2e")' 'job test ended as failure'
red "diff was cancelled before it set an output; the legs skipped" \
	".diff = {result: \"cancelled\", outputs: {}} | skip(\"$LEGS\")" 'job diff ended as cancelled'
red "diff failed before it set an output; the legs skipped" \
	".diff = {result: \"failure\", outputs: {}} | skip(\"$LEGS\")" 'job diff ended as failure'

# --- 5. a context the rules do not cover ----------------------------------------------
red "a job is missing from needs" 'del(.format)' 'job format is not in the needs of the ci job'
red "needs has a job with no rule" '.["new-leg"] = {result: "success", outputs: {}}' 'job new-leg is in the needs of the ci job and scripts/ci-gate.sh has no rule for it'
red "a result that is not one of the four" '.shell.result = "neutral"' 'job shell has the result "neutral"'
red "a job with no result" 'del(.shell.result)' 'job shell has the result not set'
red "a job ran though control_plane=false" \
	".changes.outputs.control_plane = \"false\" | skip(\"$CP_JOBS $LEGS\") | .shell.result = \"success\"" \
	'job shell ran, and changes says control_plane=false'
red "a leg ran though diff says it had nothing to run" \
	'has("true"; "true"; "false"; "false") | skip("e2e")' 'job integration ran, and diff says it had nothing to run'
red "the context is an array" '[.]' 'the needs context is not a JSON object'

raw() { # what, NEEDS_JSON value (or unset), piece of the error
	if [ "$2" = unset ]; then
		env -u NEEDS_JSON bash "$SCRIPT" "${GATE[@]}" >"$TMP/log" 2>&1
	else
		NEEDS_JSON="$2" bash "$SCRIPT" "${GATE[@]}" >"$TMP/log" 2>&1
	fi
	RC=$?
	if [ "$RC" -ne 0 ] && grep -F -- '::error::' "$TMP/log" | grep -qF -- "$3"; then
		pass "$1"
	else
		fail "$1: expected a failure saying '$3', got exit $RC"
		sed 's/^/    /' "$TMP/log"
	fi
}
raw "NEEDS_JSON is not set" unset 'NEEDS_JSON is empty'
raw "NEEDS_JSON is empty" '' 'NEEDS_JSON is empty'
raw "NEEDS_JSON is not JSON" '{"changes": ' 'could not be read as the needs context'
raw "NEEDS_JSON is the empty object" '{}' 'job changes is not in the needs of the ci job'

# --- 6. the rules against the workflow file --------------------------------------------
# job <name>: the lines of that job in $WORKFLOW (from its key to the next one).
# Always read into a variable first: under pipefail, `job x | grep -q` fails
# when grep matches early and awk dies of SIGPIPE.
job() {
	awk -v want="$1" '
		/^jobs:/ { in_jobs = 1; next }
		!in_jobs { next }
		/^  [A-Za-z0-9_-]+:[[:space:]]*$/ { name = $1; sub(/:$/, "", name) }
		name == want { print }
	' "$WORKFLOW"
}
expect() { # what, got, want
	if [ "$2" = "$3" ]; then
		pass "$1"
	else
		fail "$1"
		echo "    got:  $2"
		echo "    want: $3"
	fi
}
sorted() { tr -s ' ,' '\n' | grep -v '^$' | sort | tr '\n' ' ' | sed 's/ $//'; }

workflow_jobs="$(awk '
	/^jobs:/ { in_jobs = 1; next }
	in_jobs && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { sub(/:$/, "", $1); print $1 }
' "$WORKFLOW" | grep -vx ci | sorted)"
gate_jobs="$(bash "$SCRIPT" --jobs | sorted)"
ci_needs="$(job ci | sed -n 's/^    needs: \[\(.*\)\]$/\1/p' | sorted)"
[ -n "$workflow_jobs" ] || fail "no job found in $WORKFLOW (did its layout change?)"
expect "every job of ci.yaml but ci has a rule in the gate, and the gate has no other" "$gate_jobs" "$workflow_jobs"
expect "the ci job needs every other job of ci.yaml" "$ci_needs" "$workflow_jobs"
expect "the ci job runs whatever the others did" "$(job ci | grep -c '^    if: always()$')" 1
ci_job="$(job ci)"
if grep -qF 'NEEDS_JSON: ${{ toJSON(needs) }}' <<<"$ci_job" && grep -qE '^[[:space:]]+run: scripts/ci-gate\.sh$' <<<"$ci_job"; then
	pass "the ci job hands toJSON(needs) to scripts/ci-gate.sh"
else
	fail "the ci job does not run scripts/ci-gate.sh with NEEDS_JSON: \${{ toJSON(needs) }}"
fi

# The `if:` of each job, as the rules in ci-gate.sh assume it.
CP="needs.changes.outputs.control_plane == 'true'"
condition() { # job, the `if:` it must have (empty: none)
	expect "ci.yaml: the condition of job $1 is the one the gate assumes" \
		"$(job "$1" | sed -n 's/^    if: //p')" "$2"
}
for j in changes envoy-api-parity proxy-pin actionlint; do condition "$j" ""; done
for j in diff chart-version-bump deps-audit shell format; do condition "$j" "$CP"; done
condition test "$CP && needs.diff.outputs.has_any == 'true'"
condition race "$CP && (needs.diff.outputs.has_unit == 'true' || needs.diff.outputs.has_integration == 'true')"
condition netns "$CP && needs.diff.outputs.has_unit == 'true'"
condition integration "$CP && needs.diff.outputs.has_integration == 'true'"
condition e2e "$CP && needs.diff.outputs.has_e2e == 'true'"
# `diff` hands on exactly the four outputs the gate reads.
expect "ci.yaml: the outputs of job diff are the four the gate reads" \
	"$(job diff | sed -n 's/^      \(has_[a-z0-9_]*\): \${{ steps\.impacted\.outputs\.\(has_[a-z0-9_]*\) }}$/\1=\2/p' | sorted)" \
	"has_any=has_any has_e2e=has_e2e has_integration=has_integration has_unit=has_unit"

# #1459: one tree. No job asks actions/checkout for another ref than the run's
# own (on pull_request: the merge of the head into the base), `diff` computes
# its range from that checkout, and a job that downloads the impacted lists
# checks them against its own checkout before it uses them.
expect "ci.yaml: no checkout names a ref of its own" "$(grep -cE '^[[:space:]]+ref:' "$WORKFLOW")" 0
diff_job="$(job diff)"
if grep -qE '^[[:space:]]+run: scripts/ci-merge-range\.sh$' <<<"$diff_job" &&
	grep -qF 'BASE_SHA: ${{ steps.range.outputs.base }}' <<<"$diff_job" &&
	grep -qF 'HEAD_SHA: ${{ steps.range.outputs.head }}' <<<"$diff_job"; then
	pass "ci.yaml: diff takes its range from scripts/ci-merge-range.sh"
else
	fail "ci.yaml: diff does not take BASE_SHA and HEAD_SHA from scripts/ci-merge-range.sh"
fi
# The lists reach a job through .github/actions/impacted-lists only, which
# downloads the artifact and then runs the check. A job that reads a list
# (a path under .../impacted/, or the race job's $IMPACTED) uses the action,
# with the three outputs, before the first step that reads one; no job
# downloads the artifact itself.
if [ -f "$ACTION" ] && awk '
	/uses: actions\/download-artifact@/ { downloaded = 1 }
	downloaded && /^[[:space:]]+run: scripts\/ci-impacted-lists\.sh "\$RUNNER_TEMP\/impacted"$/ { ok = 1 }
	END { exit ok ? 0 : 1 }' "$ACTION" &&
	grep -qF 'path: ${{ runner.temp }}/impacted' "$ACTION" &&
	grep -qE '^[[:space:]]+name: \$\{\{ inputs\.artifact-name \}\}$' "$ACTION" &&
	grep -qF 'HAS_ANY: ${{ inputs.has-any }}' "$ACTION" &&
	grep -qF 'HAS_UNIT: ${{ inputs.has-unit }}' "$ACTION" &&
	grep -qF 'HAS_INTEGRATION: ${{ inputs.has-integration }}' "$ACTION"; then
	pass "the impacted-lists action downloads the artifact it is told to, then runs scripts/ci-impacted-lists.sh on it with the three outputs"
else
	fail "$ACTION does not download the artifact named by its artifact-name input and then run scripts/ci-impacted-lists.sh \"\$RUNNER_TEMP/impacted\" with HAS_ANY, HAS_UNIT and HAS_INTEGRATION"
fi
# The name ci.yaml's jobs get without saying one is the name its `diff` uploads.
expect "the action's artifact-name defaults to the artifact ci.yaml's diff uploads" \
	"$(awk '
		/^  artifact-name:[[:space:]]*$/ { mine = 1; next }
		/^  [A-Za-z0-9_-]+:[[:space:]]*$/ { mine = 0 }
		mine && /^    default: / { print $2 }' "$ACTION")" \
	"$(job diff | awk '/uses: actions\/upload-artifact@/ { up = 1 } up && /^          name: / { print $2; exit }')"
expect "ci.yaml: no job names an artifact for the action (the default is its own)" \
	"$(grep -c 'artifact-name:' "$WORKFLOW")" 0
expect "ci.yaml: no job downloads the impacted-targets artifact itself" \
	"$(grep -cE '^[[:space:]]+name: impacted-targets$' "$WORKFLOW") $(grep -c 'uses: actions/download-artifact@' "$WORKFLOW")" "1 0"
readers=""
for j in $workflow_jobs; do
	body="$(job "$j")"
	grep -qE '(runner\.temp \}\}|RUNNER_TEMP)/impacted(/|"|$)' <<<"$body" || continue
	readers="$readers $j"
	if awk '
		/^      - / { step++ }
		/uses: \.\/\.github\/actions\/impacted-lists$/ && !action { action = step }
		/(runner\.temp \}\}|RUNNER_TEMP)\/impacted(\/|"|$)/ && !reader { reader = step }
		/has-any: \$\{\{ needs\.diff\.outputs\.has_any \}\}$/ { any = 1 }
		/has-unit: \$\{\{ needs\.diff\.outputs\.has_unit \}\}$/ { unit = 1 }
		/has-integration: \$\{\{ needs\.diff\.outputs\.has_integration \}\}$/ { integration = 1 }
		END { exit (action && reader && action < reader && any && unit && integration) ? 0 : 1 }' <<<"$body"; then
		pass "ci.yaml: job $j gets the impacted lists through the checked action before it reads one"
	else
		fail "ci.yaml: job $j reads an impacted list and does not use ./.github/actions/impacted-lists (with has-any, has-unit, has-integration) in an earlier step"
	fi
done
expect "ci.yaml: the jobs that read the impacted lists" "$(sorted <<<"$readers")" "integration netns race test"

# --- 7. proxy.yml: the decision (#1489) ---------------------------------------------------
# The `proxy` job is the other required check with `if: always()`. Its legs
# skip on `needs.changes.outputs.proxy == 'true'`, so an output that was never
# set skips both exactly as `false` does: same rules, another table.
CTX='{
  "changes": {"result": "success", "outputs": {"proxy": "true"}},
  "shell":   {"result": "success", "outputs": {}},
  "test":    {"result": "success", "outputs": {}}
}'
GATE=(proxy)
green "proxy: the workspace changed, shell and test green" '.' \
	'decision: the proxy workspace changed (changes wrote proxy=true); had to run: shell test'
# proxy.yml has no `diff`: its log says nothing about one.
if grep -q 'diff' "$TMP/log"; then
	fail "proxy: the log of a run of proxy.yml speaks of a diff job"
	sed 's/^/    /' "$TMP/log"
else
	pass "proxy: the log of a run of proxy.yml does not speak of a diff job"
fi
green "proxy: control-plane-only change, proxy=false, shell and test skipped" \
	'.changes.outputs.proxy = "false" | skip("shell test")' \
	'decision: the proxy workspace is untouched (changes wrote proxy=false)'
red "proxy: changes succeeded and set no output; shell and test skipped" \
	'.changes.outputs = {} | skip("shell test")' 'its output proxy is not set'
red "proxy: the output is the empty string; shell and test skipped" \
	'.changes.outputs.proxy = "" | skip("shell test")' 'its output proxy is ""'
for value in TRUE True 1 yes ' true' null; do
	red "proxy: the output is '$value', which is not true or false" \
		".changes.outputs.proxy = \"$value\" | skip(\"shell test\")" 'its output proxy is'
done
red "proxy: changes set another output than proxy" \
	'.changes.outputs = {control_plane: "false"} | skip("shell test")' 'its output proxy is not set'
for j in shell test; do
	red "proxy: $j skipped though proxy=true" "skip(\"$j\")" "job $j was skipped, and changes says proxy=true"
	red "proxy: $j ran though proxy=false" \
		".changes.outputs.proxy = \"false\" | skip(\"shell test\") | .[\"$j\"].result = \"success\"" \
		"job $j ran, and changes says proxy=false"
done
red "proxy: changes skipped though it has no condition" \
	'skip("changes shell test")' 'job changes was skipped, and it has no condition'
for j in changes shell test; do
	for result in failure cancelled; do
		red "proxy: $j ended as $result" ".[\"$j\"].result = \"$result\"" "job $j ended as $result"
	done
done
red "proxy: changes failed before it set the output; shell and test skipped" \
	'.changes = {result: "failure", outputs: {}} | skip("shell test")' 'job changes ended as failure'
red "proxy: a job is missing from needs" 'del(.shell)' 'job shell is not in the needs of the proxy job'
red "proxy: needs has a job with no rule" '.["new-leg"] = {result: "success", outputs: {}}' \
	'job new-leg is in the needs of the proxy job and scripts/ci-gate.sh has no rule for it'
red "proxy: the context of ci.yaml is not one of proxy.yml" "$FULL" 'has no rule for it'
raw "proxy: NEEDS_JSON is empty" '' 'NEEDS_JSON is empty'
raw "proxy: NEEDS_JSON is the empty object" '{}' 'job changes is not in the needs of the proxy job'
# The rules are asked for by name, and a name the script does not know is not
# ci.yaml's rules by default.
GATE=(release)
raw "a gate the script has no rules for" "$CTX" 'no rules for a workflow named'
GATE=()
red "the context of proxy.yml is not one of ci.yaml" "$CTX" 'is not in the needs of the ci job'
CTX="$FULL"

# --- 8. proxy.yml: the rules against the workflow file ---------------------------------------
WORKFLOW="$PROXY_WORKFLOW"
workflow_jobs="$(awk '
	/^jobs:/ { in_jobs = 1; next }
	in_jobs && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { sub(/:$/, "", $1); print $1 }
' "$WORKFLOW" | grep -vx proxy | sorted)"
gate_jobs="$(bash "$SCRIPT" proxy --jobs | sorted)"
proxy_needs="$(job proxy | sed -n 's/^    needs: \[\(.*\)\]$/\1/p' | sorted)"
[ -n "$workflow_jobs" ] || fail "no job found in $WORKFLOW (did its layout change?)"
expect "every job of proxy.yml but proxy has a rule in the gate, and the gate has no other" "$gate_jobs" "$workflow_jobs"
expect "the proxy job needs every other job of proxy.yml" "$proxy_needs" "$workflow_jobs"
expect "the proxy job runs whatever the others did" "$(job proxy | grep -c '^    if: always()$')" 1
proxy_job="$(job proxy)"
if grep -qF 'NEEDS_JSON: ${{ toJSON(needs) }}' <<<"$proxy_job" && grep -qE '^[[:space:]]+run: scripts/ci-gate\.sh proxy$' <<<"$proxy_job"; then
	pass "the proxy job hands toJSON(needs) to scripts/ci-gate.sh proxy"
else
	fail "the proxy job does not run scripts/ci-gate.sh proxy with NEEDS_JSON: \${{ toJSON(needs) }}"
fi
# Nothing else decides: a step with a condition of its own could pass or be
# skipped whatever the gate said.
expect "the proxy job has no step with a condition of its own" "$(grep -c '^        if: ' <<<"$proxy_job")" 0
condition() { # job, the `if:` it must have (empty: none)
	expect "proxy.yml: the condition of job $1 is the one the gate assumes" \
		"$(job "$1" | sed -n 's/^    if: //p')" "$2"
}
condition changes ""
for j in shell test; do condition "$j" "needs.changes.outputs.proxy == 'true'"; done
expect "proxy.yml: the output of job changes is the one the gate reads" \
	"$(job changes | sed -n 's/^      \([a-z0-9_]*\): \${{ steps\.filter\.outputs\.\([a-z0-9_]*\) }}$/\1=\2/p' | sorted)" \
	"proxy=proxy"
# The gate is a script of the checkout: the job must check out the run's own
# commit, no other ref.
if grep -q 'uses: actions/checkout@' <<<"$proxy_job"; then
	pass "the proxy job checks the repository out before it runs the gate"
else
	fail "the proxy job runs scripts/ci-gate.sh without a checkout"
fi
expect "proxy.yml: no checkout names a ref of its own" "$(grep -cE '^[[:space:]]+ref:' "$WORKFLOW")" 0

# --- 9. main.yaml: the impacted lists (#1488) --------------------------------------------------
# main.yaml's `diff` uploads its lists under another name than ci.yaml's, and
# its `test` job reads them: through the same checked action, told that name.
WORKFLOW="$MAIN_WORKFLOW"
expect "main.yaml: no job downloads an artifact itself" "$(grep -c 'uses: actions/download-artifact@' "$WORKFLOW")" 0
uploaded="$(job diff | awk '/uses: actions\/upload-artifact@/ { up = 1 } up && /^          name: / { print $2; exit }')"
[ -n "$uploaded" ] || fail "main.yaml: job diff uploads no named artifact (did its layout change?)"
expect "main.yaml: the outputs of job diff are the three the action checks" \
	"$(job diff | sed -n 's/^      \(has_[a-z0-9_]*\): \${{ steps\.impacted\.outputs\.\(has_[a-z0-9_]*\) }}$/\1=\2/p' | sorted)" \
	"has_any=has_any has_integration=has_integration has_unit=has_unit"
# One tree here too: the lists are computed for github.sha, and every checkout
# of the workflow (diff, test and the main job) is of github.sha.
expect "main.yaml: the lists are computed for the commit the test job checks out" \
	"$(job diff | grep -cF 'HEAD_SHA: ${{ github.sha }}') $(grep -cE '^[[:space:]]+ref: \$\{\{ github\.sha \}\}$' "$WORKFLOW") $(grep -cE '^[[:space:]]+ref:' "$WORKFLOW")" \
	"1 3 3"
readers=""
workflow_jobs="$(awk '
	/^jobs:/ { in_jobs = 1; next }
	in_jobs && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { sub(/:$/, "", $1); print $1 }
' "$WORKFLOW" | sorted)"
[ -n "$workflow_jobs" ] || fail "no job found in $WORKFLOW (did its layout change?)"
for j in $workflow_jobs; do
	body="$(job "$j")"
	grep -qE '(runner\.temp \}\}|RUNNER_TEMP)/impacted(/|"|$)' <<<"$body" || continue
	readers="$readers $j"
	if awk -v name="$uploaded" '
		/^      - / { step++ }
		/uses: \.\/\.github\/actions\/impacted-lists$/ && !action { action = step }
		/(runner\.temp \}\}|RUNNER_TEMP)\/impacted(\/|"|$)/ && !reader { reader = step }
		$1 == "artifact-name:" && $2 == name { named = 1 }
		/has-any: \$\{\{ needs\.diff\.outputs\.has_any \}\}$/ { any = 1 }
		/has-unit: \$\{\{ needs\.diff\.outputs\.has_unit \}\}$/ { unit = 1 }
		/has-integration: \$\{\{ needs\.diff\.outputs\.has_integration \}\}$/ { integration = 1 }
		END { exit (action && reader && action < reader && named && any && unit && integration) ? 0 : 1 }' <<<"$body"; then
		pass "main.yaml: job $j gets the impacted lists through the checked action, by the name diff uploads, before it reads one"
	else
		fail "main.yaml: job $j reads an impacted list and does not use ./.github/actions/impacted-lists (artifact-name: $uploaded, has-any, has-unit, has-integration) in an earlier step"
	fi
done
expect "main.yaml: the jobs that read the impacted lists" "$(sorted <<<"$readers")" "test"

# --- 10. main.yaml: the decision (#1501) -----------------------------------------------------
# main.yaml runs on a push to main and has no `changes` job: `diff` always
# runs, and `test` skips on `needs.diff.outputs.has_any == 'true'`. A `diff`
# that wrote nothing skips it exactly as `false` does, and without the `main`
# job the run is green having tested nothing. Same rules, a third table.
MAIN_FULL='{
  "diff": {"result": "success", "outputs": {"has_any": "true", "has_unit": "true", "has_integration": "true"}},
  "test": {"result": "success", "outputs": {}}
}'
CTX="$MAIN_FULL"
GATE=(main)
DEFS="$DEFS"'
def has3($a; $u; $i): .diff.outputs = {has_any: $a, has_unit: $u, has_integration: $i};
'
green "main: everything impacted, diff and test green" '.' 'decision: impacted; had to run: test'
# main.yaml has no `changes` job and no has_e2e output: its log names neither.
if grep -qE 'changes|has_e2e|pull request' "$TMP/log"; then
	fail "main: the log of a run of main.yaml speaks of a changes job, of has_e2e or of a pull request"
	sed 's/^/    /' "$TMP/log"
else
	pass "main: the log of a run of main.yaml speaks of no changes job, no has_e2e and no pull request"
fi
green "main: nothing impacted: diff wrote false three times, test is skipped" \
	'has3("false"; "false"; "false") | skip("test")' \
	'decision: nothing impacted (diff wrote false to has_any, has_unit and has_integration)'
green "main: a library with no test impacted: test ran its build" \
	'has3("true"; "false"; "false")' 'decision: impacted; had to run: test'
green "main: only an integration test impacted: test ran its build" \
	'has3("true"; "false"; "true")' 'decision: impacted; had to run: test'
# The run of #1501: `diff` green, no output, `test` skipped on '' == 'true'.
red "main: diff succeeded and set no output; test skipped" \
	'.diff.outputs = {} | skip("test")' 'its output has_any is not set'
# An output that is not set is not read as `false` further down: the log of
# that run must not also say that nothing was impacted.
if grep -qxF 'decision: none, see the errors' "$TMP/log" && ! grep -q 'nothing impacted' "$TMP/log"; then
	pass "main: a diff that set no output is not also reported as nothing impacted"
else
	fail "main: a diff that set no output must log 'decision: none, see the errors' and no 'nothing impacted'"
	sed 's/^/    /' "$TMP/log"
fi
red "main: diff succeeded and has_any is the empty string; test skipped" \
	'.diff.outputs.has_any = "" | skip("test")' 'its output has_any is ""'
for out in has_any has_unit has_integration; do
	red "main: $out is missing, the others say false; test skipped" \
		"has3(\"false\"; \"false\"; \"false\") | del(.diff.outputs.$out) | skip(\"test\")" \
		"its output $out is not set"
done
# `test` skips its unit-test step on has_unit: unset is not "no unit test".
red "main: has_unit is missing and test ran" 'del(.diff.outputs.has_unit)' 'its output has_unit is not set'
for value in TRUE True 1 yes ' true' null; do
	red "main: has_any is '$value', which is not true or false" \
		".diff.outputs.has_any = \"$value\"" 'its output has_any is'
done
red "main: test skipped though diff says it had to run" 'skip("test")' 'job test was skipped, and diff says it had to run'
red "main: test ran though diff says it had nothing to run" \
	'has3("false"; "false"; "false")' 'job test ran, and diff says it had nothing to run'
red "main: has_any=false and has_unit=true: the outputs contradict each other" \
	'has3("false"; "true"; "false") | skip("test")' \
	'job diff says has_any=false has_unit=true has_integration=false: a test cannot be impacted when no target is'
red "main: diff skipped though it has no condition" \
	'skip("diff test")' 'job diff was skipped, and it has no condition: it runs on every push to main'
for j in diff test; do
	for result in failure cancelled; do
		red "main: $j ended as $result" ".[\"$j\"].result = \"$result\"" "job $j ended as $result"
	done
done
red "main: diff failed before it set an output; test skipped" \
	'.diff = {result: "failure", outputs: {}} | skip("test")' 'job diff ended as failure'
red "main: a job is missing from needs" 'del(.test)' 'job test is not in the needs of the main job'
red "main: needs has a job with no rule" '.["refresh-pin-prs"] = {result: "success", outputs: {}}' \
	'job refresh-pin-prs is in the needs of the main job and scripts/ci-gate.sh has no rule for it'
red "main: the context of ci.yaml is not one of main.yaml" "$FULL" 'has no rule for it'
raw "main: NEEDS_JSON is empty" '' 'NEEDS_JSON is empty'
raw "main: NEEDS_JSON is the empty object" '{}' 'job diff is not in the needs of the main job'
GATE=()
red "the context of main.yaml is not one of ci.yaml" "$CTX" 'is not in the needs of the ci job'
CTX="$FULL"

# --- 11. main.yaml: the rules against the workflow file ----------------------------------------
# `refresh-pin-prs` is the one job the `main` job does not need: it has no
# `needs`, runs on `always()` and reads no output of another job, so nothing
# another job did or did not write can skip it. Held here, so that the day it
# takes a condition it is noticed.
INDEPENDENT=refresh-pin-prs
gated_jobs="$(tr ' ' '\n' <<<"$workflow_jobs" | grep -vx -e main -e "$INDEPENDENT" | sorted)"
gate_jobs="$(bash "$SCRIPT" main --jobs | sorted)"
main_needs="$(job main | sed -n 's/^    needs: \[\(.*\)\]$/\1/p' | sorted)"
expect "every job of main.yaml but main and $INDEPENDENT has a rule in the gate, and the gate has no other" "$gate_jobs" "$gated_jobs"
expect "the main job needs every job of main.yaml but $INDEPENDENT" "$main_needs" "$gated_jobs"
expect "the main job runs whatever the others did" "$(job main | grep -c '^    if: always()$')" 1
main_job="$(job main)"
if grep -qF 'NEEDS_JSON: ${{ toJSON(needs) }}' <<<"$main_job" && grep -qE '^[[:space:]]+run: scripts/ci-gate\.sh main$' <<<"$main_job"; then
	pass "the main job hands toJSON(needs) to scripts/ci-gate.sh main"
else
	fail "the main job does not run scripts/ci-gate.sh main with NEEDS_JSON: \${{ toJSON(needs) }}"
fi
expect "the main job has no step with a condition of its own" "$(grep -c '^        if: ' <<<"$main_job")" 0
if grep -q 'uses: actions/checkout@' <<<"$main_job"; then
	pass "the main job checks the repository out before it runs the gate"
else
	fail "the main job runs scripts/ci-gate.sh without a checkout"
fi
# It reads results and outputs: no key, so no environment to wait on.
expect "the main job has no environment" "$(grep -c '^    environment:' <<<"$main_job")" 0
condition() { # job, the `if:` it must have (empty: none)
	expect "main.yaml: the condition of job $1 is the one the gate assumes" \
		"$(job "$1" | sed -n 's/^    if: //p')" "$2"
}
condition diff ""
condition test "needs.diff.outputs.has_any == 'true'"
condition "$INDEPENDENT" "always()"
independent_job="$(job "$INDEPENDENT")"
expect "main.yaml: job $INDEPENDENT needs no job and reads no output of one" \
	"$(grep -cE '^    needs:|needs\.[A-Za-z0-9_-]+\.' <<<"$independent_job")" 0
# Every condition in the file that reads another job, job or step: the `if:`
# of `test`, and the unit-test step inside it. A third one is a decision the
# gate does not know about.
expect "main.yaml: the conditions that read another job are the two the gate covers" \
	"$(grep -E '^[[:space:]]+if: .*needs\.' "$WORKFLOW" | sed 's/^[[:space:]]*//' | sort | tr '\n' '|')" \
	"if: needs.diff.outputs.has_any == 'true'|if: needs.diff.outputs.has_unit == 'true'|"
WORKFLOW="$CI_WORKFLOW"

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS check(s) failed"
	exit 1
fi
echo "all checks passed"
