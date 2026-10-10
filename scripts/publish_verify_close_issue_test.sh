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
# Whose issue it is (#1532) and how one is opened (#1568): only an issue the
# workflow's own token opened is commented on or closed, and a new one carries
# its labels. The fake `gh` is scripts/fake-gh-issues.sh, which keeps the
# issues as JSON and applies the scripts' own `--jq` filters with jq (the
# Bazel-pinned one, or the one on PATH).
#
# Run: bazel test //scripts:publish_verify_close_issue_test
# shellcheck disable=SC2016 # single-quoted $names belong to the fake's own shell.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/publish-verify-close-issue.sh"
CONTROL_ISSUE="$HERE/publish-verify-control-issue.sh"
MISSING_ISSUE="$HERE/publish-verify-missing-issue.sh"
# The workflow, to hold its title and its step to the script: a runfile under
# Bazel (//:ci_definitions), the checkout otherwise.
WORKFLOW="${TEST_SRCDIR:-}/${TEST_WORKSPACE:-_main}/.github/workflows/publish-verify.yaml"
[ -f "$WORKFLOW" ] || WORKFLOW="$HERE/../.github/workflows/publish-verify.yaml"
if [ -n "${JQ_RLOCATIONPATH:-}" ]; then
	JQ="${TEST_SRCDIR:-${RUNFILES_DIR:-$PWD/..}}/${JQ_RLOCATIONPATH}"
fi
JQ="${JQ:-$(command -v jq)}"
[ -x "$JQ" ] || {
	echo "FAIL: no jq (JQ=${JQ})"
	exit 1
}
export JQ
for f in "$SCRIPT" "$CONTROL_ISSUE" "$MISSING_ISSUE" "$WORKFLOW" "$HERE/fake-gh-issues.sh" "$HERE/rolling-issue-lib.sh"; do
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
# The issue is opened by scripts/publish-verify-missing-issue.sh, which takes
# its title from the script that closes it: one title, in one place. The
# workflow step runs that script and keeps no title, body or `gh` call of its
# own (#1532: the inline step took the first hit of a title search).
if [ "$(bash "$SCRIPT" title)" = "$title" ] && grep -qF '"${here}/publish-verify-close-issue.sh" title' "$MISSING_ISSUE"; then
	pass "the title is the closing script's, and the opening script reads it from there"
else
	fail "the script's title ('$(bash "$SCRIPT" title)') is not the one the opening script files under"
fi
open_step="$(awk '/^      - name: / { on = ($0 ~ /Open or update the missing-artifacts issue/) } on' "$WORKFLOW")"
if grep -qE '^[[:space:]]+run: \./scripts/publish-verify-missing-issue\.sh$' <<<"$open_step"; then
	pass "the workflow's missing-artifacts step runs the script"
else
	fail "the missing-artifacts step does not run ./scripts/publish-verify-missing-issue.sh"
fi
if grep -qE 'gh issue|title=' <<<"$open_step"; then
	fail "the missing-artifacts step still has issue logic of its own"
else
	pass "the missing-artifacts step has no title and no gh call of its own"
fi
# What the step is gated on, and what it hands the script, must not have moved.
if grep -qF "if: github.event_name != 'workflow_dispatch' && (cancelled() || (failure() && (steps.verify.conclusion == 'failure' || steps.cosign.conclusion == 'failure')))" <<<"$open_step" &&
	grep -qF 'VERIFY_CONCLUSION: ${{ steps.verify.conclusion }}' <<<"$open_step" &&
	grep -qF 'COSIGN_CONCLUSION: ${{ steps.cosign.conclusion }}' <<<"$open_step" &&
	grep -qF 'TRIGGERING_RUN: ${{ github.event.workflow_run.html_url }}' <<<"$open_step" &&
	grep -qF 'GH_REPO: ${{ github.repository }}' <<<"$open_step"; then
	pass "the missing-artifacts step keeps its condition and its environment"
