#!/usr/bin/env bash
# Hermetic test of scripts/publish-verify-close-issue.sh: which green
# publish-verify runs close the rolling "artifacts missing" issue (#1316), and
# -- the half that matters more -- which must leave it open. No network, no
# git: the shas are literals, MAIN_HEAD stands in for `git ls-remote`, and `gh`
# is a fake that records what it was asked to do.
#
# Also the expected-red control's own issues (#1340): which title and body
# scripts/publish-verify-control-issue.sh files for each way the control can
# fail (GREEN, red for the wrong reason, inconclusive, no verdict), and that
# any run whose control went red as expected closes them.
#
# Run: bazel test //scripts:publish_verify_close_issue_test
# shellcheck disable=SC2016 # single-quoted $names belong to the fake's own shell.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/publish-verify-close-issue.sh"
CONTROL_ISSUE="$HERE/publish-verify-control-issue.sh"
# The workflow, to hold its title and its step to the script: a runfile under
# Bazel (//:ci_definitions), the checkout otherwise.
WORKFLOW="${TEST_SRCDIR:-}/${TEST_WORKSPACE:-_main}/.github/workflows/publish-verify.yaml"
[ -f "$WORKFLOW" ] || WORKFLOW="$HERE/../.github/workflows/publish-verify.yaml"
for f in "$SCRIPT" "$CONTROL_ISSUE" "$WORKFLOW"; do
	[ -f "$f" ] || {
		echo "FAIL: $f not found"
		exit 1
	}
done

old="b1dc1c04aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
head="01391e0b6e567fd56470a32dc6f04a26ce558cc0" # main's head
title="publish: artifacts missing for a commit on main"
t_green="publish-verify: the expected-red control went GREEN"
t_wrong="publish-verify: the expected-red control went red for the wrong reason"
t_inconclusive="publish-verify: the expected-red control was inconclusive"
t_unknown="publish-verify: the expected-red control did not go red"

FAILS=0
pass() { echo "PASS  $*"; }
fail() {
	echo "FAIL  $*"
	FAILS=$((FAILS + 1))
}
# expect <want: close|keep> <name> <event> <commit> <head> <superseded> <gate> <signatures>
expect() {
	local want="$1" name="$2" got
	shift 2
	got="$(bash "$SCRIPT" decide "$@")"
	case "$want:$got" in
	close:close | keep:keep:\ *) pass "$name -> $got" ;;
	*) fail "$name: got '$got', want $want" ;;
	esac
}

# --- the decision ----------------------------------------------------------------
# Closed: a green run that verified what main ships now.
expect close "green run for main's head" workflow_run "$head" "$head" false success success
expect close "green scheduled sweep" schedule "" "" "" success success
expect close "green sweep needs no head" schedule "" "not-a-sha" "" success success

# Kept open:
expect keep "green run for a commit main has moved past" workflow_run "$old" "$head" false success success
expect keep "superseded and skipped: nothing was verified" workflow_run "$head" "$head" true skipped skipped
expect keep "superseded, whatever the conclusions claim" workflow_run "$head" "$head" true success success
expect keep "the gate failed" workflow_run "$head" "$head" false failure success
expect keep "the signature pass failed (run 37522284996)" workflow_run "$head" "$head" false success failure
expect keep "the signature pass was skipped" workflow_run "$head" "$head" false success skipped
expect keep "the gate did not run" schedule "" "" "" "" success
expect keep "a sweep whose signature pass failed" schedule "" "" "" success failure
expect keep "a sweep that was cancelled" schedule "" "" "" cancelled ""
expect keep "main's head unknown: fail closed" workflow_run "$head" "" false success success
expect keep "main's head malformed: fail closed" workflow_run "$head" "not-a-sha" false success success
expect keep "no commit: fail closed" workflow_run "" "$head" false success success
expect keep "abbreviated sha: fail closed" workflow_run "${head:0:12}" "${head:0:12}" false success success
expect keep "uppercase sha: not a git sha, fail closed" workflow_run "${head^^}" "${head^^}" false success success
expect keep "a manual run never closes it" workflow_dispatch "$head" "$head" false success success
expect keep "an unknown event" push "$head" "$head" false success success
expect keep "no event" "" "$head" "$head" false success success

if bash "$SCRIPT" decide schedule >/dev/null 2>&1; then
	fail "decide with too few arguments must be refused"
else
	pass "decide with too few arguments is refused"
fi

