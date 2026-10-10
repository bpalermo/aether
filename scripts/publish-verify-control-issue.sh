#!/usr/bin/env bash
# File publish-verify's expected-red control issue, saying WHICH failure it was
# (#1340).
#
# The control (scripts/publish-verify-control.sh) hands the verifier a commit
# that was never published and expects it back red. It can end three other
# ways, and they are not the same event:
#
#   green         the verifier PASSED that commit (exit 0). A real defect: the
#                 gate is vacuous, and a green publish-verify proves nothing.
#   wrong         the verifier went red, but not for the right reason (a count
#                 off, a line about another commit or another registry, an
#                 absence with no witness). Part of the gate cannot fail.
#   inconclusive  the control could not reach a verdict: the registry could not
#                 be read (after the control's own re-runs), or the control
#                 commit could not be built. Says nothing about the gate.
#
# The workflow used to file all of them as "the expected-red control did not go
# red", with a body that said "or could not read the registry at all". Run
# 37586580592 filed #1340 that way for one 502 from the registry: an alarm
# worded as a broken gate, for a transient. So each case has its own title and
# its own first paragraph, and the body quotes what the control itself said.
#
# The control records its verdict and its summary in a file (first line the
# verdict, then the summary); this script and the closing half,
# scripts/publish-verify-close-issue.sh, read it. A run whose control goes red
# as expected (`ok`) closes every open control issue.
#
#   scripts/publish-verify-control-issue.sh
#     the workflow step ("Open or update the vacuous-gate issue"), which runs
#     only when the control step failed. Needs GH_TOKEN with `issues: write`,
#     GH_REPO and RUN_URL. Comments on the open issue with that case's exact
#     title, or opens one. No verdict on record (the control was killed before
#     it wrote one) is filed under the old, general title.
#     "The open issue" is the one this workflow's token opened, found by
#     listing: an issue anyone else filed under the title is not written to
#     (#1532; scripts/rolling-issue-lib.sh). A new issue is opened with
#     ISSUE_LABELS (#1568); if a label is gone the issue is filed without it
#     and the step fails, as it does when the issue cannot be filed at all.
#   scripts/publish-verify-control-issue.sh result-file
#     where the verdict is recorded: $CONTROL_RESULT_FILE, else
#     $RUNNER_TEMP/publish-verify-control.result (GitHub Actions), else nothing
#     (a run by hand records nothing).
#   scripts/publish-verify-control-issue.sh verdict
#     the recorded verdict: ok | green | wrong | inconclusive | unknown
#   scripts/publish-verify-control-issue.sh title <verdict>
#   scripts/publish-verify-control-issue.sh titles
#     every title a control issue can have, one per line (what a good run closes)
#   scripts/publish-verify-control-issue.sh body <verdict>
#     the issue body for that verdict, from the recorded summary and RUN_URL
#
# Test: scripts/publish_verify_close_issue_test.sh (//scripts:publish_verify_close_issue_test).
set -euo pipefail

# The title every case was filed under before #1340, and still the one for a
# control that left no verdict. Kept in `titles` so an issue filed under it is
# closed by the next good run like the others.
TITLE_UNKNOWN="publish-verify: the expected-red control did not go red"
TITLE_GREEN="publish-verify: the expected-red control went GREEN"
TITLE_WRONG="publish-verify: the expected-red control went red for the wrong reason"
TITLE_INCONCLUSIVE="publish-verify: the expected-red control was inconclusive"
# Labels the repository has; AGENTS.md asks a kind and an area of every issue.
ISSUE_LABELS=(bug ci)

# shellcheck source=scripts/rolling-issue-lib.sh
. "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/rolling-issue-lib.sh"

result_file() {
	if [ -n "${CONTROL_RESULT_FILE:-}" ]; then
		printf '%s\n' "$CONTROL_RESULT_FILE"
	elif [ -n "${RUNNER_TEMP:-}" ]; then
		printf '%s\n' "${RUNNER_TEMP}/publish-verify-control.result"
	fi
}

# The recorded verdict. Anything that is not one of the four words the control
# writes -- no file, an empty one, a stray line -- is `unknown`, never a guess.
verdict() {
	local file v=""
	file="$(result_file)"
	if [ -n "$file" ] && [ -r "$file" ]; then
		v="$(sed -n 1p "$file")"
	fi
	case "$v" in
	ok | green | wrong | inconclusive) printf '%s\n' "$v" ;;
	*) printf 'unknown\n' ;;
	esac
}

# What the control said, for the body: the lines after the verdict, at most 20
# of them and 400 bytes each, with nothing unprintable and no backtick (the
# text is quoted in a code fence, which a backtick run could close).
summary() {
	local file
	file="$(result_file)"
	[ -n "$file" ] && [ -r "$file" ] || return 0
	sed -n '2,$p' "$file" | tr -c '[:print:]\n' '?' | tr '`' "'" | cut -c1-400 | sed -n '1,20p'
}

