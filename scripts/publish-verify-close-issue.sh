#!/usr/bin/env bash
# Close publish-verify's rolling "artifacts missing" issue once a run proves
# there is no gap (#1316).
#
# publish-verify opens ONE issue, "publish: artifacts missing for a commit on
# main", and reuses it for consecutive failures. Nothing closed it: run
# 37522284996 filed #1316 for a single 502 from the registry, the next three
# runs were green, and the issue stayed open saying UNVERIFIED. An open alarm
# for a gap that no longer exists is how the next real one gets ignored, so a
# green run closes it and names itself.
#
# WHICH GREEN RUNS CLOSE IT. Only one that verified what main ships now:
#   - the scheduled sweep (`schedule`): every push head since the last green
#     sweep, so it covers whatever a failed run left unverified;
#   - a `workflow_run` whose commit IS main's head when it finishes.
# and only when the gate and the signature pass both concluded `success`.
# Everything else keeps the issue open:
#   - a run that skipped its commit as superseded verified nothing
#     (scripts/publish-verify-superseded.sh);
#   - a `workflow_run` for a commit main has moved past says nothing about the
#     head (that head's own run, or the next sweep, closes it);
#   - `workflow_dispatch`: a person checking one commit by hand, which never
#     opens the issue either;
#   - main's head unknown, or a malformed sha (fail closed: keep).
#
# THE CONTROL'S ISSUES TOO (#1340). The expected-red control has rolling
# issues of its own, one per way it can fail
# (scripts/publish-verify-control-issue.sh). #1340 was filed for one 502 from
# the registry and nothing closed it either. They close on a different rule,
# because the control does not depend on the commit under verification: ANY
# `workflow_run` or `schedule` run whose control recorded `ok` -- it went red
# as expected -- closes every open control issue, whatever that run's gate
# decided about the artifacts issue (a superseded commit, an older commit).
# The verdict is read from the file the control wrote; no verdict on record
# keeps them open (fail closed). The step this script runs in is gated on
# `success()`, so it does not run at all when the control failed.
#
#   scripts/publish-verify-close-issue.sh decide <event> <commit> <main head> \
#       <superseded: true|false|""> <gate conclusion> <signatures conclusion>
#     prints `close` or `keep: <why>` (pure; the test drives this)
#   scripts/publish-verify-close-issue.sh decide-control <event> <control verdict>
#     the same for the control's issues (pure)
#   scripts/publish-verify-close-issue.sh
#     the workflow step. Reads GITHUB_EVENT_NAME, TARGET, SUPERSEDED,
#     VERIFY_CONCLUSION, COSIGN_CONCLUSION and RUN_URL; resolves main's head
#     with `git ls-remote origin` (MAIN_HEAD overrides); needs GH_TOKEN with
#     `issues: write` and GH_REPO, the same as the step that opens the issue.
#     Closes every OPEN issue whose title is exactly ISSUE_TITLE with a one-line
#     comment naming the run. No open issue is the normal case and says so.
#     Only an issue this workflow's token opened: one that somebody else filed
#     under the title is theirs, and stays open (#1532;
#     scripts/rolling-issue-lib.sh, which also lists instead of searching).
#     Then the control's issues, by the recorded verdict
#     (`publish-verify-control-issue.sh verdict` / `titles`).
#
# NEVER FAILS THE RUN. A green verification must not go red because the issue
# API hiccuped: a failed listing or close is a ::warning::, exit 0, and the
# next green run tries again.
#
# Test: scripts/publish_verify_close_issue_test.sh (//scripts:publish_verify_close_issue_test).
set -euo pipefail

# The title the "Open or update the missing-artifacts issue" step of
# .github/workflows/publish-verify.yaml files under: its script,
# scripts/publish-verify-missing-issue.sh, asks this one for it (`title`).
ISSUE_TITLE="publish: artifacts missing for a commit on main"

# How every closing comment of this script starts: what tells a closing
# comment from a report when the comments are counted (close_titled).
CLOSING_PREFIX="Closed by a "

# shellcheck source=scripts/rolling-issue-lib.sh
. "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/rolling-issue-lib.sh"

is_sha() {
	case "$1" in
	*[!0-9a-f]* | "") return 1 ;;
	esac
	[ "${#1}" -eq 40 ]
}

decide() {
	local event="$1" commit="$2" head="$3" superseded="$4" gate="$5" signatures="$6"
	if [ "$gate" != success ] || [ "$signatures" != success ]; then
		echo "keep: the run did not verify anything (gate ${gate:-not run}, signatures ${signatures:-not run})"
	elif [ "$superseded" = true ]; then
		echo "keep: the commit was skipped as superseded, not verified"
	elif [ "$event" = schedule ]; then
		echo close
	elif [ "$event" != workflow_run ]; then
		echo "keep: a ${event:-<no event>} run never opens or closes the issue"
	elif ! is_sha "$commit" || ! is_sha "$head"; then
		echo "keep: cannot tell whether ${commit:-<no commit>} is main's head (${head:-<unknown>})"
	elif [ "$commit" != "$head" ]; then
		echo "keep: ${commit:0:12} is no longer main's head (${head:0:12})"
	else
		echo close
	fi
}