# --- the control's issues: the decision (#1340) ----------------------------------
# expect_control <want: close|keep> <name> <event> <control verdict>
expect_control() {
	local want="$1" name="$2" got
	shift 2
	got="$(bash "$SCRIPT" decide-control "$@")"
	case "$want:$got" in
	close:close | keep:keep:\ *) pass "control: $name -> $got" ;;
	*) fail "control: $name: got '$got', want $want" ;;
	esac
}
expect_control close "a publish run whose control went red as expected" workflow_run ok
expect_control close "a sweep whose control went red as expected" schedule ok
expect_control keep "the control went green" workflow_run green
expect_control keep "the control went red for the wrong reason" schedule wrong
expect_control keep "the control was inconclusive" workflow_run inconclusive
expect_control keep "no verdict on record: fail closed" schedule unknown
expect_control keep "an empty verdict: fail closed" schedule ""
expect_control keep "a verdict that is not one: fail closed" schedule OK
expect_control keep "a manual run never closes it" workflow_dispatch ok
expect_control keep "an unknown event" push ok
expect_control keep "no event" "" ok
if bash "$SCRIPT" decide-control schedule >/dev/null 2>&1; then
	fail "decide-control with too few arguments must be refused"
else
	pass "decide-control with too few arguments is refused"
fi

# --- the control's issues: one title per case ------------------------------------
for pair in "green:$t_green" "wrong:$t_wrong" "inconclusive:$t_inconclusive" "unknown:$t_unknown"; do
	if [ "$(bash "$CONTROL_ISSUE" title "${pair%%:*}")" = "${pair#*:}" ]; then
		pass "the ${pair%%:*} control is filed as \"${pair#*:}\""
	else
		fail "title ${pair%%:*}: got '$(bash "$CONTROL_ISSUE" title "${pair%%:*}")'"
	fi
done
if [ "$(bash "$CONTROL_ISSUE" titles | sort)" = "$(printf '%s\n' "$t_green" "$t_wrong" "$t_inconclusive" "$t_unknown" | sort)" ] &&
	[ "$(bash "$CONTROL_ISSUE" titles | sort -u | wc -l)" -eq 4 ]; then
	pass "titles lists the four of them, all different (what a good run closes)"
else
	fail "titles is not the four case titles: $(bash "$CONTROL_ISSUE" titles | tr '\n' '|')"
fi
if bash "$CONTROL_ISSUE" title ok >/dev/null 2>&1 || bash "$CONTROL_ISSUE" title "" >/dev/null 2>&1; then
	fail "a control that went red as expected has no issue title"
else
	pass "there is no issue title for 'ok', nor for an empty verdict"
fi

# --- the title is the workflow's -------------------------------------------------
if [ "$(bash "$SCRIPT" title)" = "$title" ] && grep -qF "title=\"${title}\"" "$WORKFLOW"; then
	pass "the title is the one the workflow files the issue under"
else
	fail "the script's title ('$(bash "$SCRIPT" title)') is not the workflow's"
fi
if grep -qE '^[[:space:]]+run: \./scripts/publish-verify-close-issue\.sh$' "$WORKFLOW"; then
	pass "the workflow runs the script"
else
	fail "no step of the workflow runs ./scripts/publish-verify-close-issue.sh"
fi
# The control's issue is filed by the script, in the step gated on the control
# step failing; the workflow keeps no title or body of its own for it.
if grep -A8 -F 'name: Open or update the vacuous-gate issue' "$WORKFLOW" |
	grep -qE '^[[:space:]]+run: \./scripts/publish-verify-control-issue\.sh$' &&
	grep -A2 -F 'name: Open or update the vacuous-gate issue' "$WORKFLOW" |
	grep -qF "steps.control.conclusion == 'failure'"; then
	pass "the workflow's vacuous-gate step runs the control-issue script, when the control step failed"
else
	fail "the vacuous-gate step does not run ./scripts/publish-verify-control-issue.sh on a failed control"
fi
if grep -qF 'title="publish-verify:' "$WORKFLOW"; then
	fail "the workflow still files a control issue under a title of its own"
else
	pass "the workflow has no control-issue title of its own"
fi

