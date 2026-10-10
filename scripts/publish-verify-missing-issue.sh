#!/usr/bin/env bash
# File publish-verify's rolling "artifacts missing" issue: the step "Open or
# update the missing-artifacts issue" of .github/workflows/publish-verify.yaml.
#
# A failure of that workflow is invisible unless it is written down somewhere a
# person reads: it is attached to no pull request. ONE rolling issue, "publish:
# artifacts missing for a commit on main", reused for consecutive gaps and
# closed by the next green run (scripts/publish-verify-close-issue.sh, which
# owns the title).
#
# This was inline in the workflow, and it found "the open one" with a title
# search that took the first hit: any open issue whose title held those words,
# whoever opened it, got the comment (#1532), and a new issue had no label
# (#1568). Now the issue is the one the workflow's own token opened under
# exactly the title, found by listing, and a new one is opened with
# ISSUE_LABELS: scripts/rolling-issue-lib.sh has the rules, the same as for
# the control's issues (scripts/publish-verify-control-issue.sh).
#
# What the body says, from what the two steps before it left behind:
#   - every `MISSING` line of verify.log (the gate) and every `FAILED` line of
#     cosign.log, as `UNVERIFIED` (a signature that exists but does not verify,
#     #925);
#   - with neither, the check did not finish (a step timed out, the run was
#     cancelled, #1281): the body says so, and that the commits are UNVERIFIED.
# A line of those logs is quoted in a code fence: no backtick and nothing
# unprintable gets through, and a line is cut at 400 bytes. GitHub refuses an
# issue body or comment over 65,536 characters, and a report that is refused is
# no report: the quoted lines stop at QUOTE_LIMIT characters in all (both logs
# together), and a last line says how many were not shown.
#
# Usage:
#   scripts/publish-verify-missing-issue.sh        the workflow step
#   scripts/publish-verify-missing-issue.sh body   the body it would file (no `gh`)
#
# Environment:
#   VERIFY_CONCLUSION, COSIGN_CONCLUSION   the two steps' conclusions
#   RUN_URL, TRIGGERING_RUN                this run, and the publish run
#   LOG_DIR                                where verify.log and cosign.log are
#                                          (default: the working directory)
#   GH_TOKEN, GH_REPO                      for `gh` (issues: write)
#
# Exit 0 when the gap is on the issue. Exit 1 when it could not be filed, or
# was filed on an issue opened without one of its labels: the run is already
# red, and a report that did not land must not pass quietly.
#
# Test: scripts/publish_verify_close_issue_test.sh (//scripts:publish_verify_close_issue_test).
set -euo pipefail

here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# Labels the repository has; AGENTS.md asks a kind and an area of every issue.
ISSUE_LABELS=(bug ci)

# shellcheck source=scripts/rolling-issue-lib.sh
. "${here}/rolling-issue-lib.sh"

# Characters of quoted log lines in one body. The rest of the body is under
# 2,000, so this leaves GitHub's 65,536 well clear.
QUOTE_LIMIT=40000

# lines <file> <sed expression>: the matching lines, made safe to quote.
lines() {
	local file="${LOG_DIR:-.}/$1"
	[ -r "$file" ] || return 0
	sed -nE "$2" "$file" | tr -c '[:print:]\n' '?' | tr '`' "'" | cut -c1-400
}

# fit: whole lines from stdin while they fit in QUOTE_LIMIT characters, then
# one line saying how many were left out.
fit() {
	awk -v limit="$QUOTE_LIMIT" '
		!full && used + length($0) + 1 <= limit { print; used += length($0) + 1; next }
		{ full = 1; cut++ }
		END { if (cut) printf "(%d more line(s) not shown: an issue body has a size limit; the run log has every line)\n", cut }'
}

body() {
	local missing lead
	missing="$({
		lines verify.log 's/^[[:space:]]*MISSING[[:space:]]+/MISSING /p'
		lines cosign.log 's/^[[:space:]]*FAILED[[:space:]]+/UNVERIFIED /p'
	} | { grep . || true; } | fit)"
	lead="A commit on \`main\` is missing artifacts in the image registry (bazel/registry/registry.bzl) — never published, or published only in part. A deploy pinned to it will 404, and \`helm upgrade\` against its commit-suffixed chart tag cannot work."
	if [ -z "$missing" ]; then
		# Nothing recorded as missing: the check did not finish (#1281).
		missing="(none recorded — the check did not finish: gate ${VERIFY_CONCLUSION:-not run}, signatures ${COSIGN_CONCLUSION:-not run}; see the run log)"
		lead="publish-verify did not finish (a step timed out, the run was cancelled, or a lookup was inconclusive), so the commits it was to check are UNVERIFIED — a gap here would have gone unreported."
	fi
	# shellcheck disable=SC2016 # literal Markdown code fences, nothing to expand
	printf '%s\n\n```\n%s\n```\n\n%s\n%s\n\n%s\n' \
		"$lead" \
		"$missing" \
		"Verification run: ${RUN_URL:-run URL not recorded}" \
		"Publish run: ${TRIGGERING_RUN:-n/a}" \
		"_Filed automatically by \`publish-verify\`; this issue is reused for consecutive gaps. To republish: re-run the cancelled publish run (\`gh run rerun <id>\`), which re-runs at that same commit. Never push images or charts by hand — the release workflow is the only publisher. See docs/runbook.md, \"Pre-flight: did that commit actually publish?\"._"
}

case "${1:-}" in
body)
	body
	exit 0
	;;
"") ;;
*)
	echo "usage: $0 [body]" >&2
	exit 2
	;;
esac

title="$("${here}/publish-verify-close-issue.sh" title)"
[ -n "$title" ] || {
	echo "::error title=publish-verify::could not read the issue title from publish-verify-close-issue.sh" >&2
	exit 1
}
rc=0
rolling_issue_report "$title" "$(body)" "${ISSUE_LABELS[@]}" || rc=$?
case "$rc" in
0) ;;
3)
	echo "::error title=publish-verify::the missing-artifacts issue was filed without its labels (${ISSUE_LABELS[*]})" >&2
	exit 1
	;;
*)
	echo "::error title=publish-verify::the missing-artifacts issue could not be filed" >&2
	exit 1
	;;
esac
