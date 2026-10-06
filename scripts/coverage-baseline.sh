#!/usr/bin/env bash
# Find the coverage baseline a pull request is compared with, and download it.
# docs/runbook.md, "Code coverage". Run by the `gate` job of
# .github/workflows/coverage.yaml, ahead of scripts/coverage-compare.sh.
#
#   scripts/coverage-baseline.sh --base-sha <sha> --out <dir> [--wait-seconds <n>]
#
# The baseline is the `coverage-report` artifact of a SUCCESSFUL run of the
# coverage workflow ON MAIN: event `push` or `workflow_dispatch`, branch main,
# this repository (never a fork's run of a branch that happens to be called
# main). Chosen in this order:
#
#   1. exact   the run for --base-sha, the pull request's base commit. The
#              ruleset keeps a branch up to date with main, so this is normally
#              main's head, and the delta is then the pull request's own. When
#              that run is still queued or in progress (the pull request was
#              pushed just after a merge), it is waited for, up to
#              --wait-seconds (default 0).
#   2. latest  otherwise the most recent such run whose artifact still exists,
#              whatever its commit. This is what a stacked pull request gets:
#              its base is the `upgrade/**` branch beneath it, a commit main
#              never had, and the stack's eventual target is main, so each
#              member is measured, cumulatively with the members beneath it,
#              against main's latest. It is also what a pull request gets
#              whose base commit was never measured (its run failed).
#   3. none    exit 1 with an actionable `::error`. Never a pass: a gate that
#              passes when it has nothing to compare with is not a gate.
#
# stdout: `key=value` lines for $GITHUB_OUTPUT:
#   sha=<the baseline's commit>  run_id=<n>  run_url=<url>  match=exact|latest
# The artifact's files land in --out (coverage.lcov among them).
#
# Needs GH_TOKEN (actions: read) and GH_REPO, `gh`, and jq (JQ overrides the
# binary, as in scripts/stuck-runs.sh).
# shellcheck disable=SC2016 # single-quoted $names here are jq variables,
# never shell expansions.
set -euo pipefail

WORKFLOW="coverage.yaml"
ARTIFACT="coverage-report"
BRANCH="main"
# How many of main's most recent successful runs are looked at for one whose
# artifact has not expired.
CANDIDATES=30
POLL_SECONDS="${COVERAGE_BASELINE_POLL_SECONDS:-20}"

JQ="${JQ:-jq}"
base_sha=""
out=""
wait_seconds=0

usage() {
	echo "usage: $0 --base-sha <sha> --out <dir> [--wait-seconds <n>]" >&2
	exit 2
}
while [ "$#" -gt 0 ]; do
	[ "$#" -ge 2 ] || usage
	case "$1" in
	--base-sha) base_sha="$2" ;;
	--out) out="$2" ;;
	--wait-seconds) wait_seconds="$2" ;;
	*) usage ;;
	esac
	shift 2
done
[ -n "$base_sha" ] && [ -n "$out" ] || usage
[[ "$wait_seconds" =~ ^[0-9]+$ ]] || usage
[[ "$POLL_SECONDS" =~ ^[0-9]+$ ]] || usage
: "${GH_REPO:?GH_REPO must be set (owner/repo)}"

# Runs of the coverage workflow on main that can be a baseline, newest first,
# as "<id>\t<status>\t<conclusion>\t<head_sha>\t<html_url>". $1: extra query.
runs() {
	gh api "repos/$GH_REPO/actions/workflows/$WORKFLOW/runs?branch=$BRANCH&per_page=$CANDIDATES$1" |
		"$JQ" -r --arg repo "$GH_REPO" --arg branch "$BRANCH" '
			.workflow_runs[]
			| select(.head_branch == $branch)
			| select(.event == "push" or .event == "workflow_dispatch")
			| select(.head_repository.full_name == $repo)
			| [.id, .status, (.conclusion // ""), .head_sha, .html_url] | @tsv'
}

# Whether the run still has the artifact (artifacts expire; main's are kept 90
# days by the workflow).
has_artifact() { # run id
	local n
	n="$(gh api "repos/$GH_REPO/actions/runs/$1/artifacts?name=$ARTIFACT" |
		"$JQ" --arg name "$ARTIFACT" '[.artifacts[] | select(.name == $name and .expired == false)] | length')"
	[ "$n" -gt 0 ]
}

chosen=""
match=""

# 1. The base commit's own run.
waited=0
while :; do
	exact="$(runs "&head_sha=$base_sha")"
	done_run="$(awk -F '\t' '$2 == "completed" && $3 == "success" { print; exit }' <<<"$exact")"
	if [ -n "$done_run" ]; then
		if has_artifact "$(cut -f1 <<<"$done_run")"; then
			chosen="$done_run"
			match="exact"
		else
			echo "coverage-baseline: the run for the base commit $base_sha succeeded but its artifact has expired" >&2
		fi
		break
	fi
	pending="$(awk -F '\t' '$2 != "completed" { print $1; exit }' <<<"$exact")"
	if [ -z "$pending" ] || [ "$waited" -ge "$wait_seconds" ]; then
		if [ -n "$pending" ]; then
			echo "coverage-baseline: main's coverage run $pending for the base commit $base_sha has not finished after ${waited}s" >&2
		else
			echo "coverage-baseline: no successful coverage run on $BRANCH for the base commit $base_sha" >&2
		fi
		break
	fi
	echo "coverage-baseline: waiting for main's coverage run $pending for the base commit $base_sha (${waited}s of ${wait_seconds}s)" >&2
	sleep "$POLL_SECONDS"
	waited=$((waited + POLL_SECONDS))
done

# 2. Main's most recent successful run that still has its artifact.
if [ -z "$chosen" ]; then
	while IFS= read -r candidate; do
		[ -n "$candidate" ] || continue
		if has_artifact "$(cut -f1 <<<"$candidate")"; then
			chosen="$candidate"
			match="latest"
			break
		fi
	done < <(runs "&status=success" | awk -F '\t' '$2 == "completed" && $3 == "success"')
fi

# 3. Nothing to compare with.
if [ -z "$chosen" ]; then
	echo "::error title=Coverage gate: no baseline::no successful run of the coverage workflow on $BRANCH still has a '$ARTIFACT' artifact (they are kept 90 days), so there is nothing to compare this pull request with and the gate cannot pass. Re-run the coverage workflow on $BRANCH (Actions > coverage > Run workflow, or: gh workflow run $WORKFLOW --ref $BRANCH), wait for it to succeed, then re-run this job. docs/runbook.md, 'Code coverage'." >&2
	exit 1
fi

IFS=$'\t' read -r run_id _ _ sha url <<<"$chosen"
mkdir -p "$out"
gh run download "$run_id" --repo "$GH_REPO" --name "$ARTIFACT" --dir "$out"
[ -f "$out/coverage.lcov" ] || {
	echo "::error title=Coverage gate: no baseline::the '$ARTIFACT' artifact of run $run_id has no coverage.lcov" >&2
	exit 1
}
echo "coverage-baseline: $match baseline: $BRANCH at $sha (run $run_id)" >&2

echo "sha=$sha"
echo "run_id=$run_id"
echo "run_url=$url"
echo "match=$match"