# The control's issues: close on any automatic run whose control went red as
# expected. <verdict> is what the control recorded (ok | green | wrong |
# inconclusive | unknown).
decide_control() {
	local event="$1" verdict="$2"
	if [ "$event" != workflow_run ] && [ "$event" != schedule ]; then
		echo "keep: a ${event:-<no event>} run never opens or closes the issue"
	elif [ "$verdict" != ok ]; then
		echo "keep: this run's control did not go red as expected (recorded: ${verdict:-nothing})"
	else
		echo close
	fi
}

# close_titled <title> <comment>: close every OPEN issue of the workflow's own
# whose title is exactly <title>. Never fails: an issue-API error is a
# ::warning::.
#
# AT THE SAME TIME. publish-verify runs overlap (one concurrency group per
# commit), so a run that failed can write on the issue while this one is
# closing it, and GitHub takes a comment on a closed issue: the failure would
# sit where nobody looks. The reporter reads the issue's state after its
# comment and reopens it (rolling_issue_report), which covers a close that
# landed BEFORE that reading. For one that lands after it, this side counts
# the reports on the issue before it writes anything and again after its
# close, and reopens if one was added in between. A report written between
# the two counts is seen by this side; one written before the first count was
# there when this run decided to close, as before. Another green run's closing
# comment is not a report (every closing comment starts with CLOSING_PREFIX).
# If the reports cannot be counted first, the issue is not closed.
close_titled() {
	local title="$1" comment="$2" numbers num
	# Exactly the title, and opened by the workflow token: an issue that merely
	# quotes the title is not this one, and neither is one a person filed.
	if ! numbers="$(rolling_issue_list open "$title")"; then
		echo "::warning title=publish-verify::could not list open issues, so \"${title}\" was not closed; the next green run will try again"
		return 0
	fi
	if [ -z "$numbers" ]; then
		echo "no open \"${title}\" issue; nothing to close"
		return 0
	fi
	local before after
	while read -r num; do
		[[ "$num" =~ ^[0-9]+$ ]] || continue
		if ! before="$(rolling_issue_reports "$num" "$CLOSING_PREFIX")"; then
			echo "::warning title=publish-verify::could not read #${num}, so it was not closed; the next green run will try again"
			continue
		fi
		if rolling_issue_comment "$num" "$comment" && rolling_issue_close "$num" completed; then
			echo "closed #${num}"
		else
			echo "::warning title=publish-verify::could not close #${num}; the next green run will try again"
			continue
		fi
		if ! after="$(rolling_issue_reports "$num" "$CLOSING_PREFIX")"; then
			echo "::warning title=publish-verify::closed #${num}, but could not read it back: check that no failure was written on it meanwhile"
		elif [ "$after" -gt "$before" ]; then
			if rolling_issue_reopen "$num"; then
				echo "reopened #${num}: a failure was written on it while it was being closed"
			else
				echo "::warning title=publish-verify::a failure was written on #${num} while it was being closed, and it could not be reopened: reopen it by hand"
			fi
		fi
	done <<<"$numbers"
}

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ "${1:-}" = decide-control ]; then
	[ "$#" -eq 3 ] || {
		echo "usage: $0 decide-control <event> <control verdict>" >&2
		exit 2
	}
	decide_control "$2" "$3"
	exit 0
fi
if [ "${1:-}" = decide ]; then
	[ "$#" -eq 7 ] || {
		echo "usage: $0 decide <event> <commit> <main head> <superseded> <gate conclusion> <signatures conclusion>" >&2
		exit 2
	}
	decide "$2" "$3" "$4" "$5" "$6" "$7"
	exit 0
fi
if [ "${1:-}" = title ]; then
	printf '%s\n' "$ISSUE_TITLE"
	exit 0
fi

event="${GITHUB_EVENT_NAME:-}"
target="${TARGET:-}"
head="${MAIN_HEAD:-}"
if [ -z "$head" ] && [ "$event" = workflow_run ]; then
	head="$(git ls-remote origin refs/heads/main 2>/dev/null | cut -f1)" || head=""
fi

verdict="$(decide "$event" "$target" "$head" "${SUPERSEDED:-}" "${VERIFY_CONCLUSION:-}" "${COSIGN_CONCLUSION:-}")"
echo "event ${event:-<none>}, commit ${target:-<none>}, main head ${head:-<not needed>}: ${verdict}"
if [ "$verdict" = close ]; then
	if [ "$event" = schedule ]; then
		what="the scheduled sweep verified every push head it covers"
	else
		what="main's head ${target:0:12} is published and its signatures verify"
	fi
	close_titled "$ISSUE_TITLE" "Closed by a green publish-verify run: ${what}. ${RUN_URL:-run URL not recorded}"
fi

# The expected-red control's issues (#1340). A verdict or a title list that
# cannot be read keeps them open.
control="$("${here}/publish-verify-control-issue.sh" verdict)" || control=""
control_verdict="$(decide_control "$event" "$control")"
echo "expected-red control (recorded: ${control:-nothing}): ${control_verdict}"
[ "$control_verdict" = close ] || exit 0
if ! titles="$("${here}/publish-verify-control-issue.sh" titles)" || [ -z "$titles" ]; then
	echo "::warning title=publish-verify::could not read the control's issue titles; its issues were not closed"
	exit 0
fi
while read -r control_title; do
	[ -n "$control_title" ] || continue
	close_titled "$control_title" "Closed by a publish-verify run whose expected-red control went red as expected: the gate was shown to fail on a never-published commit. ${RUN_URL:-run URL not recorded}"
done <<<"$titles"