# --- the step, through a fake gh -------------------------------------------------
# $FAKE_ISSUES: `<number>\t<title>` per open issue. The fake applies the exact
# title filter the script passes as its jq expression, so a script that stopped
# filtering would be handed every issue the search returns.
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
cat >"$tmp/bin/gh" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$FAKE_LOG"
case "$1 $2" in
"issue list")
	[ -z "${FAKE_LIST_DOWN:-}" ] || { echo "gh: HTTP 502" >&2; exit 1; }
	# The title asked for is whatever the search names; the jq filter must
	# select exactly that title, or the fake refuses.
	want="${6#in:title \"}"
	want="${want%\"}"
	[ "$*" = "issue list --state open --search in:title \"${want}\" -L 20 --json number,title -q .[] | select(.title == \"${want}\") | .number" ] || {
		echo "fake gh: unexpected list arguments: $*" >&2
		exit 1
	}
	while IFS=$'\t' read -r number issue_title; do
		[ "$issue_title" = "$want" ] && echo "$number"
	done <"$FAKE_ISSUES"
	exit 0
	;;
"issue comment")
	[ -z "${FAKE_WRITE_DOWN:-}" ] || { echo "gh: HTTP 502" >&2; exit 1; }
	[ "$#" -eq 5 ] && [ "$4" = --body ] || { echo "fake gh: unexpected comment arguments: $*" >&2; exit 1; }
	printf 'comment\t%s\n' "$3" >>"$FAKE_FILED"
	printf '%s\n' "$5" >"$FAKE_BODY"
	;;
"issue create")
	[ -z "${FAKE_WRITE_DOWN:-}" ] || { echo "gh: HTTP 502" >&2; exit 1; }
	[ "$#" -eq 6 ] && [ "$3 $5" = "--title --body" ] || { echo "fake gh: unexpected create arguments: $*" >&2; exit 1; }
	printf 'create\t%s\n' "$4" >>"$FAKE_FILED"
	printf '%s\n' "$6" >"$FAKE_BODY"
	;;
"issue close")
	[ -z "${FAKE_CLOSE_DOWN:-}" ] || { echo "gh: HTTP 502" >&2; exit 1; }
	[ "$4 $5 $6" = "--reason completed --comment" ] || { echo "fake gh: unexpected close arguments: $*" >&2; exit 1; }
	printf '%s\t%s\n' "$3" "$7" >>"$FAKE_CLOSED"
	;;
*)
	echo "fake gh: unexpected $*" >&2
	exit 1
	;;
esac
FAKE
chmod +x "$tmp/bin/gh"

# step <want exit> <name> [VAR=value...]: run the workflow step; stdout+stderr in $tmp/log.
step() {
	local want="$1" name="$2" rc
	shift 2
	: >"$tmp/gh.log"
	: >"$tmp/closed"
	env -u RUNNER_TEMP -u CONTROL_RESULT_FILE \
		PATH="$tmp/bin:$PATH" GH_TOKEN=x GH_REPO=o/r \
		FAKE_LOG="$tmp/gh.log" FAKE_ISSUES="$tmp/issues" FAKE_CLOSED="$tmp/closed" \
		RUN_URL=https://example.invalid/runs/42 MAIN_HEAD="$head" \
		VERIFY_CONCLUSION=success COSIGN_CONCLUSION=success SUPERSEDED=false "$@" \
		bash "$SCRIPT" >"$tmp/log" 2>&1
	rc=$?
	if [ "$rc" -ne "$want" ]; then
		fail "$name: exit $rc, wanted $want: $(cat "$tmp/log")"
		return 1
	fi
}
check() {
	local what="$1"
	shift
	if "$@"; then pass "$what"; else
		fail "$what"
		cat "$tmp/log" "$tmp/gh.log" "$tmp/closed"
	fi
}

# Two decoys the search would return: a title that only contains the words,
# and the control's own rolling issue.
printf '%s\t%s\n' \
	1316 "$title" \
	1400 "Re: ${title} (discussion)" \
	1401 "publish-verify: the expected-red control did not go red" >"$tmp/issues"

step 0 "green run for main's head" GITHUB_EVENT_NAME=workflow_run TARGET="$head" &&
	check "head: exactly #1316 is closed, with one line naming the run" test "$(cat "$tmp/closed")" = \
		"1316	Closed by a green publish-verify run: main's head ${head:0:12} is published and its signatures verify. https://example.invalid/runs/42"
check "head: says so in the log" grep -qx 'closed #1316' "$tmp/log"

step 0 "green sweep" GITHUB_EVENT_NAME=schedule TARGET= MAIN_HEAD= &&
	check "sweep: #1316 is closed, the comment says it was the sweep" test "$(cat "$tmp/closed")" = \
		"1316	Closed by a green publish-verify run: the scheduled sweep verified every push head it covers. https://example.invalid/runs/42"

