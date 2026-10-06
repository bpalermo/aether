#!/usr/bin/env bash
# Test of the coverage gate: scripts/coverage-compare.sh (the comparison and
# the verdict) and scripts/coverage-baseline.sh (which run on main a pull
# request is compared with). No Bazel, no network: bash, awk, sort, the
# Bazel-pinned jq, and a fake `gh`.
#
# What it pins for the comparison:
#   - the whole report, byte for byte, against a golden file
#     (testdata/coverage-gate/expected.md) for a pair in which a file and a
#     component disappear, a file and a component appear, and the total drops;
#   - the verdict for: equal reports, a drop within the threshold, a drop of
#     exactly the threshold (passes), a drop beyond it, a rise;
#   - the numbers in the one `::error` / `::notice`, as literals worked out by
#     hand, and that there is exactly one workflow command per run;
#   - COVERAGE_MAX_DROP and COVERAGE_MIN: empty means unset, a value moves the
#     verdict both ways, and a malformed one is refused instead of ignored;
#   - a missing or empty baseline is exit 2 with what to do about it, never a
#     pass, and writes no report;
#   - the tables that do not gate: components and changed files that only one
#     side has, paths in neither report, per-line merging of concatenated
#     tracefiles, and agreement with scripts/coverage-summary.sh on the total.
# For the baseline selection: the base commit's own run, the fallback to
# main's latest (a stacked pull request), which runs can never be a baseline
# (a pull request's, a fork's, another branch's, a failed one, one whose
# artifact expired), waiting for a run that is still in progress, and the
# refusal when nothing is left.
#
# Run: bazel test //scripts:coverage_gate_test, or
#      bash scripts/coverage_gate_test.sh with jq on PATH.
# shellcheck disable=SC2016 # single-quoted backticks are literal Markdown and
# single-quoted $names are jq variables; neither is a shell expansion.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
COMPARE="$HERE/coverage-compare.sh"
BASELINE="$HERE/coverage-baseline.sh"
SUMMARY="$HERE/coverage-summary.sh"
DATA="$HERE/testdata/coverage-gate"
for f in "$COMPARE" "$BASELINE" "$SUMMARY" "$DATA/base.lcov" "$DATA/head.lcov" "$DATA/changed.txt" "$DATA/expected.md"; do
	[ -f "$f" ] || {
		echo "FAIL: $f not found"
		exit 1
	}
done

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
# The thresholds come from the environment in CI; nothing of the caller's may
# leak into a case that does not set them.
unset COVERAGE_MAX_DROP COVERAGE_MIN

FAILS=0
pass() { echo "PASS  $*"; }
fail() {
	echo "FAIL  $*"
	FAILS=$((FAILS + 1))
}
check() {
	local what="$1"
	shift
	if "$@"; then pass "$what"; else fail "$what"; fi
}
has() { grep -qF -- "$1" "$2"; }
lacks() { [ -f "$2" ] && ! grep -qF -- "$1" "$2"; }

# mk <out.lcov> <path>:<lines>:<covered>...: a tracefile in which the first
# <covered> of each file's <lines> lines are hit.
mk() {
	local out="$1" spec path lines covered i
	shift
	: >"$out"
	for spec in "$@"; do
		IFS=: read -r path lines covered <<<"$spec"
		echo "SF:$path" >>"$out"
		for ((i = 1; i <= lines; i++)); do
			echo "DA:$i,$((i <= covered ? 1 : 0))" >>"$out"
		done
		printf 'LH:%d\nLF:%d\nend_of_record\n' "$covered" "$lines" >>"$out"
	done
}

