#!/usr/bin/env bash
#
# Find GitHub Actions runs that are stuck before they ever ran, report them, and
# cancel the ones that are clearly safe to cancel.
#
# WHY
#
# publish.yaml (group publish-main) and pages.yaml (group pages) serialise with
# `cancel-in-progress: false`, and GitHub holds at most ONE pending run per
# concurrency group. So one run whose job is never assigned a runner holds the
# group forever: every later run shows `pending` with zero jobs, and each newer
# one replaces the previous pending one as `cancelled`. On 2026-10-05 a publish
# run for b1dc1c04 sat `queued` for five hours and blocked eight publishes; a
# pages run queued since 2026-10-02 blocked every website deploy for three days.
# Neither was noticed. A job `timeout-minutes` does not help: it starts counting
# when the job starts, and these never started.
#
# WHAT IT DOES
#
# A run is STUCK when, for longer than the threshold (STUCK_THRESHOLD_MINUTES,
# default 60; a publish or proxy-release run takes ~10 minutes, so an hour is
# six of them):
#   - its status is pending, waiting or requested (measured from the start of
#     its current attempt, run_started_at, so a re-run is judged by its own
#     date); or
#   - it is queued or in_progress and one of its jobs has sat `queued` — no
#     runner — that long (measured from the JOB's creation, so a run that waited
#     its turn behind another and then got a runner promptly is not stuck); or
#   - it is queued and has no job at all.
#
# Every stuck run is REPORTED. A stuck run is CANCELLED only when it is a
# superseded run nobody can still want, which means all of:
#   - event is `push` or `pull_request` (the run is for a commit on a branch;
#     never schedule, workflow_dispatch, workflow_run, ...);
#   - run_attempt is 1 (a re-run of an old commit is deliberate — it is how
#     publish-verify tells you to republish a commit);
#   - the head repository is this repository (not a fork);
#   - EITHER its branch no longer exists, OR the branch's head is no longer the
#     run's commit AND a newer run of the same workflow on that branch is queued,
#     running or has succeeded. The second half matters for path-filtered
#     workflows (pages, proxy-release): a newer commit on main does not mean a
#     newer deploy is coming, and cancelling the only one would drop it.
# The head's own run is never cancelled, however long it has been stuck: that
# is the run someone is waiting for, and it needs a human. An unknown head or an
# unread run list (a lookup failed) is reported, never cancelled.
#
# USAGE
#
#   scripts/stuck-runs.sh decide  < snapshot.json   # pure; prints the decisions
#   scripts/stuck-runs.sh collect > snapshot.json   # live; GitHub API via `gh`
#   scripts/stuck-runs.sh run [--dry-run]           # collect | decide | act
#
# The snapshot is the raw API JSON plus the branch heads:
#   { "now": "<ISO-8601>",            # optional; default: the current time
#     "repository": "<owner>/<repo>",
#     "runs": [ <GET /actions/runs objects> ],
#     "jobs": { "<run id>": [ <GET /actions/runs/<id>/jobs objects> ] },
#     "branches": { "<branch>": "<head sha>" | null },    # null: no such branch
#     "workflow_runs": { "<workflow id>:<branch>":        # the newest runs of
#                        [ <GET /actions/workflows/<id>/runs objects> ] } }
#
# `decide` prints one tab-separated line per stuck run:
#   cancel|report  id  workflow  event  branch  sha  status  age_minutes  reason  url
#
# `run` cancels the `cancel` lines (unless --dry-run), then keeps ONE rolling
# issue current (the e2e.yaml `report-failure` shape: title-deduped, reused, no
# duplicates). It comments only when the set of stuck runs changed since its
# last comment, so a run stuck for a day costs one comment, not 48; and it
# closes the issue once nothing is stuck. --dry-run cancels nothing and writes
# no issue; it prints what it would do. Needs GH_TOKEN (actions: write, issues:
# write) and GH_REPO.
#
# Exit 0 whether or not anything is stuck (the issue is the report); 2 when the
# check itself cannot run.
#
# Tests: scripts/stuck_runs_test.sh (//scripts:stuck_runs_test), canned API JSON
# in scripts/testdata/stuck-runs/.
# shellcheck disable=SC2016 # single-quoted $names here are jq variables (and
# Markdown backticks), never shell expansions.
set -euo pipefail