step 0 "green run for an older commit" GITHUB_EVENT_NAME=workflow_run TARGET="$old" &&
	check "older commit: gh is never called" test ! -s "$tmp/gh.log"
check "older commit: the log says why" grep -q "keep: ${old:0:12} is no longer main's head" "$tmp/log"

step 0 "superseded" GITHUB_EVENT_NAME=workflow_run TARGET="$head" SUPERSEDED=true VERIFY_CONCLUSION=skipped COSIGN_CONCLUSION=skipped &&
	check "superseded: gh is never called" test ! -s "$tmp/gh.log"

step 0 "manual run" GITHUB_EVENT_NAME=workflow_dispatch TARGET="$head" &&
	check "manual: gh is never called" test ! -s "$tmp/gh.log"

step 0 "signature pass failed" GITHUB_EVENT_NAME=workflow_run TARGET="$head" COSIGN_CONCLUSION=failure &&
	check "red: gh is never called" test ! -s "$tmp/gh.log"

# Two open issues with the title (a duplicate filed by hand): both go.
printf '%s\t%s\n' 1316 "$title" 1320 "$title" >"$tmp/issues"
step 0 "two open issues" GITHUB_EVENT_NAME=schedule &&
	check "duplicates: both are closed" test "$(cut -f1 "$tmp/closed" | tr '\n' ' ')" = "1316 1320 "

# The normal case: nothing is open.
: >"$tmp/issues"
step 0 "no open issue" GITHUB_EVENT_NAME=schedule &&
	check "none open: nothing is closed" test ! -s "$tmp/closed"
check "none open: says so" grep -q 'nothing to close' "$tmp/log"

# The issue API failing must not turn a green verification red.
printf '%s\t%s\n' 1316 "$title" >"$tmp/issues"
step 0 "the listing fails" GITHUB_EVENT_NAME=schedule FAKE_LIST_DOWN=1 &&
	check "listing down: a warning, not a failure" grep -q '^::warning title=publish-verify::could not list open issues' "$tmp/log"
check "listing down: nothing is closed" test ! -s "$tmp/closed"
step 0 "the close fails" GITHUB_EVENT_NAME=schedule FAKE_CLOSE_DOWN=1 &&
	check "close down: a warning naming the issue" grep -q '^::warning title=publish-verify::could not close #1316' "$tmp/log"

# --- the control's issues are closed by a run whose control went red (#1340) ------
# Every kind of control issue open at once, plus the artifacts issue and a
# decoy that only quotes a control title.
all_open() {
	printf '%s\t%s\n' \
		1316 "$title" \
		1340 "$t_unknown" \
		1350 "$t_inconclusive" \
		1351 "$t_green" \
		1352 "$t_wrong" \
		1353 "Re: ${t_inconclusive} (again?)" >"$tmp/issues"
}
closed_numbers() { cut -f1 "$tmp/closed" | sort -n | tr '\n' ' '; }
record() { printf '%s\n' "$@" >"$tmp/control.result"; }

all_open
record ok
step 0 "a run for an older commit whose control went red as expected" \
	GITHUB_EVENT_NAME=workflow_run TARGET="$old" CONTROL_RESULT_FILE="$tmp/control.result" &&
	check "control ok, older commit: the four control issues close, the artifacts issue and the decoy do not" \
		test "$(closed_numbers)" = "1340 1350 1351 1352 "
check "control ok: the comment says the control went red, and names the run" grep -qxF \
	"1350	Closed by a publish-verify run whose expected-red control went red as expected: the gate was shown to fail on a never-published commit. https://example.invalid/runs/42" "$tmp/closed"
check "control ok: the log says both decisions" grep -qx 'expected-red control (recorded: ok): close' "$tmp/log"

step 0 "a superseded run whose control went red as expected" \
	GITHUB_EVENT_NAME=workflow_run TARGET="$head" SUPERSEDED=true VERIFY_CONCLUSION=skipped COSIGN_CONCLUSION=skipped \
	CONTROL_RESULT_FILE="$tmp/control.result" &&
	check "control ok, superseded: the control issues close (the control does not depend on the commit)" \
		test "$(closed_numbers)" = "1340 1350 1351 1352 "

step 0 "a green sweep whose control went red as expected" GITHUB_EVENT_NAME=schedule CONTROL_RESULT_FILE="$tmp/control.result" &&
	check "control ok, sweep: the artifacts issue and the control issues all close" \
		test "$(closed_numbers)" = "1316 1340 1350 1351 1352 "

