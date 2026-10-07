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
#   scripts/publish-verify-close-issue.sh decide <event> <commit> <main head> \
#       <superseded: true|false|""> <gate conclusion> <signatures conclusion>
#     prints `close` or `keep: <why>` (pure; the test drives this)
#   scripts/publish-verify-close-issue.sh
#     the workflow step. Reads GITHUB_EVENT_NAME, TARGET, SUPERSEDED,
#     VERIFY_CONCLUSION, COSIGN_CONCLUSION and RUN_URL; resolves main's head
#     with `git ls-remote origin` (MAIN_HEAD overrides); needs GH_TOKEN with
#     `issues: write` and GH_REPO, the same as the step that opens the issue.
#     Closes every OPEN issue whose title is exactly ISSUE_TITLE with a one-line
#     comment naming the run. No open issue is the normal case and says so.
#
# NEVER FAILS THE RUN. A green verification must not go red because the issue
# API hiccuped: a failed listing or close is a ::warning::, exit 0, and the
# next green run tries again.
#
# Test: scripts/publish_verify_close_issue_test.sh (//scripts:publish_verify_close_issue_test).
set -euo pipefail

# The title the "Open or update the missing-artifacts issue" step of
# .github/workflows/publish-verify.yaml files under. The test reads that file
# and fails if the two ever differ.
ISSUE_TITLE="publish: artifacts missing for a commit on main"

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
[ "$verdict" = close ] || exit 0

if [ "$event" = schedule ]; then
	what="the scheduled sweep verified every push head it covers"
else
	what="main's head ${target:0:12} is published and its signatures verify"
fi
comment="Closed by a green publish-verify run: ${what}. ${RUN_URL:-run URL not recorded}"

# `in:title` is a word search, so match the title exactly before touching
# anything: an issue that merely quotes it is not this one.
if ! numbers="$(gh issue list --state open --search "in:title \"${ISSUE_TITLE}\"" -L 20 \
	--json number,title -q ".[] | select(.title == \"${ISSUE_TITLE}\") | .number")"; then
	echo "::warning title=publish-verify::could not list open issues, so \"${ISSUE_TITLE}\" was not closed; the next green run will try again"
	exit 0
fi
if [ -z "$numbers" ]; then
	echo "no open \"${ISSUE_TITLE}\" issue; nothing to close"
	exit 0
fi
while read -r num; do
	[[ "$num" =~ ^[0-9]+$ ]] || continue
	if gh issue close "$num" --reason completed --comment "$comment"; then
		echo "closed #${num}"
	else
		echo "::warning title=publish-verify::could not close #${num}; the next green run will try again"
	fi
done <<<"$numbers"
