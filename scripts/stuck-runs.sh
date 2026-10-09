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
# issue current (reused, no duplicates). It comments only when the set of stuck
# runs changed since its last comment, so a run stuck for a day costs one
# comment, not 48; and it closes the issue once nothing is stuck. --dry-run
# cancels nothing and writes no issue; it prints what it would do. Needs
# GH_TOKEN (actions: write, issues: write) and GH_REPO.
#
# WHOSE ISSUE (#1532, #1568). The rolling issue is the one the workflow token
# opened under the title, found by listing and never by search, and only what
# that account wrote on it is read back: scripts/rolling-issue-lib.sh says why.
# It matters twice here. An issue a stranger opened under the title was
# commented on and closed; and the memory below is a hidden marker, so a
# comment by anyone carrying `stuck-runs-uncancellable: <id>` made the watchdog
# stop counting that run. The issue is opened with ISSUE_LABELS; a label that
# is gone still files the report, and then fails the run (exit 2).
#
# UNCANCELLABLE RUNS
#
# GitHub can strand a run it then refuses to cancel: run 34723047990, a proxy
# run queued since 2026-09-12 on a branch since deleted, answers both cancel and
# force-cancel with HTTP 409 ("Cannot cancel a workflow run that is not in
# progress"). Nothing in this repository can clear it, and re-reporting it every
# tick kept the rolling issue open forever (#1301). So when a cancel AND the
# force-cancel after it both come back 409, the run is reported ONCE as
# "uncancellable — needs GitHub support" and remembered in a hidden marker,
#   <!-- stuck-runs-uncancellable: <id>,<id> -->
# carried in every report the watchdog writes (body, comment, closing comment).
# Each tick reads it back from its own issue with the title (the open one, else the newest closed),
# and leaves those runs out of the stuck set, so the issue closes once nothing
# else is stuck and a NEW stuck run is reported as usual. A remembered id is
# carried only while GitHub still lists the run as stuck. Any other cancel
# failure (a 5xx, a timeout) is not remembered: the next tick tries again.
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
# Labels the repository has; AGENTS.md asks a kind and an area of every issue.
ISSUE_LABELS=(bug ci)
MARKER_PREFIX="<!-- stuck-runs:"
# Not a prefix of MARKER_PREFIX ("-" after "stuck-runs", not ":"), so the
# same-set check never mistakes one for the other.
UNCANCELLABLE_PREFIX="<!-- stuck-runs-uncancellable:"
UNCANCELLABLE_OUTCOME="uncancellable — needs GitHub support"

# shellcheck source=scripts/rolling-issue-lib.sh
. "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/rolling-issue-lib.sh"

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
	if grep -qF -- "$UNCANCELLABLE_OUTCOME" "$outcomes"; then
		echo
		echo "**${UNCANCELLABLE_OUTCOME}**: GitHub refused both the cancel and the force-cancel (HTTP 409). Nothing in this repository can clear such a run; ask GitHub support to remove it. It is reported this once and not counted as stuck again."
	fi
}

# The runs remembered as uncancellable: every id in an UNCANCELLABLE_PREFIX
# marker on the issue that holds the watchdog's record under the report's
# title: its open one, and with none open its newest closed one (which still
# carries its closing comment's marker), never a duplicate that was folded into
# another (rolling_issue_record). Only what the watchdog itself wrote there is
# read. One id per line. Returns non-zero when the issue cannot be read.
read_uncancellable() {
	local num
	num="$(rolling_issue_record "$ISSUE_TITLE")" || return 1
	[ -n "$num" ] || return 0
	rolling_issue_text "$num" >"$SCRATCH/issue.txt" || return 1
	# grep -o reads all of its input (never the early-exit `| grep -q` shape), and
	# finding no marker is not an error.
	{ grep -oE -- "${UNCANCELLABLE_PREFIX} [0-9,]+ -->" "$SCRATCH/issue.txt" || true; } |
		grep -oE '[0-9]+' | sort -un || true
}

