#!/usr/bin/env bash
# The revision range the `diff` job of .github/workflows/ci.yaml hands to
# scripts/ci-impacted-targets.sh (#1459).
#
# On `pull_request`, GITHUB_SHA is the merge of the pull request's head into its
# base, and actions/checkout with no `ref:` checks that commit out. Every job of
# ci.yaml builds it. `diff` used to check out the pull request's HEAD instead
# and list the impacted targets there, against the tip of the base: for a pull
# request that was behind its base, a target the base had removed was handed to
# the test jobs, which failed on "no such target", and a target the base had
# added was in no list at all, so a change that broke it was not tested.
#
# The range is now read from the merge itself: its first parent (the base, as
# GitHub merged it) to the merge commit. That is what the pull request adds to
# its base, on the tree every other job builds.
#
# Refused (exit 1, no output): a checkout that is not the run's commit, and a
# merge whose second parent is not the pull request's head. A commit that is
# not a two-parent merge gets an empty base, which makes
# ci-impacted-targets.sh take its full run.
#
# Env:
#   EXPECT_SHA     the run's commit (`github.sha`)
#   PR_HEAD_SHA    the pull request's head (`github.event.pull_request.head.sha`)
#   GITHUB_OUTPUT  if set, `base` and `head` are appended
set -uo pipefail

die() {
	echo "::error::ci-merge-range: $1" >&2
	exit 1
}
full_sha() { [[ "$1" =~ ^[0-9a-f]{40}$ ]]; }

full_sha "${EXPECT_SHA:-}" || die "EXPECT_SHA is not a full commit id: '${EXPECT_SHA:-}'"
full_sha "${PR_HEAD_SHA:-}" || die "PR_HEAD_SHA is not a full commit id: '${PR_HEAD_SHA:-}'"

here="$(git rev-parse HEAD 2>/dev/null)" || die "git rev-parse HEAD failed: not a git checkout"
[ "$here" = "$EXPECT_SHA" ] ||
	die "the checkout is at ${here} and this run is for ${EXPECT_SHA}: the impacted targets would be listed on another tree than the one the test jobs build"

# "<commit> <parent>..." on one line.
read -r -a line < <(git rev-list --parents -n 1 HEAD) || die "git rev-list --parents failed for ${here}"
parents=("${line[@]:1}")

base=""
if [ "${#parents[@]}" -eq 2 ]; then
	[ "${parents[1]}" = "$PR_HEAD_SHA" ] ||
		die "${here} is the merge of ${parents[1]}, and the pull request's head is ${PR_HEAD_SHA}: it is not this pull request's merge"
	base="${parents[0]}"
	echo ">> range: ${base} (the base, first parent of the merge) .. ${here} (the merge of ${PR_HEAD_SHA})"
else
	echo "::warning::ci-merge-range: ${here} has ${#parents[@]} parent(s), so it is not the merge of a pull request; no base, falling back to a full //... run"
fi

if [ -n "${GITHUB_OUTPUT:-}" ]; then
	{
		echo "base=${base}"
		echo "head=${here}"
	} >>"$GITHUB_OUTPUT" || die "could not write to GITHUB_OUTPUT"
fi
echo "  output: base=${base}"
echo "  output: head=${here}"