THRESHOLD="${STUCK_THRESHOLD_MINUTES:-60}"
JQ="${JQ:-jq}"
ISSUE_TITLE="CI: workflow runs stuck before they started"
MARKER_PREFIX="<!-- stuck-runs:"

SCRATCH="$(mktemp -d)"
trap 'rm -rf "$SCRATCH"' EXIT

# scratch <name>: a fresh directory under the one removed on exit.
scratch() {
	mkdir -p "$SCRATCH/$1"
	printf '%s\n' "$SCRATCH/$1"
}

die() {
	echo "::error::stuck-runs: $*" >&2
	exit 2
}

case "$THRESHOLD" in
'' | *[!0-9]*) die "STUCK_THRESHOLD_MINUTES must be a whole number of minutes, got '$THRESHOLD'" ;;
esac

# The decision, as one jq program over the snapshot. Pure: no clock other than
# .now (or the current time when the snapshot carries none), no network.
DECIDE='
def ts: sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601;
(if (.now // "") != "" then (.now | ts) else now end) as $now
| def mins(t): ((($now - (t | ts)) / 60) | floor);
.repository as $repo
| (.branches // {}) as $heads
| (.jobs // {}) as $jobs
| (.workflow_runs // {}) as $wfruns
| .runs[]
| . as $r
| mins($r.run_started_at // $r.created_at) as $age
| ($jobs[($r.id | tostring)] // null) as $rjobs
| ([($rjobs // [])[] | select(.status == "queued") | mins(.created_at // $r.created_at)] | max // -1) as $jobwait
| select(
    (($r.status | IN("pending", "waiting", "requested")) and $age >= $threshold)
    or (($r.status | IN("queued", "in_progress")) and $jobwait >= $threshold)
    or ($r.status == "queued" and $rjobs != null and ($rjobs | length) == 0 and $age >= $threshold)
  )
| (if $jobwait >= 0 then $jobwait else $age end) as $stuck_for
| ($r.head_branch // "") as $branch
| ($heads | has($branch)) as $known
| $heads[$branch] as $head
| ([($wfruns["\($r.workflow_id):\($branch)"] // [])[]
    | select(.id != $r.id)
    | select((.created_at | ts) > ($r.created_at | ts))
    | select(.status != "completed" or .conclusion == "success")
  ] | sort_by(.created_at) | last) as $newer
| (if ($r.event | IN("push", "pull_request") | not) then
     {a: "report", why: "event \($r.event): only push/pull_request runs are ever cancelled"}
   elif ($r.run_attempt // 1) != 1 then
     {a: "report", why: "re-run (attempt \($r.run_attempt)): deliberate, never cancelled"}
   elif (($r.head_repository.full_name // "") != $repo) then
     {a: "report", why: "head repository \($r.head_repository.full_name // "unknown") is not \($repo)"}
   elif ($branch == "" or ($known | not)) then
     {a: "report", why: "head of \($branch) unknown: not cancelled"}
   elif $head == null then
     {a: "cancel", why: "superseded: branch \($branch) no longer exists"}
   elif $head == $r.head_sha and $r.status == "pending" then
     {a: "report", why: "the head of \($branch), waiting on its concurrency group: it starts when the run holding the group ends"}
   elif $head == $r.head_sha then
     {a: "report", why: "the head of \($branch): never cancelled, needs a human"}
   elif $newer == null then
     {a: "report", why: "\($branch) has moved on (head \($head[0:8])) but no newer \($r.name // "") run on it will replace this one: not cancelled"}
   else
     {a: "cancel", why: "superseded: \($branch) head is now \($head[0:8]); newer run \($newer.id) is \($newer.conclusion // $newer.status)"}
   end) as $d
| [ $d.a, ($r.id | tostring), ($r.name // $r.path // "?"), $r.event, $branch, $r.head_sha,
    (if $jobwait >= $threshold then "\($r.status), a job queued" else $r.status end),
    ($stuck_for | tostring), $d.why, ($r.html_url // "") ]
| map(gsub("[\t\n]"; " "))
| @tsv
'

cmd_decide() {
	"$JQ" -r --argjson threshold "$THRESHOLD" "$DECIDE"
}

urlencode() {
	"$JQ" -rn --arg v "$1" '$v | @uri'
}

# gh api with the status kept apart from the body: prints the body, returns 0;
# returns 4 on a 404 and 1 on anything else.
api_get() {
	local out
	if out="$(gh api "$1" 2>&1)"; then
		printf '%s\n' "$out"
		return 0
	fi
	case "$out" in
	*"HTTP 404"*) return 4 ;;
	esac
	printf '%s\n' "$out" >&2
	return 1
}

cmd_collect() {
	local repo="${GH_REPO:?GH_REPO must name the repository (owner/repo)}"
	local tmp
	tmp="$(scratch collect)"

	local status
	: >"$tmp/runs.jsonl"
	for status in queued pending waiting requested in_progress; do
		api_get "repos/${repo}/actions/runs?status=${status}&per_page=100" >"$tmp/page.json" ||
			die "could not list ${status} runs"
		"$JQ" -c '.workflow_runs[]' "$tmp/page.json" >>"$tmp/runs.jsonl"
	done
	"$JQ" -s 'unique_by(.id)' "$tmp/runs.jsonl" >"$tmp/runs.json"

	# Jobs of every queued / in_progress run old enough to matter (one call
	# each): whether a job has waited for a runner is what makes those stuck.
	# STUCK_NOW pins the clock (tests); otherwise the current time.
	local now cutoff
	now="${STUCK_NOW:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
	cutoff="$("$JQ" -rn --arg n "$now" --argjson t "$THRESHOLD" '($n | fromdateiso8601) - $t * 60')" ||
		die "STUCK_NOW must be an ISO-8601 UTC time, got '$now'"
	echo '{}' >"$tmp/jobs.json"
	local id
	while IFS= read -r id; do
		[ -n "$id" ] || continue
		if api_get "repos/${repo}/actions/runs/${id}/jobs?per_page=100" >"$tmp/page.json"; then
			"$JQ" --arg id "$id" --slurpfile p "$tmp/page.json" '. + {($id): $p[0].jobs}' \
				"$tmp/jobs.json" >"$tmp/jobs.next"
			mv "$tmp/jobs.next" "$tmp/jobs.json"
		else
			echo "::warning::stuck-runs: could not list the jobs of run ${id}" >&2
		fi
	done < <("$JQ" -r --argjson cutoff "$cutoff" \
		'.[] | select(.status == "queued" or .status == "in_progress")
		     | select((.created_at | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601) <= $cutoff)
		     | .id' "$tmp/runs.json")

	# The newest runs of each (workflow, branch) with a run old enough to matter:
	# whether a newer run will replace a stuck one. A failed lookup leaves the
	# key out, which `decide` reads as "no newer run" — reported, never cancelled.
	echo '{}' >"$tmp/wfruns.json"
	local wf branch
	while IFS=$'\t' read -r wf branch; do
		[ -n "$wf" ] || continue
		if api_get "repos/${repo}/actions/workflows/${wf}/runs?branch=$(urlencode "$branch")&per_page=30" >"$tmp/page.json"; then
			"$JQ" --arg k "${wf}:${branch}" --slurpfile p "$tmp/page.json" '. + {($k): $p[0].workflow_runs}' \
				"$tmp/wfruns.json" >"$tmp/wfruns.next"
			mv "$tmp/wfruns.next" "$tmp/wfruns.json"
		else
			echo "::warning::stuck-runs: could not list the runs of workflow ${wf} on ${branch}" >&2
		fi
	done < <("$JQ" -r --argjson cutoff "$cutoff" \
		'[.[] | select((.created_at | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601) <= $cutoff)
		      | select(.head_branch != null)
		      | [(.workflow_id | tostring), .head_branch]] | unique | .[] | @tsv' "$tmp/runs.json")

	# Branch heads: a 404 is "no such branch" (null), any other failure leaves
	# the branch out, which `decide` treats as unknown — reported, never cancelled.
	echo '{}' >"$tmp/branches.json"
	local sha rc
	while IFS= read -r branch; do
		[ -n "$branch" ] || continue
		rc=0
		sha="$(api_get "repos/${repo}/git/ref/heads/$(urlencode "$branch" | sed 's|%2F|/|g')" | "$JQ" -r '.object.sha // empty')" || rc=$?
		if [ "$rc" -eq 0 ] && [ -n "$sha" ]; then
			"$JQ" --arg b "$branch" --arg s "$sha" '. + {($b): $s}' "$tmp/branches.json" >"$tmp/b.next"
		elif [ "$rc" -eq 4 ]; then
			"$JQ" --arg b "$branch" '. + {($b): null}' "$tmp/branches.json" >"$tmp/b.next"
		else
			echo "::warning::stuck-runs: could not read the head of ${branch}; its runs are reported, never cancelled" >&2
			continue
		fi
		mv "$tmp/b.next" "$tmp/branches.json"
	done < <("$JQ" -r '[.[].head_branch // empty] | unique | .[]' "$tmp/runs.json")

	"$JQ" -n --arg repo "$repo" --arg now "$now" \
		--slurpfile runs "$tmp/runs.json" \
		--slurpfile jobs "$tmp/jobs.json" \
		--slurpfile branches "$tmp/branches.json" \
		--slurpfile wfruns "$tmp/wfruns.json" \
		'{now: $now, repository: $repo, runs: $runs[0], jobs: $jobs[0],
		  branches: $branches[0], workflow_runs: $wfruns[0]}'
}

# The report, as Markdown, from the decisions on stdin (plus the cancel outcome
# per id in $1, "<id> <outcome>" lines).
render_report() {
	local outcomes="$1"
	echo "Workflow runs stuck for more than ${THRESHOLD} minutes before they started (pending behind a concurrency group, or a job that never got a runner):"
	echo
	echo "| run | workflow | event | branch | commit | status | stuck for | action |"
	echo "|---|---|---|---|---|---|---|---|"
	local action id wf event branch sha status age why url outcome
	while IFS=$'\t' read -r action id wf event branch sha status age why url; do
		outcome="$(awk -v id="$id" '$1 == id { $1 = ""; sub(/^ /, ""); print }' "$outcomes")"
		[ -n "$outcome" ] || outcome="reported only"
		printf '| [%s](%s) | %s | %s | `%s` | `%s` | %s | %sm | %s — %s |\n' \
			"$id" "$url" "$wf" "$event" "$branch" "${sha:0:8}" "$status" "$age" "$outcome" "$why"
	done
	echo
	echo "A run is cancelled only when it is a superseded push/pull_request run (its commit is no longer the head of its branch, first attempt, not a fork). The head's own run is never cancelled: unstick it by hand — \`gh run cancel <id>\` (or \`gh api -X POST repos/<repo>/actions/runs/<id>/force-cancel\`), then re-run it. See docs/runbook.md, \"Stuck workflow runs\"."
}

cmd_run() {
	local dry=0
	case "${1:-}" in
	--dry-run) dry=1 ;;
	"") ;;
	*) die "unknown argument '$1'" ;;
	esac
	local repo="${GH_REPO:?GH_REPO must name the repository (owner/repo)}"
	local tmp
	tmp="$(scratch run)"

	cmd_collect >"$tmp/snapshot.json"
	cmd_decide <"$tmp/snapshot.json" >"$tmp/decisions.tsv"
	local n
	n="$(wc -l <"$tmp/decisions.tsv" | tr -d ' ')"
	echo "stuck runs (threshold ${THRESHOLD}m): ${n}"
	sed 's/^/  /' "$tmp/decisions.tsv"

	: >"$tmp/outcomes"
	local action id wf event branch sha status age why url
	while IFS=$'\t' read -r action id wf event branch sha status age why url; do
		[ "$action" = cancel ] || continue
		if [ "$dry" -eq 1 ]; then
			echo "DRY RUN: would cancel ${id} (${wf}, ${branch}@${sha:0:8}): ${why}"
			echo "${id} would cancel (dry run)" >>"$tmp/outcomes"
		elif gh api -X POST "repos/${repo}/actions/runs/${id}/cancel" >/dev/null 2>"$tmp/cancel.err"; then
			echo "cancelled ${id} (${wf}, ${branch}@${sha:0:8}): ${why}"
			echo "${id} CANCELLED" >>"$tmp/outcomes"
		else
			echo "::warning::stuck-runs: could not cancel ${id}: $(tr '\n' ' ' <"$tmp/cancel.err")"
			echo "${id} cancel FAILED" >>"$tmp/outcomes"
		fi
	done <"$tmp/decisions.tsv"

	local ids marker
	ids="$(cut -f2 "$tmp/decisions.tsv" | sort -n | paste -sd, -)"
	marker="${MARKER_PREFIX} ${ids:-none} -->"
	if [ "$n" -gt 0 ]; then
		{
			render_report "$tmp/outcomes" <"$tmp/decisions.tsv"
			echo
			echo "Watchdog run: ${RUN_URL:-n/a}"
			echo
			echo "_Filed automatically by \`stuck-runs\` (.github/workflows/stuck-runs.yaml); this issue is reused while runs stay stuck, and closed once none are._"
			echo
			echo "$marker"
		} >"$tmp/body.md"
		if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then cat "$tmp/body.md" >>"$GITHUB_STEP_SUMMARY"; fi
		while IFS=$'\t' read -r action id wf event branch sha status age why url; do
			echo "::warning title=stuck run ${id}::${wf} on ${branch}@${sha:0:8} has been ${status} for ${age}m (${action}: ${why})"
		done <"$tmp/decisions.tsv"
	fi

	local num
	if [ "$dry" -eq 1 ]; then
		if [ "$n" -gt 0 ]; then
			echo "DRY RUN: would open or update the issue \"${ISSUE_TITLE}\" with:"
			sed 's/^/  | /' "$tmp/body.md"
		else
			echo "DRY RUN: nothing stuck; would close an open \"${ISSUE_TITLE}\" issue if there is one"
		fi
		return 0
	fi

	# Exact-title match (the search is fuzzy, the select is not).
	num="$(gh issue list --state open --limit 100 --search "in:title \"${ISSUE_TITLE}\"" \
		--json number,title --jq "[.[] | select(.title == \"${ISSUE_TITLE}\")] | .[0].number // empty")"
	if [ "$n" -eq 0 ]; then
		if [ -n "$num" ]; then
			gh issue comment "$num" --body "Nothing is stuck any more (threshold ${THRESHOLD}m). Closing; the next stuck run opens a new issue. ${RUN_URL:-}"
			gh issue close "$num"
			echo "closed #${num}"
		fi
		return 0
	fi
	if [ -z "$num" ]; then
		gh issue create --title "$ISSUE_TITLE" --body-file "$tmp/body.md"
		return 0
	fi
	# Same set of stuck runs as the last report: say nothing new.
	local last
	last="$(gh issue view "$num" --json body,comments \
		--jq '[.body, .comments[].body] | map(select(contains("'"${MARKER_PREFIX}"'"))) | last // ""' |
		grep -oF -- "$marker" || true)"
	if [ -n "$last" ]; then
		echo "#${num} already reports this set of stuck runs"
		return 0
	fi
	gh issue comment "$num" --body-file "$tmp/body.md"
	echo "commented on #${num}"
}

case "${1:-}" in
decide) cmd_decide ;;
collect) cmd_collect ;;
run)
	shift
	cmd_run "$@"
	;;
*)
	echo "usage: $0 decide < snapshot.json | collect | run [--dry-run]" >&2
	exit 2
	;;
esac
