#!/usr/bin/env bash
# The step of the `report-failure` job of .github/workflows/e2e.yaml: a nightly
# e2e run that failed is written on ONE rolling issue, "Nightly e2e failing".
#
# A scheduled run that fails notifies nobody. The suite sat red for three
# nights (07-23..07-25) before anyone noticed, so the job files an issue, and
# reuses the open one rather than filing one per night: three duplicates are
# how people learn to ignore them.
#
# This was inline in the workflow, and it found "the open one" with a title
# search that took the first hit: any open issue whose title held those words,
# whoever opened it (#1532). Now the issue is the one the workflow's own token
# opened under exactly this title, found by listing, and a new one is opened
# with ISSUE_LABELS (#1568): scripts/rolling-issue-lib.sh has the reasons and
# the rules, the same as for every other rolling issue.
#
# Usage:
#   scripts/e2e-report-failure.sh          the job's step
#   scripts/e2e-report-failure.sh title    the issue title
#   scripts/e2e-report-failure.sh body     the comment it would write (no `gh`)
#
# Environment:
#   RESULTS             one `<job>=<result>` per line, from `needs.*.result`
#   RUN_URL             the failed run
#   GH_TOKEN, GH_REPO   for `gh` (issues: write)
#
# Exit 0 when the failure is on the issue. Exit 1 when it could not be filed,
# or was filed on an issue opened without one of its labels: the job that
# exists to report must not look clean when it did not.
#
# Tests: scripts/e2e_report_failure_test.sh (//scripts:e2e_report_failure_test).
set -euo pipefail

ISSUE_TITLE="Nightly e2e failing"
# Labels the repository has; AGENTS.md asks a kind and an area of every issue.
ISSUE_LABELS=(bug ci)

# shellcheck source=scripts/rolling-issue-lib.sh
. "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/rolling-issue-lib.sh"

# The jobs that failed, each in a code span. A job name is the workflow's own,
# but it is reduced to plain characters all the same before it is quoted.
body() {
	local failed
	failed="$(awk -F= '$2 == "failure" { gsub(/[^A-Za-z0-9_.-]/, "", $1); if ($1 != "") printf "`%s` ", $1 }' <<<"${RESULTS:-}")"
	[ -n "$failed" ] || failed="(no job reported failure — check the run)"
	printf '%s\n\n%s\n\n%s\n' \
		"Failed jobs: ${failed}" \
		"Run: ${RUN_URL:-run URL not recorded}" \
		"_Filed automatically by the \`report-failure\` job of \`e2e\` (.github/workflows/e2e.yaml); this issue is reused for consecutive failures. Close it by hand once a nightly run is green._"
}

case "${1:-}" in
title)
	printf '%s\n' "$ISSUE_TITLE"
	exit 0
	;;
body)
	body
	exit 0
	;;
"") ;;
*)
	echo "usage: $0 [title | body]" >&2
	exit 2
	;;
esac

rc=0
rolling_issue_report "$ISSUE_TITLE" "$(body)" "${ISSUE_LABELS[@]}" || rc=$?
case "$rc" in
0) ;;
3)
	echo "::error title=e2e report-failure::the issue was filed without its labels (${ISSUE_LABELS[*]})" >&2
	exit 1
	;;
*)
	echo "::error title=e2e report-failure::the nightly failure could not be filed" >&2
	exit 1
	;;
esac