title() {
	case "$1" in
	green) printf '%s\n' "$TITLE_GREEN" ;;
	wrong) printf '%s\n' "$TITLE_WRONG" ;;
	inconclusive) printf '%s\n' "$TITLE_INCONCLUSIVE" ;;
	unknown) printf '%s\n' "$TITLE_UNKNOWN" ;;
	*)
		echo "publish-verify-control-issue: no issue title for verdict '$1'" >&2
		return 2
		;;
	esac
}

lead() {
	case "$1" in
	green)
		printf '%s\n' "**The control went GREEN: a real defect in the gate.** publish-verify's expected-red control handed the verifier a commit that was never published, and the verifier PASSED it (exit 0). The gate is vacuous: until this is fixed, a green publish-verify run proves nothing (#930, #853)."
		;;
	wrong)
		printf '%s\n' "**The control went red, but for the wrong reason: a real defect in the gate.** publish-verify's expected-red control handed the verifier a commit that was never published and did not get back exit 1 with every artifact MISSING for that commit, each absence witnessed, on the registry that commit names. Part of the gate can no longer fail: until this is fixed, a green publish-verify run proves less than it reads as (#930, #853)."
		;;
	inconclusive)
		printf '%s\n' "**The control was INCONCLUSIVE: no verdict on the gate, either way.** publish-verify's expected-red control could not be completed — the registry could not be read, or the control commit could not be built — and its own re-runs did not clear it. This is NOT the control going green, and it is usually a registry outage. This issue closes itself on the next run whose control goes red as expected; while it stays open, no run has shown that the gate can fail, so a green publish-verify is unproven."
		;;
	*)
		printf '%s\n' "**The control ended without recording a verdict.** publish-verify's expected-red control step failed before it could say whether the verifier went red, green or could not be run (killed, timed out, or an error in the control itself); the run log has the rest. Until a run's control goes red as expected, a green publish-verify run is unproven (#930, #853)."
		;;
	esac
}

body() {
	local v="$1" said
	said="$(summary)"
	[ -n "$said" ] || said="(the control recorded no summary; see the run log)"
	# shellcheck disable=SC2016 # literal Markdown code fences, nothing to expand
	printf '%s\n\nWhat the control said:\n\n```\n%s\n```\n\n%s\n\n%s\n' \
		"$(lead "$v")" \
		"$said" \
		"Run: ${RUN_URL:-run URL not recorded}" \
		"_Filed automatically by \`publish-verify\`; reused for consecutive failures of the same kind, and closed by the next run whose control goes red as expected. Reproduce with \`scripts/publish-verify-control.sh\`; the offline check is \`scripts/check-publish-verify-control.sh\`._"
}

case "${1:-}" in
result-file)
	result_file
	exit 0
	;;
verdict)
	verdict
	exit 0
	;;
title)
	[ "$#" -eq 2 ] || {
		echo "usage: $0 title <green|wrong|inconclusive|unknown>" >&2
		exit 2
	}
	title "$2"
	exit
	;;
titles)
	printf '%s\n' "$TITLE_GREEN" "$TITLE_WRONG" "$TITLE_INCONCLUSIVE" "$TITLE_UNKNOWN"
	exit 0
	;;
body)
	[ "$#" -eq 2 ] || {
		echo "usage: $0 body <green|wrong|inconclusive|unknown>" >&2
		exit 2
	}
	title "$2" >/dev/null
	body "$2"
	exit 0
	;;
"") ;;
*)
	echo "usage: $0 [result-file | verdict | title <verdict> | titles | body <verdict>]" >&2
	exit 2
	;;
esac

v="$(verdict)"
if [ "$v" = ok ]; then
	# The step runs only when the control step failed, so this is a control that
	# passed and a step that failed for another reason. Not a control issue.
	echo "the expected-red control recorded 'ok' (it went red as expected); nothing to file"
	exit 0
fi
issue_title="$(title "$v")"
issue_body="$(body "$v")"
echo "expected-red control: ${v} -> \"${issue_title}\""

# The workflow's own open issue with exactly this title (the other cases'
# issues share most of its words), or a new one. The run is already red: an
# issue that could not be filed, or was filed without a label, fails this step.
rc=0
rolling_issue_report "$issue_title" "$issue_body" "${ISSUE_LABELS[@]}" || rc=$?
case "$rc" in
0) ;;
3)
	echo "::error title=publish-verify::the control issue was filed without its labels (${ISSUE_LABELS[*]})" >&2
	exit 1
	;;
*)
	echo "::error title=publish-verify::the control issue could not be filed" >&2
	exit 1
	;;
esac