# compare <want exit> <description> [VAR=value...] -- <args...>: run the
# comparison; the report lands in $TMP/out, the workflow command in $TMP/err.
compare() {
	local want="$1" what="$2" rc env=()
	shift 2
	while [ "$1" != "--" ]; do
		env+=("$1")
		shift
	done
	shift
	env "${env[@]}" bash "$COMPARE" "$@" >"$TMP/out" 2>"$TMP/err"
	rc=$?
	if [ "$rc" -ne "$want" ]; then
		fail "$what: exit $rc, wanted $want: $(cat "$TMP/err")"
		return
	fi
	# Exactly one workflow command, of the kind the exit status calls for.
	local commands kind
	commands="$(grep -c '^::' "$TMP/err")"
	kind="$([ "$want" -eq 0 ] && echo '::notice ' || echo '::error ')"
	if [ "$commands" -ne 1 ] || [ "$(wc -l <"$TMP/err")" -ne 1 ] || ! grep -q "^$kind" "$TMP/err"; then
		fail "$what: wanted exactly one '$kind' line on stderr, got: $(cat "$TMP/err")"
		return
	fi
	if [ "$want" -eq 2 ] && [ -s "$TMP/out" ]; then
		fail "$what: could not evaluate the gate but still wrote a report"
		return
	fi
	pass "$what"
}

# --- the golden report -----------------------------------------------------------
# base.lcov: agent/a.go 8 of 10, agent/gone.go 4 of 4, cni/p.go 2 of 4,
# legacy/old.go 1 of 2: 15 of 20 = 75.00%.
# head.lcov: agent/a.go 7 of 10, agent/new.go 0 of 5 (a file no test links),
# cni/p.go 3 of 4, edge/e.go 1 of 1: 11 of 20 = 55.00%. Twenty points down.
compare 1 "the golden pair fails the gate" -- \
	--baseline "$DATA/base.lcov" --head "$DATA/head.lcov" \
	--baseline-label 'main at `0123abc`' --changed-files "$DATA/changed.txt"
if diff -u "$DATA/expected.md" "$TMP/out" >"$TMP/diff"; then
	pass "report equals the golden file"
else
	fail "report differs from the golden file"
	cat "$TMP/diff"
fi
check "golden: the error carries both totals and the delta" \
	has 'line coverage dropped by more than 1.0 points: 55.00% (11 of 20 lines) against 75.00% for the baseline main at `0123abc` (15 of 20 lines): -20.000 points; maximum drop 1.0 points (the default), no minimum' "$TMP/err"
check "golden: totals row" has '| **Delta** | +0 | -4 | **-20.000 points** |' "$TMP/out"
check "golden: a component only the baseline has is 'removed'" \
	has '| `legacy` | 2 | 1 | 50.00% | - | - | - | removed |' "$TMP/out"
check "golden: a component only the pull request has is 'new'" \
	has '| `edge` | - | - | - | 1 | 1 | 100.00% | new |' "$TMP/out"
# agent: 12 of 14 = 85.71% -> 7 of 15 = 46.67%.
check "golden: component delta" has '| `agent` | 14 | 12 | 85.71% | 15 | 7 | 46.67% | -39.05 |' "$TMP/out"
check "golden: a changed file that is new" has '| `agent/new.go` | - | - | - | 5 | 0 | 0.00% | new |' "$TMP/out"
check "golden: a changed file that was deleted" has '| `agent/gone.go` | 4 | 4 | 100.00% | - | - | - | removed |' "$TMP/out"
check "golden: a changed file in both" has '| `agent/a.go` | 10 | 8 | 80.00% | 10 | 7 | 70.00% | -10.00 |' "$TMP/out"
check "golden: an unchanged file is not listed" lacks '| `cni/p.go` |' "$TMP/out"
check "golden: paths in neither report are counted, not listed" \
	has '2 changed path(s) are in neither report' "$TMP/out"
check "golden: a test file is not listed" lacks 'a_test.go' "$TMP/out"

# --- the verdict -----------------------------------------------------------------
# A 200-line baseline at 80.00%: one line is half a point.
mk "$TMP/base.lcov" agent/a.go:150:120 cni/p.go:50:40
mk "$TMP/equal.lcov" agent/a.go:150:120 cni/p.go:50:40
mk "$TMP/small.lcov" agent/a.go:150:119 cni/p.go:50:40  # 79.50%, -0.5
mk "$TMP/exact.lcov" agent/a.go:150:118 cni/p.go:50:40  # 79.00%, -1.0
mk "$TMP/beyond.lcov" agent/a.go:150:117 cni/p.go:50:40 # 78.50%, -1.5
mk "$TMP/rise.lcov" agent/a.go:150:130 cni/p.go:50:40   # 85.00%, +5.0
B=(--baseline "$TMP/base.lcov")