# RUNNER_TEMP is where the control records it in Actions, with no variable set.
mkdir -p "$tmp/runner"
cp "$tmp/control.result" "$tmp/runner/publish-verify-control.result"
step 0 "the verdict is found under RUNNER_TEMP" GITHUB_EVENT_NAME=workflow_run TARGET="$old" RUNNER_TEMP="$tmp/runner" &&
	check "RUNNER_TEMP: the control issues close" test "$(closed_numbers)" = "1340 1350 1351 1352 "

for v in green wrong inconclusive garbage ""; do
	record "$v" "some summary"
	step 0 "a green sweep whose control recorded '${v}'" GITHUB_EVENT_NAME=schedule CONTROL_RESULT_FILE="$tmp/control.result" &&
		check "control '${v}': only the artifacts issue closes" test "$(closed_numbers)" = "1316 "
done
step 0 "a green sweep with no verdict on record" GITHUB_EVENT_NAME=schedule CONTROL_RESULT_FILE="$tmp/absent" &&
	check "no verdict: only the artifacts issue closes (fail closed)" test "$(closed_numbers)" = "1316 "
check "no verdict: the log says why" grep -q 'expected-red control (recorded: unknown): keep: ' "$tmp/log"

record ok
step 0 "a manual run whose control went red as expected" GITHUB_EVENT_NAME=workflow_dispatch TARGET="$head" CONTROL_RESULT_FILE="$tmp/control.result" &&
	check "manual, control ok: gh is never called" test ! -s "$tmp/gh.log"

step 0 "control ok, the listing fails" GITHUB_EVENT_NAME=workflow_run TARGET="$old" CONTROL_RESULT_FILE="$tmp/control.result" FAKE_LIST_DOWN=1 &&
	check "control ok, listing down: one warning per title, exit 0" test "$(grep -c '^::warning title=publish-verify::could not list open issues' "$tmp/log")" = 4
step 0 "control ok, the close fails" GITHUB_EVENT_NAME=workflow_run TARGET="$old" CONTROL_RESULT_FILE="$tmp/control.result" FAKE_CLOSE_DOWN=1 &&
	check "control ok, close down: a warning naming each issue" test "$(grep -c '^::warning title=publish-verify::could not close #13' "$tmp/log")" = 4

# --- filing the control's issue: which case, in the title and the body (#1340) ----
# file <want exit> <name> [VAR=value...]: run the vacuous-gate step.
file() {
	local want="$1" name="$2" rc
	shift 2
	: >"$tmp/gh.log"
	: >"$tmp/filed"
	: >"$tmp/body"
	env -u RUNNER_TEMP PATH="$tmp/bin:$PATH" GH_TOKEN=x GH_REPO=o/r \
		FAKE_LOG="$tmp/gh.log" FAKE_ISSUES="$tmp/issues" FAKE_FILED="$tmp/filed" FAKE_BODY="$tmp/body" \
		RUN_URL=https://example.invalid/runs/42 CONTROL_RESULT_FILE="$tmp/control.result" "$@" \
		bash "$CONTROL_ISSUE" >"$tmp/log" 2>&1
	rc=$?
	if [ "$rc" -ne "$want" ]; then
		fail "$name: exit $rc, wanted $want: $(cat "$tmp/log")"
		return 1
	fi
}
body_has() { grep -qF -- "$1" "$tmp/body"; }
body_lacks() { ! grep -qF -- "$1" "$tmp/body"; }

# Run 37586580592, as it would be filed now: the registry answered 502 through
# every attempt.
: >"$tmp/issues"
record inconclusive \
	"INCONCLUSIVE — the verifier could not complete (exit 2) on any of 3 attempt(s); a red caused by an unreadable registry proves nothing about detection" \
	"the verifier's last attempt said:" \
	"registry-lib: GET https://registry.example/v2/auth?service=registry.example&scope=repository:acme/widget:pull answered HTTP 502; giving up after 4 attempt(s)" \
	"could not obtain a pull token for acme/widget"
file 0 "an inconclusive control, nothing open" &&
	check "inconclusive: a new issue under the inconclusive title" test "$(cat "$tmp/filed")" = "create	${t_inconclusive}"
check "inconclusive: the body says it is NOT the control going green" body_has "**The control was INCONCLUSIVE: no verdict on the gate, either way.**"
check "inconclusive: ... and that it closes itself" body_has "closes itself on the next run whose control goes red as expected"
check "inconclusive: the body quotes what the registry answered" body_has "answered HTTP 502; giving up after 4 attempt(s)"
check "inconclusive: the body quotes the attempts" body_has "on any of 3 attempt(s)"
check "inconclusive: the body names the run" body_has "Run: https://example.invalid/runs/42"
check "inconclusive: the body does not call it a vacuous gate" body_lacks "The gate is vacuous"