else
	fail "the missing-artifacts step's condition or environment changed"
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
# $tmp/issues: `<number>\t<title>[\t<who opened it>]` per open issue; the
# workflow's own token unless a third field names somebody else (`mallory`: a
# person, `dependabot`: another bot, `pr`: a pull request of the workflow's).
# seed_issues turns it into the fake's JSON before every run, and after the run
# $tmp/closed (`<number>\t<the comment it was closed with>`), $tmp/filed
# (`create\t<title>` or `comment\t<number>`) and $tmp/body (what was filed) are
# read back from the fake.
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/state"
cat >"$tmp/bin/gh" <<EOF
#!/usr/bin/env bash
exec bash "$HERE/fake-gh-issues.sh" "\$@"
EOF
chmod +x "$tmp/bin/gh"
: >"$tmp/issues"
seed_issues() {
	# shellcheck disable=SC2086 # SEED_LABELS is a list of words
	printf '%s\n' ${SEED_LABELS:-bug ci enhancement} >"$tmp/state/labels"
	"$JQ" -R -s 'split("\n") | map(select(. != "") | split("\t")) | map(. as $r |
		{number: ($r[0] | tonumber), title: $r[1], state: "open", body: "", labels: [], comments: [],
		 user: (if $r[2] == "mallory" then {login: "mallory", type: "User"}
		        elif $r[2] == "dependabot" then {login: "dependabot[bot]", type: "Bot"}
		        else {login: "github-actions[bot]", type: "Bot"} end)}
		| if $r[2] == "pr" then .pull_request = {} else . end)' "$tmp/issues" >"$tmp/state/issues.json"
	# SEED_RACE=<number>:<title>: another run opens that issue while this one
	# is creating its own.
	rm -f "$tmp/state/race.json"
	if [ -n "${SEED_RACE:-}" ]; then
		"$JQ" -n --argjson n "${SEED_RACE%%:*}" --arg t "${SEED_RACE#*:}" \
			'{number: $n, title: $t, state: "open", body: "", labels: [], comments: [],
			  user: {login: "github-actions[bot]", type: "Bot"}}' >"$tmp/state/race.json"
	fi
}
# What the run wrote, in the shapes the checks below read.
read_back() {
	local n
	: >"$tmp/closed"
	: >"$tmp/filed"
	: >"$tmp/body"
	while read -r n; do
		printf '%s\t%s\n' "$n" "$("$JQ" -r --argjson n "$n" '.[] | select(.number == $n) | .comments[-1].body // ""' "$tmp/state/issues.json")" >>"$tmp/closed"
	done < <(sed -n 's/^WRITE issue close \([0-9]*\) completed$/\1/p' "$tmp/gh.log")
	while read -r n; do
		printf 'comment\t%s\n' "$n" >>"$tmp/filed"
		"$JQ" -r --argjson n "$n" '.[] | select(.number == $n) | .comments[-1].body' "$tmp/state/issues.json" >"$tmp/body"
	done < <(sed -n 's/^WRITE issue comment \([0-9]*\)$/\1/p' "$tmp/gh.log")
	while read -r n; do
		printf 'create\t%s\n' "$("$JQ" -r --argjson n "$n" '.[] | select(.number == $n) | .title' "$tmp/state/issues.json")" >>"$tmp/filed"
		"$JQ" -r --argjson n "$n" '.[] | select(.number == $n) | .body' "$tmp/state/issues.json" >"$tmp/body"
	done < <(sed -n 's/^WRITE issue create \([0-9]*\) .*$/\1/p' "$tmp/gh.log")
}