compare 0 "equal reports pass" -- "${B[@]}" --head "$TMP/equal.lcov"
check "equal: the notice says +0.000 points" \
	has '::notice title=Coverage gate passed::line coverage 80.00% (160 of 200 lines) against 80.00% for the baseline main (160 of 200 lines): +0.000 points; maximum drop 1.0 points (the default), no minimum' "$TMP/err"
check "equal: the report says passed" has '## Coverage gate: passed' "$TMP/out"
check "equal: no changed-files section without --changed-files" lacks '### Changed files' "$TMP/out"

compare 0 "a drop within the threshold passes (-0.5)" -- "${B[@]}" --head "$TMP/small.lcov"
check "small drop: the notice carries the delta" has '79.50% (159 of 200 lines)' "$TMP/err"
check "small drop: -0.500 points" has ': -0.500 points;' "$TMP/err"

compare 0 "a drop of exactly the threshold passes (-1.0)" -- "${B[@]}" --head "$TMP/exact.lcov"
check "exact: -1.000 points" has ': -1.000 points;' "$TMP/err"

compare 1 "a drop beyond the threshold fails (-1.5)" -- "${B[@]}" --head "$TMP/beyond.lcov"
check "beyond: the error carries both totals and the delta" \
	has '::error title=Coverage gate failed::line coverage dropped by more than 1.0 points: 78.50% (157 of 200 lines) against 80.00% for the baseline main (160 of 200 lines): -1.500 points; maximum drop 1.0 points (the default), no minimum' "$TMP/err"
check "beyond: the report says FAILED" has '## Coverage gate: FAILED' "$TMP/out"
check "beyond: the report says by how much" \
	has 'Total line coverage dropped by **1.500 points**, more than the **1.0** allowed.' "$TMP/out"
check "beyond: the report points at the runbook" has 'The gate failed' "$TMP/out"

compare 0 "a rise passes (+5.0)" -- "${B[@]}" --head "$TMP/rise.lcov"
check "rise: +5.000 points" has ': +5.000 points;' "$TMP/err"

# Deleting well-tested code lowers the percentage with no test removed: 40
# fully covered lines gone, 120 of 160 = 75.00%.
mk "$TMP/deleted.lcov" agent/a.go:150:120 cni/p.go:10:0
compare 1 "deleting covered code is a drop like any other" -- "${B[@]}" --head "$TMP/deleted.lcov"
check "deleted: the line counts show it" has '| **Delta** | -40 | -40 | **-5.000 points** |' "$TMP/out"

# --- the thresholds --------------------------------------------------------------
compare 0 "COVERAGE_MAX_DROP=2 lets a 1.5-point drop through" COVERAGE_MAX_DROP=2 -- "${B[@]}" --head "$TMP/beyond.lcov"
check "COVERAGE_MAX_DROP: the notice names where the threshold came from" \
	has 'maximum drop 2 points (the repository variable COVERAGE_MAX_DROP)' "$TMP/err"
compare 1 "COVERAGE_MAX_DROP=0.25 fails a half-point drop" COVERAGE_MAX_DROP=0.25 -- "${B[@]}" --head "$TMP/small.lcov"
compare 1 "COVERAGE_MAX_DROP=0 fails any drop" COVERAGE_MAX_DROP=0 -- "${B[@]}" --head "$TMP/small.lcov"
compare 0 "COVERAGE_MAX_DROP=0 passes equal reports" COVERAGE_MAX_DROP=0 -- "${B[@]}" --head "$TMP/equal.lcov"
compare 1 "an empty COVERAGE_MAX_DROP is the default, not zero and not off" COVERAGE_MAX_DROP= -- "${B[@]}" --head "$TMP/beyond.lcov"
check "empty COVERAGE_MAX_DROP: the default is named" has 'maximum drop 1.0 points (the default)' "$TMP/err"
compare 0 "--max-drop wins over COVERAGE_MAX_DROP" COVERAGE_MAX_DROP=0 -- "${B[@]}" --head "$TMP/small.lcov" --max-drop 1

