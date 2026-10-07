#!/usr/bin/env bash
# Hermetic test of scripts/publish-verify-close-issue.sh: which green
# publish-verify runs close the rolling "artifacts missing" issue (#1316), and
# -- the half that matters more -- which must leave it open. No network, no
# git: the shas are literals, MAIN_HEAD stands in for `git ls-remote`, and `gh`
# is a fake that records what it was asked to do.
#
# Run: bazel test //scripts:publish_verify_close_issue_test
# shellcheck disable=SC2016 # single-quoted $names belong to the fake's own shell.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/publish-verify-close-issue.sh"
# The workflow, to hold its title and its step to the script: a runfile under
# Bazel (//:ci_definitions), the checkout otherwise.
WORKFLOW="${TEST_SRCDIR:-}/${TEST_WORKSPACE:-_main}/.github/workflows/publish-verify.yaml"
[ -f "$WORKFLOW" ] || WORKFLOW="$HERE/../.github/workflows/publish-verify.yaml"
for f in "$SCRIPT" "$WORKFLOW"; do
	[ -f "$f" ] || {
		echo "FAIL: $f not found"
		exit 1
	}
done

old="b1dc1c04aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
head="01391e0b6e567fd56470a32dc6f04a26ce558cc0" # main's head
title="publish: artifacts missing for a commit on main"

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
	[ "$*" = "issue list --state open --search in:title \"${WANT_TITLE}\" -L 20 --json number,title -q .[] | select(.title == \"${WANT_TITLE}\") | .number" ] || {
		echo "fake gh: unexpected list arguments: $*" >&2
		exit 1
	}
	while IFS=$'\t' read -r number issue_title; do
		[ "$issue_title" = "$WANT_TITLE" ] && echo "$number"
	done <"$FAKE_ISSUES"
	exit 0
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
	env PATH="$tmp/bin:$PATH" GH_TOKEN=x GH_REPO=o/r WANT_TITLE="$title" \
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

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS check(s) failed"
	exit 1
fi
echo "all checks passed"