record green "the verifier PASSED a commit that was never published — the gate is vacuous" \
	"0 MISSING line(s), expected 22 — part of the gate cannot fail"
file 0 "a control that went green, nothing open" &&
	check "green: a new issue under the GREEN title" test "$(cat "$tmp/filed")" = "create	${t_green}"
check "green: the body says it is a real defect" body_has "**The control went GREEN: a real defect in the gate.**"
check "green: the body quotes the control" body_has "the verifier PASSED a commit that was never published"
check "green: the body does not call it inconclusive" body_lacks "INCONCLUSIVE"

record wrong "a MISSING line does not name the control sha — red for the wrong reason:"
file 0 "a control that went red for the wrong reason" &&
	check "wrong: a new issue under its own title" test "$(cat "$tmp/filed")" = "create	${t_wrong}"
check "wrong: the body says which" body_has "**The control went red, but for the wrong reason: a real defect in the gate.**"

# No verdict: the control was killed before it wrote one.
rm -f "$tmp/control.result"
file 0 "a control that left no verdict" &&
	check "no verdict: filed under the general title" test "$(cat "$tmp/filed")" = "create	${t_unknown}"
check "no verdict: the body says there is none, and guesses nothing" body_has "**The control ended without recording a verdict.**"
check "no verdict: the body says there is no summary" body_has "(the control recorded no summary; see the run log)"
record "not-a-verdict" "x"
file 0 "a verdict file that does not hold a verdict" &&
	check "garbage: filed under the general title" test "$(cat "$tmp/filed")" = "create	${t_unknown}"

# Reuse: the open issue with EXACTLY this case's title gets the comment. The
# other cases' issues, which share most of its words, do not.
printf '%s\t%s\n' 1351 "$t_green" 1352 "$t_wrong" 1353 "Re: ${t_inconclusive} (again?)" 1350 "$t_inconclusive" 1360 "$t_inconclusive" >"$tmp/issues"
record inconclusive "INCONCLUSIVE — again"
file 0 "an inconclusive control, the inconclusive issue already open" &&
	check "reuse: a comment on #1350, no new issue" test "$(cat "$tmp/filed")" = "comment	1350"
check "reuse: says so" grep -qx 'commented on #1350' "$tmp/log"
record green "the verifier PASSED"
file 0 "then the control goes green while the inconclusive issue is open" &&
	check "another case: its own issue, #1351" test "$(cat "$tmp/filed")" = "comment	1351"
printf '%s\t%s\n' 1350 "$t_inconclusive" >"$tmp/issues"
file 0 "the control goes green with only the inconclusive issue open" &&
	check "another case, none open: a NEW issue, the inconclusive one is not reused" test "$(cat "$tmp/filed")" = "create	${t_green}"

# A control that went red as expected files nothing, whatever ran this step.
record ok
file 0 "a control that went red as expected" &&
	check "ok: gh is never called" test ! -s "$tmp/gh.log"

# What the control said is quoted in a code fence: no backtick may close it, no
# control byte may reach the issue, and a runaway summary is cut.
{
	printf 'inconclusive\n'
	printf 'a line with ``` a fence and \033[31m an escape\n'
	for i in $(seq 1 40); do printf 'line %s\n' "$i"; done
} >"$tmp/control.result"
: >"$tmp/issues"
file 0 "a summary with a fence, an escape and forty lines" &&
	check "summary: exactly the one opening and one closing fence" test "$(grep -c '```' "$tmp/body")" = 2
check "summary: the escape byte is gone" bash -c '! grep -q "$(printf "\033")" "$1"' _ "$tmp/body"
check "summary: cut at twenty lines" bash -c 'grep -qx "line 19" "$1" && ! grep -qx "line 20" "$1"' _ "$tmp/body"

# The run is already red; an issue that could not be filed must not pass quietly.
record green "the verifier PASSED"
file 1 "the issue API is down while filing" FAKE_WRITE_DOWN=1
: >"$tmp/issues"
file 1 "the listing is down while filing" FAKE_LIST_DOWN=1 &&
	check "listing down: nothing is filed blind" test ! -s "$tmp/filed"

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS check(s) failed"
	exit 1
fi
echo "all checks passed"