for bad in 1% -1 +1 abc 1e3 101 100.5 1. .5 ' 1' '1 ' 1,5 0x1; do
	compare 2 "a malformed COVERAGE_MAX_DROP is refused: '$bad'" "COVERAGE_MAX_DROP=$bad" -- "${B[@]}" --head "$TMP/equal.lcov"
	check "malformed '$bad': the error quotes the value" has "is '$bad'" "$TMP/err"
done
check "malformed: the error says how to fix it" has 'Fix the variable' "$TMP/err"
compare 2 "a malformed --max-drop is refused" -- "${B[@]}" --head "$TMP/equal.lcov" --max-drop lots
compare 0 "COVERAGE_MAX_DROP=100 is accepted" COVERAGE_MAX_DROP=100 -- "${B[@]}" --head "$TMP/beyond.lcov"

compare 0 "no COVERAGE_MIN: no minimum" -- "${B[@]}" --head "$TMP/small.lcov"
compare 0 "an empty COVERAGE_MIN is off" COVERAGE_MIN= -- "${B[@]}" --head "$TMP/small.lcov"
compare 0 "COVERAGE_MIN at the total passes (79.5)" COVERAGE_MIN=79.5 -- "${B[@]}" --head "$TMP/small.lcov"
check "COVERAGE_MIN: the notice names it" has 'minimum 79.5% (the repository variable COVERAGE_MIN)' "$TMP/err"
compare 1 "COVERAGE_MIN above the total fails although the drop is allowed" COVERAGE_MIN=79.6 -- "${B[@]}" --head "$TMP/small.lcov"
check "COVERAGE_MIN: the error says which limit" \
	has 'line coverage is below the minimum of 79.6%: 79.50% (159 of 200 lines)' "$TMP/err"
check "COVERAGE_MIN: the report says it" has 'below the minimum of **79.6%**' "$TMP/out"
compare 1 "both limits broken is still one error" COVERAGE_MIN=79 -- "${B[@]}" --head "$TMP/beyond.lcov"
check "both: the error names both" has 'line coverage dropped and is below the minimum' "$TMP/err"
compare 2 "a malformed COVERAGE_MIN is refused" COVERAGE_MIN=high -- "${B[@]}" --head "$TMP/equal.lcov"

# --- no baseline -----------------------------------------------------------------
compare 2 "a missing baseline is not a pass" -- --baseline "$TMP/no-such.lcov" --head "$TMP/equal.lcov"
check "missing baseline: titled" has '::error title=Coverage gate: no baseline::' "$TMP/err"
check "missing baseline: says how to get one" has 'gh workflow run coverage.yaml --ref main' "$TMP/err"
printf 'SF:agent/a.go\nLH:0\nLF:0\nend_of_record\n' >"$TMP/empty.lcov"
compare 2 "an empty baseline is not a pass" -- --baseline "$TMP/empty.lcov" --head "$TMP/equal.lcov"
check "empty baseline: says how to get one" has 'gh workflow run coverage.yaml --ref main' "$TMP/err"
compare 2 "a missing report is refused" -- "${B[@]}" --head "$TMP/no-such.lcov"
compare 2 "an empty report is refused" -- "${B[@]}" --head "$TMP/empty.lcov"
compare 2 "a missing --changed-files list is refused" -- "${B[@]}" --head "$TMP/equal.lcov" --changed-files "$TMP/no-such.txt"
compare 2 "an unknown argument is refused" -- "${B[@]}" --head "$TMP/equal.lcov" --force
compare 2 "--head is required" -- "${B[@]}"