# cancel_run <repo> <id> <err file>: prints the outcome for the report.
# Cancel; on a 409 (GitHub says the run is not in progress, though it lists it
# as queued) try force-cancel; a 409 from that too is UNCANCELLABLE_OUTCOME.
cancel_run() {
	local repo="$1" id="$2" err="$3"
	if gh api -X POST "repos/${repo}/actions/runs/${id}/cancel" >/dev/null 2>"$err"; then
		echo "CANCELLED"
		return 0
	fi
	if ! grep -qF 'HTTP 409' "$err"; then
		echo "cancel FAILED"
		return 0
	fi
	if gh api -X POST "repos/${repo}/actions/runs/${id}/force-cancel" >/dev/null 2>"$err.force"; then
		echo "FORCE-CANCELLED (cancel was refused)"
	elif grep -qF 'HTTP 409' "$err.force"; then
		echo "$UNCANCELLABLE_OUTCOME"
	else
		echo "cancel FAILED"
	fi
	cat "$err.force" >>"$err"
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
	cmd_decide <"$tmp/snapshot.json" >"$tmp/all.tsv"

	# Runs already reported as uncancellable are not stuck any more, as far as
	# this watchdog is concerned (see UNCANCELLABLE RUNS). Unreadable memory is
	# not fatal: the run is reported again, which is the pre-#1301 behaviour.
	if ! read_uncancellable >"$tmp/known"; then
		echo "::warning::stuck-runs: could not read the uncancellable runs from the issue; reporting every stuck run"
		: >"$tmp/known"
	fi
	# The ids as a variable, not a first file: `NR == FNR` misreads an EMPTY
	# first file, and no memory is the common case.
	local known_ids split_known='BEGIN { n = split(known, a, " "); for (i = 1; i <= n; i++) k[a[i]] = 1 }'
	known_ids="$(paste -sd' ' "$tmp/known")"
	awk -F'\t' -v known="$known_ids" "$split_known"' ($2 in k)' "$tmp/all.tsv" >"$tmp/ignored.tsv"
	awk -F'\t' -v known="$known_ids" "$split_known"' !($2 in k)' "$tmp/all.tsv" >"$tmp/decisions.tsv"

	local n
	n="$(wc -l <"$tmp/decisions.tsv" | tr -d ' ')"
	echo "stuck runs (threshold ${THRESHOLD}m): ${n}"
	sed 's/^/  /' "$tmp/decisions.tsv"
	local action id wf event branch sha status age why url
	while IFS=$'\t' read -r action id wf event branch sha status age why url; do
		echo "ignored ${id} (${wf}, ${branch}@${sha:0:8}): already reported as ${UNCANCELLABLE_OUTCOME}"
	done <"$tmp/ignored.tsv"

	: >"$tmp/outcomes"
	: >"$tmp/new-uncancellable"
	local outcome
	while IFS=$'\t' read -r action id wf event branch sha status age why url; do
		[ "$action" = cancel ] || continue
		if [ "$dry" -eq 1 ]; then
			echo "DRY RUN: would cancel ${id} (${wf}, ${branch}@${sha:0:8}): ${why}"
			echo "${id} would cancel (dry run)" >>"$tmp/outcomes"
			continue
		fi
		outcome="$(cancel_run "$repo" "$id" "$tmp/cancel.err")"
		case "$outcome" in
		*CANCELLED*) echo "cancelled ${id} (${wf}, ${branch}@${sha:0:8}): ${outcome}: ${why}" ;;
		*)
			echo "::warning::stuck-runs: could not cancel ${id} (${outcome}): $(tr '\n' ' ' <"$tmp/cancel.err")"
			[ "$outcome" = "$UNCANCELLABLE_OUTCOME" ] && echo "$id" >>"$tmp/new-uncancellable"
			;;
		esac
		echo "${id} ${outcome}" >>"$tmp/outcomes"
	done <"$tmp/decisions.tsv"

	# The memory to carry forward: the remembered runs GitHub still lists as
	# stuck, plus the ones that just refused. A run that is gone drops out.
	local remember uncancellable_marker=""
	remember="$(cut -f2 "$tmp/ignored.tsv" | cat - "$tmp/new-uncancellable" | sed '/^$/d' | sort -un | paste -sd, -)"
	[ -z "$remember" ] || uncancellable_marker="${UNCANCELLABLE_PREFIX} ${remember} -->"

	local ids marker
	ids="$(cut -f2 "$tmp/decisions.tsv" | sort -n | paste -sd, -)"
	marker="${MARKER_PREFIX} ${ids:-none} -->"
	if [ "$n" -gt 0 ]; then
		{
			render_report "$tmp/outcomes" <"$tmp/decisions.tsv"
			if [ -s "$tmp/ignored.tsv" ]; then
				echo
				printf 'Not counted (already reported as %s): %s\n' "$UNCANCELLABLE_OUTCOME" \
					"$(awk -F'\t' '{ printf "%s[%s](%s)", (NR > 1 ? ", " : ""), $2, $10 }' "$tmp/ignored.tsv")"
			fi
			echo
			echo "Watchdog run: ${RUN_URL:-n/a}"
			echo
			echo "_Filed automatically by \`stuck-runs\` (.github/workflows/stuck-runs.yaml); this issue is reused while runs stay stuck, and closed once none are._"
			echo
			echo "$marker"
			[ -z "$uncancellable_marker" ] || echo "$uncancellable_marker"
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

	# The watchdog's own open issue with exactly the title (the oldest, should
	# there be two): never one somebody else opened under it.
	local numbers
	numbers="$(rolling_issue_list open "$ISSUE_TITLE")" || die "could not list the open issues"
	num="$(sed -n 1p <<<"$numbers")"
	if [ "$n" -eq 0 ]; then
		if [ -n "$num" ]; then
			local closing="Nothing is stuck any more (threshold ${THRESHOLD}m). Closing; the next stuck run opens a new issue. ${RUN_URL:-}"
			if [ -n "$uncancellable_marker" ]; then
				# The closed issue is where the next tick reads the memory from.
				closing="${closing}"$'\n\n'"Still listed by GitHub but not counted (${UNCANCELLABLE_OUTCOME}): ${remember//,/, }"$'\n\n'"${uncancellable_marker}"
			fi
			rolling_issue_comment "$num" "$closing" || die "could not comment on #${num}"
			rolling_issue_close "$num" completed || die "could not close #${num}"
			echo "closed #${num}"
		fi
		return 0
	fi
	if [ -z "$num" ]; then
		local rc=0
		num="$(rolling_issue_create "$ISSUE_TITLE" "$(cat "$tmp/body.md")" "${ISSUE_LABELS[@]}")" || rc=$?
		[ "$rc" -ne 1 ] || die "could not open the issue"
		echo "reported on #${num}"
		# The report is filed; an issue without its labels is not a clean run.
		[ "$rc" -eq 0 ] || die "#${num} was opened without its labels (${ISSUE_LABELS[*]})"
		return 0
	fi
	# Same set of stuck runs as the last report: say nothing new — unless a run
	# just turned out uncancellable, which is reported (once) regardless.
	# The last set the watchdog itself wrote there (rolling_issue_text prints
	# nothing anyone else wrote).
	local last
	rolling_issue_text "$num" >"$tmp/issue.txt" || die "could not read #${num}"
	last="$({ grep -oE -- "${MARKER_PREFIX} [0-9a-z,]+ -->" "$tmp/issue.txt" || true; } | sed -n '$p')"
	if [ "$last" = "$marker" ] && [ ! -s "$tmp/new-uncancellable" ]; then
		echo "#${num} already reports this set of stuck runs"
		return 0
	fi
	rolling_issue_comment "$num" "$(cat "$tmp/body.md")" || die "could not comment on #${num}"
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