# step <want exit> <name> [VAR=value...]: run the workflow step; stdout+stderr in $tmp/log.
step() {
	local want="$1" name="$2" rc
	shift 2
	: >"$tmp/gh.log"
	seed_issues
	env -u RUNNER_TEMP -u CONTROL_RESULT_FILE -u FAKE_FAIL -u FAKE_LABELS \
		PATH="$tmp/bin:$PATH" GH_TOKEN=x GH_REPO=o/r \
		FAKE_LOG="$tmp/gh.log" FAKE_STATE="$tmp/state" \
		RUN_URL=https://example.invalid/runs/42 MAIN_HEAD="$head" \
		VERIFY_CONCLUSION=success COSIGN_CONCLUSION=success SUPERSEDED=false "$@" \
		bash "$SCRIPT" >"$tmp/log" 2>&1
	rc=$?
	read_back
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

# Two open issues of the workflow's own with the title (two runs opened one in
# the same moment): both go.
printf '%s\t%s\n' 1316 "$title" 1320 "$title" >"$tmp/issues"
step 0 "two open issues" GITHUB_EVENT_NAME=schedule &&
	check "duplicates: both are closed" test "$(cut -f1 "$tmp/closed" | tr '\n' ' ')" = "1316 1320 "

# An issue somebody else opened under the title is theirs (#1532): a person's,
# another bot's, and a pull request of the workflow's own all stay as they are.
printf '%s\t%s\t%s\n' 1316 "$title" "" 1321 "$title" mallory 1322 "$title" dependabot 1323 "$title" pr >"$tmp/issues"
step 0 "issues under the title that are not the workflow's" GITHUB_EVENT_NAME=schedule &&
	check "not the workflow's: only its own #1316 is closed" test "$(cut -f1 "$tmp/closed" | tr '\n' ' ')" = "1316 "
check "not the workflow's: nothing is written on the others" test "$(grep -c '^WRITE' "$tmp/gh.log")" = 2
printf '%s\t%s\t%s\n' 1321 "$title" mallory >"$tmp/issues"
step 0 "only a stranger's issue under the title" GITHUB_EVENT_NAME=schedule &&
	check "a stranger's issue alone: nothing is written" test "$(grep -c '^WRITE' "$tmp/gh.log")" = 0
check "a stranger's issue alone: the log says there is none to close" grep -q 'nothing to close' "$tmp/log"

# The normal case: nothing is open.
: >"$tmp/issues"
step 0 "no open issue" GITHUB_EVENT_NAME=schedule &&
	check "none open: nothing is closed" test ! -s "$tmp/closed"
check "none open: says so" grep -q 'nothing to close' "$tmp/log"

# A failure is written on the issue between this run's reading of it and its
# close (publish-verify runs overlap: one group per commit). GitHub takes a
# comment on a closed issue, so the failure would sit where nobody looks: the
# closer reads the issue again after closing, and reopens it.
issue_state() { "$JQ" -r --argjson n "$1" '.[] | select(.number == $n) | .state' "$tmp/state/issues.json"; }
printf '%s\t%s\n' 1316 "$title" >"$tmp/issues"
step 0 "a failure lands between the read and the close" GITHUB_EVENT_NAME=schedule \
	FAKE_COMMENT_BEFORE_CLOSE="A commit on main is missing artifacts in the image registry" &&
	check "failure in between: the issue is open again" test "$(issue_state 1316)" = open
check "failure in between: closed, then reopened, in that order" test \
	"$(grep -E '^WRITE issue (close|reopen) 1316' "$tmp/gh.log" | tr '\n' '|')" = "WRITE issue close 1316 completed|WRITE issue reopen 1316|"
check "failure in between: the log says why" grep -q '^reopened #1316: a failure was written on it while it was being closed' "$tmp/log"
# Another green run closing it in the same moment is not a failure: it stays closed.
step 0 "another green run comments in between" GITHUB_EVENT_NAME=schedule \
	FAKE_COMMENT_BEFORE_CLOSE="Closed by a green publish-verify run: the scheduled sweep verified every push head it covers. https://example.invalid/runs/43" &&
	check "another closer in between: the issue stays closed" test "$(issue_state 1316)" = closed
# Nor is the library's own note on a duplicate it is folding, in either wording
# (scripts/rolling-issue-lib.sh): no report landed, so nothing is reopened.
for note in "Duplicate of #1300, which is older: the report is there." \
	"Duplicate of #1300, opened in the same moment, which is older: the report is there."; do
	step 0 "a duplicate note lands in between" GITHUB_EVENT_NAME=schedule FAKE_COMMENT_BEFORE_CLOSE="$note" &&
		check "a duplicate note in between: the issue stays closed" test "$(issue_state 1316)" = closed
done
# ... and those are the very notes the library writes.
check "the library's two duplicate notes start the way the count leaves out" test \
	"$(grep -c 'rolling_issue_comment "$[a-z]*" "${ROLLING_ISSUE_NOTE_PREFIX}' "$HERE/rolling-issue-lib.sh") $(grep -c 'A duplicate of\|"Opened in the same moment' "$HERE/rolling-issue-lib.sh")" = "2 0"
# Nor is a comment by anyone else: a person writing on the issue in that moment
# does not get to reopen it.
step 0 "a person comments in between" GITHUB_EVENT_NAME=schedule \
	FAKE_COMMENT_BEFORE_CLOSE="is this still happening?" FAKE_COMMENT_BEFORE_CLOSE_BY=user &&
	check "a person's comment in between: the issue stays closed" test "$(issue_state 1316)" = closed
# What is on the issue cannot be read before the close: it is not closed blind.
step 0 "the comments cannot be read" GITHUB_EVENT_NAME=schedule FAKE_FAIL=comments &&
	check "comments unreadable: nothing is closed" test "$(issue_state 1316) $(grep -c '^WRITE' "$tmp/gh.log")" = "open 0"
check "comments unreadable: a warning, not a failure" grep -q '^::warning title=publish-verify::could not read #1316' "$tmp/log"
# The same for the control's issues, which close through the same function.
printf '%s\t%s\n' 1350 "$t_inconclusive" >"$tmp/issues"
printf '%s\n' ok >"$tmp/control.result"
step 0 "a control failure lands between the read and the close" GITHUB_EVENT_NAME=workflow_run TARGET="$old" \
	CONTROL_RESULT_FILE="$tmp/control.result" FAKE_COMMENT_BEFORE_CLOSE="**The control was INCONCLUSIVE: no verdict on the gate, either way.**" &&
	check "control failure in between: the issue is open again" test "$(issue_state 1350)" = open

# The issue API failing must not turn a green verification red.
printf '%s\t%s\n' 1316 "$title" >"$tmp/issues"
step 0 "the listing fails" GITHUB_EVENT_NAME=schedule FAKE_FAIL=list &&
	check "listing down: a warning, not a failure" grep -q '^::warning title=publish-verify::could not list open issues' "$tmp/log"
check "listing down: nothing is closed" test ! -s "$tmp/closed"
step 0 "the close fails" GITHUB_EVENT_NAME=schedule FAKE_FAIL=patch &&
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

step 0 "control ok, the listing fails" GITHUB_EVENT_NAME=workflow_run TARGET="$old" CONTROL_RESULT_FILE="$tmp/control.result" FAKE_FAIL=list &&
	check "control ok, listing down: one warning per title, exit 0" test "$(grep -c '^::warning title=publish-verify::could not list open issues' "$tmp/log")" = 4
step 0 "control ok, the close fails" GITHUB_EVENT_NAME=workflow_run TARGET="$old" CONTROL_RESULT_FILE="$tmp/control.result" FAKE_FAIL=patch &&
	check "control ok, close down: a warning naming each issue" test "$(grep -c '^::warning title=publish-verify::could not close #13' "$tmp/log")" = 4

# --- filing the control's issue: which case, in the title and the body (#1340) ----
# file <want exit> <name> [VAR=value...]: run the vacuous-gate step.
file() {
	local want="$1" name="$2" rc
	shift 2
	: >"$tmp/gh.log"
	seed_issues
	env -u RUNNER_TEMP -u FAKE_FAIL -u FAKE_LABELS PATH="$tmp/bin:$PATH" GH_TOKEN=x GH_REPO=o/r \
		FAKE_LOG="$tmp/gh.log" FAKE_STATE="$tmp/state" \
		RUN_URL=https://example.invalid/runs/42 CONTROL_RESULT_FILE="$tmp/control.result" "$@" \
		bash "$CONTROL_ISSUE" >"$tmp/log" 2>&1
	rc=$?
	read_back
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
	check "reuse: the report is a comment on #1350, no new issue" test "$(grep -c "^WRITE issue create" "$tmp/gh.log") $(sed -n 1p "$tmp/filed")" = "0 comment	1350"
# #1360 is a second open issue of the workflow's own under the title: closed as
# a duplicate of the older, so one rolling issue is left.
check "reuse: the newer duplicate #1360 is closed" grep -qx "WRITE issue close 1360 not_planned" "$tmp/gh.log"
check "reuse: says so" grep -qx 'commented on #1350' "$tmp/log"
record green "the verifier PASSED"
file 0 "then the control goes green while the inconclusive issue is open" &&
	check "another case: its own issue, #1351" test "$(cat "$tmp/filed")" = "comment	1351"
printf '%s\t%s\n' 1350 "$t_inconclusive" >"$tmp/issues"
file 0 "the control goes green with only the inconclusive issue open" &&
	check "another case, none open: a NEW issue, the inconclusive one is not reused" test "$(cat "$tmp/filed")" = "create	${t_green}"

# Whose issue (#1532): an inconclusive issue a person opened is not the
# workflow's. It gets no comment, and the workflow opens its own.
printf '%s\t%s\t%s\n' 1350 "$t_inconclusive" mallory 1354 "$t_inconclusive" dependabot >"$tmp/issues"
record inconclusive "INCONCLUSIVE — again"
file 0 "an inconclusive control, a stranger's issue open under the title" &&
	check "a stranger's issue: a NEW issue, no comment on theirs" test "$(cat "$tmp/filed")" = "create	${t_inconclusive}"
# How it is opened (#1568): with a kind and an area label.
check "a new control issue is labelled bug and ci" grep -qxF "WRITE issue create 1355 [bug+ci]: ${t_inconclusive}" "$tmp/gh.log"
# A label that is gone: the issue is filed all the same, and the step fails
# (GitHub refusing the label, or dropping it without a word).
: >"$tmp/issues"
for mode in reject drop; do
	SEED_LABELS=bug file 1 "a label is gone (GitHub would ${mode} it)" FAKE_LABELS="$mode" &&
		check "label gone (${mode}): the issue is filed" test "$(cat "$tmp/filed")" = "create	${t_inconclusive}"
	check "label gone (${mode}): the log says which label" grep -qE 'was opened without the label\(s\) (bug, )?ci:' "$tmp/log"
done
# Two runs that opened one in the same moment: the report moves to the older
# issue and the newer is closed.
printf '%s\t%s\n' 2000 "an unrelated issue" >"$tmp/issues"
SEED_RACE="1999:${t_inconclusive}" file 0 "another run opens the same issue in the same moment" &&
	check "same moment: the report is a comment on the older #1999" grep -qx 'WRITE issue comment 1999' "$tmp/gh.log"
check "same moment: the newer #2001 is closed as a duplicate" grep -qx 'WRITE issue close 2001 not_planned' "$tmp/gh.log"

# --- filing the missing-artifacts issue (#1532, #1568) -----------------------------
# miss <want exit> <name> [VAR=value...]: run the missing-artifacts step on the
# logs in $tmp/logs.
miss() {
	local want="$1" name="$2" rc
	shift 2
	: >"$tmp/gh.log"
	seed_issues
	env -u RUNNER_TEMP -u FAKE_FAIL -u FAKE_LABELS PATH="$tmp/bin:$PATH" GH_TOKEN=x GH_REPO=o/r \
		FAKE_LOG="$tmp/gh.log" FAKE_STATE="$tmp/state" LOG_DIR="$tmp/logs" \
		RUN_URL=https://example.invalid/runs/42 TRIGGERING_RUN=https://example.invalid/runs/41 \
		VERIFY_CONCLUSION=failure COSIGN_CONCLUSION=skipped "$@" \
		bash "$MISSING_ISSUE" >"$tmp/log" 2>&1
	rc=$?
	read_back
	if [ "$rc" -ne "$want" ]; then
		fail "$name: exit $rc, wanted $want: $(cat "$tmp/log")"
		return 1
	fi
}
mkdir -p "$tmp/logs"
printf '%s\n' 'checking 01391e0b6e56' '  MISSING  quay.io/acme/agent:1.0.0-01391e0' '  ok       quay.io/acme/cni:1.0.0-01391e0' \
	'  MISSING  a ``` fence and an escape '"$(printf '\033')"'[31m' >"$tmp/logs/verify.log"
printf '%s\n' '  FAILED   quay.io/acme/registrar@sha256:abc' >"$tmp/logs/cosign.log"

# The decoys the old search would have handed over first: a stranger's issue
# under the title, another bot's, a longer title.
printf '%s\t%s\t%s\n' 1500 "$title" mallory 1501 "$title" dependabot 1502 "Re: ${title} (discussion)" "" >"$tmp/issues"
miss 0 "a gap, only issues that are not the workflow's open" &&
	check "missing: a NEW issue under the title, nothing written on the others" test "$(cat "$tmp/filed") $(grep -c '^WRITE' "$tmp/gh.log")" = "create	${title} 1"
check "missing: the new issue is labelled bug and ci" grep -qxF "WRITE issue create 1503 [bug+ci]: ${title}" "$tmp/gh.log"
check "missing: the body lists what is missing" body_has "MISSING quay.io/acme/agent:1.0.0-01391e0"
check "missing: ... and what does not verify" body_has "UNVERIFIED quay.io/acme/registrar@sha256:abc"
check "missing: ... and not what is there" body_lacks "quay.io/acme/cni"
check "missing: the body names both runs" bash -c 'grep -qxF "Verification run: https://example.invalid/runs/42" "$1" && grep -qxF "Publish run: https://example.invalid/runs/41" "$1"' _ "$tmp/body"
check "missing: a backtick in a log line cannot close the fence" test "$(grep -c '```' "$tmp/body")" = 2
check "missing: no escape byte reaches the issue" bash -c '! grep -q "$(printf "\033")" "$1"' _ "$tmp/body"

# Reuse: the workflow's own open issue gets the comment.
printf '%s\t%s\t%s\n' 1500 "$title" mallory 1316 "$title" "" >"$tmp/issues"
miss 0 "a gap, the workflow's issue open" &&
	check "missing, reuse: a comment on #1316 and nothing else" test "$(cat "$tmp/filed") $(grep -c '^WRITE' "$tmp/gh.log")" = "comment	1316 1"

# The check did not finish: nothing recorded as missing.
: >"$tmp/logs/verify.log"
: >"$tmp/logs/cosign.log"
: >"$tmp/issues"
miss 0 "a run that was cancelled before it could say" VERIFY_CONCLUSION=cancelled COSIGN_CONCLUSION= &&
	check "unfinished: the body says the commits are UNVERIFIED" body_has "so the commits it was to check are UNVERIFIED"
check "unfinished: ... and how far it got" body_has "(none recorded — the check did not finish: gate cancelled, signatures not run; see the run log)"
check "unfinished: ... and does not claim artifacts are missing" body_lacks "is missing artifacts in the image registry"
rm -f "$tmp/logs/verify.log" "$tmp/logs/cosign.log"
miss 0 "no log at all" &&
	check "no logs: filed as unfinished" body_has "the check did not finish"

# A sweep that finds everything missing: GitHub refuses a body over 65,536
# characters, and a report that is refused is no report. The quoted lines are
# cut to fit, and the body says how many were cut.
for i in $(seq 1 300); do printf '  MISSING  quay.io/acme/img-%03d:%s\n' "$i" "$(printf 'x%.0s' $(seq 1 380))"; done >"$tmp/logs/verify.log"
for i in $(seq 1 300); do printf '  FAILED   quay.io/acme/sig-%03d@%s\n' "$i" "$(printf 'y%.0s' $(seq 1 380))"; done >"$tmp/logs/cosign.log"
printf '  FAILED   short-last-line\n' >>"$tmp/logs/cosign.log"
miss 0 "six hundred long lines and a short one" &&
	check "long report: the body fits GitHub's limit with room to spare" test "$(wc -m <"$tmp/body")" -lt 60000
kept="$(grep -cE '^(MISSING|UNVERIFIED) ' "$tmp/body")"
cut_n="$(sed -nE 's/^\(([0-9]+) more line\(s\) not shown: .*/\1/p' "$tmp/body")"
check "long report: it says how many lines were cut, and none is unaccounted for" test "$kept ${cut_n:-0} $((kept + ${cut_n:-0}))" = "$kept $((601 - kept)) 601"
check "long report: ... and some were kept, the first ones" bash -c '[ "$1" -gt 50 ] && grep -q "^MISSING quay.io/acme/img-001:" "$2"' _ "$kept" "$tmp/body"
# What is shown is the start of the list, not whichever later lines happen to fit.
check "long report: a short line after the cut is not slipped in" body_lacks "short-last-line"
check "long report: the fence still closes and the runs are still named" bash -c '[ "$(grep -c "\`\`\`" "$1")" = 2 ] && grep -qxF "Verification run: https://example.invalid/runs/42" "$1"' _ "$tmp/body"
# A report that fits is not cut and says nothing about cutting.
printf '%s\n' '  MISSING  quay.io/acme/agent:1.0.0-01391e0' >"$tmp/logs/verify.log"
: >"$tmp/logs/cosign.log"
miss 0 "one line" &&
	check "short report: nothing is cut" body_lacks "not shown"
rm -f "$tmp/logs/verify.log" "$tmp/logs/cosign.log"

# A label that is gone, and an issue API that does not answer: the step fails.
for mode in reject drop; do
	SEED_LABELS=bug miss 1 "missing: a label is gone (GitHub would ${mode} it)" FAKE_LABELS="$mode" &&
		check "missing, label gone (${mode}): the issue is filed all the same" test "$(cat "$tmp/filed")" = "create	${title}"
done
miss 1 "missing: the issue API is down while filing" FAKE_FAIL="create comment"
miss 1 "missing: the listing is down while filing" FAKE_FAIL=list &&
	check "missing, listing down: nothing is filed blind" test ! -s "$tmp/filed"

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
file 1 "the issue API is down while filing" FAKE_FAIL="create comment"
: >"$tmp/issues"
file 1 "the listing is down while filing" FAKE_FAIL=list &&
	check "listing down: nothing is filed blind" test ! -s "$tmp/filed"

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS check(s) failed"
	exit 1
fi
echo "all checks passed"