# --- what does not gate ----------------------------------------------------------
# Concatenated tracefiles: line 1 is hit by the first record only, line 2 by
# the second only, line 3 by neither.
printf 'SF:agent/a.go\nDA:1,1\nDA:2,0\nend_of_record\nSF:./agent/a.go\nDA:1,0\nDA:2,3\nDA:3,0\nend_of_record\n' >"$TMP/twice.lcov"
compare 0 "hits of one line are added across records" -- --baseline "$TMP/twice.lcov" --head "$TMP/twice.lcov"
check "twice: 2 of 3 lines" has '66.67% (2 of 3 lines)' "$TMP/err"

: >"$TMP/none.txt"
compare 0 "an empty changed-files list" -- "${B[@]}" --head "$TMP/equal.lcov" --changed-files "$TMP/none.txt"
check "empty list: says so" has 'None of the changed files is in either report.' "$TMP/out"
printf './cni/p.go\r\n\ncni/p.go\ndocs/runbook.md\n' >"$TMP/messy.txt"
compare 0 "a changed-files list with ./, CRLF, a blank line and a duplicate" -- "${B[@]}" --head "$TMP/rise.lcov" --changed-files "$TMP/messy.txt"
check "messy list: the file is listed once" test "$(grep -cF 'cni/p.go' "$TMP/out")" = 1
check "messy list: one file, one path in neither report" has '1 changed path(s) are in neither report' "$TMP/out"

# The gate and the summary table must agree on what they were both given.
bash "$SUMMARY" "$DATA/head.lcov" >"$TMP/summary.md" 2>/dev/null
check "the summary's total is the gate's (20 lines, 11 covered)" \
	test "$(tail -n 1 "$TMP/summary.md")" = '<!-- coverage-total lines=20 covered=11 percent=55.00 -->'

# --- which run is the baseline ---------------------------------------------------
# A fake `gh`: `api` answers the two endpoints from $FAKE_RUNS (and, once
# $FAKE_FLIP_AFTER listings have been served, from $FAKE_RUNS_LATER), applying
# the head_sha and status filters the real API applies (the status one not when
# FAKE_IGNORE_STATUS=1, so the script's own filter is tested too); `run
# download` writes a coverage.lcov that names the run it came from.
mkdir -p "$TMP/bin"
cat >"$TMP/bin/gh" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
echo "$*" >>"$FAKE_LOG"
case "$1 $2" in
"api repos/o/r/actions/workflows/coverage.yaml/runs?"*)
	[ -z "${FAKE_API_DOWN:-}" ] || { echo "gh: HTTP 502" >&2; exit 1; }
	query="${2#*\?}"
	sha="" status=""
	[[ "$query" != *head_sha=* ]] || { sha="${query##*head_sha=}"; sha="${sha%%&*}"; }
	[[ "$query" != *status=* ]] || { status="${query##*status=}"; status="${status%%&*}"; }
	[[ "$query" == *branch=main* ]] || { echo "fake gh: no branch=main in $2" >&2; exit 1; }
	calls="$(cat "$FAKE_STATE" 2>/dev/null || echo 0)"
	echo "$((calls + 1))" >"$FAKE_STATE"
	src="$FAKE_RUNS"
	if [ -n "${FAKE_FLIP_AFTER:-}" ] && [ "$calls" -ge "$FAKE_FLIP_AFTER" ]; then src="$FAKE_RUNS_LATER"; fi
	"$JQ" --arg sha "$sha" --arg status "$status" '{workflow_runs: [.runs[]
		| select($sha == "" or .head_sha == $sha)
		| select($status == "" or env.FAKE_IGNORE_STATUS == "1" or .conclusion == $status)]}' "$src"
	;;
"api repos/o/r/actions/runs/"*"/artifacts?name=coverage-report")
	id="${2#repos/o/r/actions/runs/}"
	"$JQ" --arg id "${id%%/*}" '{artifacts: (.artifacts[$id] // [])}' "$FAKE_RUNS"
	;;
"run download")
	id="$3"
	shift 3
	[ "$*" = "--repo o/r --name coverage-report --dir $FAKE_OUT" ] || { echo "fake gh: unexpected download args: $*" >&2; exit 1; }
	mkdir -p "$FAKE_OUT"
	[ -n "${FAKE_NO_LCOV:-}" ] || echo "from run $id" >"$FAKE_OUT/coverage.lcov"
	;;
*)
	echo "fake gh: unexpected $*" >&2
	exit 1
	;;
esac
FAKE
chmod +x "$TMP/bin/gh"

# run <id> <sha> <status> <conclusion> [<event> [<branch> [<repo>]]]
run() {
	"$JQ" -n --argjson id "$1" --arg sha "$2" --arg status "$3" --arg conclusion "$4" \
		--arg event "${5:-push}" --arg branch "${6:-main}" --arg repo "${7:-o/r}" \
		'{id: $id, head_sha: $sha, status: $status, conclusion: (if $conclusion == "" then null else $conclusion end),
		  event: $event, head_branch: $branch, head_repository: {full_name: $repo},
		  html_url: "https://example.test/runs/\($id)"}'
}
# fixture <name> <ids with a live artifact> <ids with an expired one> <run json>...
fixture() {
	local name="$1" live="$2" expired="$3"
	shift 3
	printf '%s\n' "$@" | "$JQ" -s --arg live "$live" --arg expired "$expired" '
		{runs: .,
		 artifacts: (
			([$live | split(" ")[] | select(. != "") | {(.): [{name: "coverage-report", expired: false}]}] | add // {})
			+ ([$expired | split(" ")[] | select(. != "") | {(.): [{name: "coverage-report", expired: true}]}] | add // {}))}' \
		>"$TMP/$name.json"
}
# baseline <want exit> <description> <fixture> <base sha> [VAR=value...] [-- <args...>]
baseline() {
	local want="$1" what="$2" fx="$3" sha="$4" rc env=()
	shift 4
	while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do
		env+=("$1")
		shift
	done
	[ "$#" -eq 0 ] || shift
	rm -rf "$TMP/dl" "$TMP/state"
	: >"$TMP/log"
	env PATH="$TMP/bin:$PATH" GH_REPO=o/r GH_TOKEN=x FAKE_LOG="$TMP/log" FAKE_STATE="$TMP/state" \
		FAKE_OUT="$TMP/dl" FAKE_RUNS="$TMP/$fx.json" COVERAGE_BASELINE_POLL_SECONDS=0 "${env[@]}" \
		bash "$BASELINE" --base-sha "$sha" --out "$TMP/dl" "$@" >"$TMP/out" 2>"$TMP/err"
	rc=$?
	if [ "$rc" -ne "$want" ]; then
		fail "$what: exit $rc, wanted $want: $(cat "$TMP/err")"
		return
	fi
	if [ "$want" -ne 0 ] && { [ -s "$TMP/out" ] || [ -e "$TMP/dl/coverage.lcov" ]; }; then
		fail "$what: failed but still named or downloaded a baseline"
		return
	fi
	pass "$what"
}

# Newest first, as the API lists them. 30 is a pull request's run that names
# main as its branch; 29 a fork's push to its own main; 28 a run on another
# branch that the API's branch filter would not return but a filter must not
# rely on; 27 a failed run (its artifact exists: a failed report job can still
# have uploaded one); 26 succeeded but its artifact expired.
fixture main "25 24 23 30 29 28 27" "26" \
	"$(run 30 ffff completed success pull_request main)" \
	"$(run 29 eeee completed success push main fork/r)" \
	"$(run 28 dddd completed success push upgrade/x)" \
	"$(run 27 cccc completed failure)" \
	"$(run 26 bbbb completed success)" \
	"$(run 25 aaaa completed success workflow_dispatch)" \
	"$(run 24 9999 completed success)" \
	"$(run 23 8888 completed success)"

baseline 0 "the base commit's own run is the baseline" main 9999
check "exact: sha, run and match are reported" \
	test "$(tr '\n' ' ' <"$TMP/out")" = "sha=9999 run_id=24 run_url=https://example.test/runs/24 match=exact "
check "exact: that run's artifact is what is downloaded" test "$(cat "$TMP/dl/coverage.lcov" 2>/dev/null)" = "from run 24"

baseline 0 "a base commit main never had (a stacked pull request) gets main's latest" main 7777
check "stack: main's latest usable run, a workflow_dispatch one" \
	test "$(tr '\n' ' ' <"$TMP/out")" = "sha=aaaa run_id=25 run_url=https://example.test/runs/25 match=latest "
check "stack: says the base commit has no run" has 'no successful coverage run on main for the base commit 7777' "$TMP/err"
check "stack: says which commit the baseline is" has 'latest baseline: main at aaaa (run 25)' "$TMP/err"

baseline 0 "a pull request's own run is never the baseline" main ffff
check "pull_request run: skipped" has 'run_id=25' "$TMP/out"
baseline 0 "a fork's run is never the baseline" main eeee
check "fork run: skipped" has 'run_id=25' "$TMP/out"
baseline 0 "another branch's run is never the baseline" main dddd
check "other branch: skipped" has 'run_id=25' "$TMP/out"
baseline 0 "a failed run of the base commit falls back to the latest" main cccc
check "failed run: skipped" test "$(tr '\n' ' ' <"$TMP/out")" = "sha=aaaa run_id=25 run_url=https://example.test/runs/25 match=latest "
baseline 0 "a failed run is never the fallback either, whatever the API returns" main 7777 FAKE_IGNORE_STATUS=1
check "failed run in the listing: skipped" has 'run_id=25' "$TMP/out"
baseline 0 "a base commit whose artifact expired falls back to the latest" main bbbb
check "expired: says so" has 'its artifact has expired' "$TMP/err"
check "expired: main's latest with a live artifact" has 'run_id=25' "$TMP/out"

fixture expired "" "26 24" "$(run 27 cccc completed failure)" "$(run 26 bbbb completed success)" "$(run 24 9999 completed success)"
baseline 1 "no run with a live artifact is not a pass" expired 9999
check "no baseline: titled" has '::error title=Coverage gate: no baseline::' "$TMP/err"
check "no baseline: says how to get one" has 'gh workflow run coverage.yaml --ref main' "$TMP/err"
check "no baseline: nothing was downloaded" lacks 'run download' "$TMP/log"
fixture nothing "" ""
baseline 1 "no run at all is not a pass" nothing 9999
check "no runs: says how to get one" has 'gh workflow run coverage.yaml --ref main' "$TMP/err"

# The pull request was pushed right after a merge: main's run for the base
# commit is still going.
fixture racing "31 24" "" "$(run 31 1111 in_progress "")" "$(run 24 9999 completed success)"
fixture raced "31 24" "" "$(run 31 1111 completed success)" "$(run 24 9999 completed success)"
baseline 0 "an unfinished base run is not waited for by default" racing 1111
check "no wait: main's latest" has 'match=latest' "$TMP/out"
check "no wait: says the base run has not finished" has 'run 31 for the base commit 1111 has not finished' "$TMP/err"
# The third listing is the first to show the run finished.
baseline 0 "the base run finishing while waited for is the baseline" racing 1111 \
	FAKE_FLIP_AFTER=2 "FAKE_RUNS_LATER=$TMP/raced.json" COVERAGE_BASELINE_POLL_SECONDS=1 -- --wait-seconds 30
check "waited: exact" test "$(tr '\n' ' ' <"$TMP/out")" = "sha=1111 run_id=31 run_url=https://example.test/runs/31 match=exact "
check "waited: says it waited" has 'waiting for main'"'"'s coverage run 31' "$TMP/err"
baseline 0 "a base run that outlasts the wait falls back to the latest" racing 1111 COVERAGE_BASELINE_POLL_SECONDS=1 -- --wait-seconds 2
check "outlasted: main's latest" has 'match=latest' "$TMP/out"

baseline 1 "an artifact without coverage.lcov is not a baseline" main 9999 FAKE_NO_LCOV=1
check "no lcov: says so" has 'has no coverage.lcov' "$TMP/err"
baseline 1 "an API failure is not a pass" main 9999 FAKE_API_DOWN=1
baseline 2 "--base-sha is required" main ""
baseline 2 "a non-numeric --wait-seconds is refused" main 9999 -- --wait-seconds soon

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS check(s) failed"
	exit 1
fi
echo "all checks passed"
