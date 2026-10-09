#!/usr/bin/env bash
#
# Report a watcher workflow whose OWN run failed (#1533).
#
# WHY
#
# Some workflows exist to tell a person about a failure nobody would otherwise
# see: `main-post-merge-watch` (a red post-merge run), `publish-verify` (a
# commit whose artifacts are missing), `stuck-runs` (a run that never started),
# `third-party-images` (a pin that fell behind), the `report-failure` job of
# `e2e` (a red nightly). Each reports on a rolling issue. When the reporter
# itself fails (the API answered 5xx, a label is gone, a script has a bug) the
# result is a red run of a workflow attached to no pull request: nobody is
# told, and the failure it was reporting goes unreported with it.
#
# WHAT IT DOES
#
# It is a second step of .github/workflows/stuck-runs.yaml, which already runs
# on a schedule with the two permissions this needs (read runs, write issues).
# For each workflow in WATCHERS it lists the newest completed runs that an
# event started (schedule, workflow_run, push; never a pull request's dry run
# or a dispatch, which a person is looking at) and reports, on ONE rolling
# issue, "CI: a watcher workflow's own run failed":
#
#   which runs    `latest`: only the newest. For a watcher that looks at the
#                 whole state each time, a later green run has done the work of
#                 the failed one. `each`: every one of the newest 20. For
#                 `main-post-merge-watch`, which judges ONE post-merge run per
#                 run of its own: a green watcher run for another commit says
#                 nothing about the one whose watcher failed.
#   what counts   `run`: the run ended failure, timed_out or startup_failure
#                 (the workflow reports and exits 0, so any such end is the
#                 reporter failing). `step:<regex>`: such a run AND a step
#                 whose name matches failed (publish-verify goes red by design
#                 when artifacts are missing; only its issue steps failing is
#                 the reporter failing). `job:<name>`: such a run AND that job
#                 failed (the nightly e2e is red whenever a suite is; only
#                 `report-failure` failing is the reporter failing).
#   cancelled     never counts: stuck-runs cancels its own older tick, and of
#                 two watchers waiting on one run GitHub keeps one.
#
# The issue lists the runs, what each watcher's failure leaves unreported and
# the command that re-runs it. It gets a comment only when the SET of failed
# runs changes, and closes when there is none: a `latest` watcher clears when
# its next run passes, an `each` one when the failed run is re-run green (same
# run id) or leaves the newest 20.
#
# NO WATCHER OF WATCHERS. This is not a new workflow and nothing watches it in
# turn. `stuck-runs` is in WATCHERS, so a tick that failed is reported by the
# next tick that works: this step runs even when the step before it failed
# (`if: ${{ !cancelled() }}` in the workflow), and a failure of its own is the run
# failing, which the next tick sees the same way. WHAT IS STILL UNSEEN:
#   - stuck-runs.yaml failing EVERY time, before or in this step (a workflow
#     file that does not parse, a token without `issues: write`, this script
#     broken, the issue API down for good). Nothing in the repository reports
#     that; the Actions page shows it, and GitHub mails the person who last
#     edited a scheduled workflow when it fails, if they kept that
#     notification on.
#   - the schedule not firing. GitHub delays and drops scheduled runs, and
#     disables a schedule after 60 days without a commit.
#   - a watcher that ends `success` having reported nothing or the wrong
#     thing. A conclusion is all that is read here.
#   - until the next tick: a failed watcher run is seen when stuck-runs next
#     runs, not when it fails.
#
# USAGE
#
#   scripts/watcher-runs.sh decide < snapshot.json   # pure; prints the failed runs
#   scripts/watcher-runs.sh collect > snapshot.json  # live; GitHub API via `gh`
#   scripts/watcher-runs.sh run [--dry-run]          # collect | decide | report
#   scripts/watcher-runs.sh watchers                 # the table, tab-separated
#   scripts/watcher-runs.sh title | labels
#
# The snapshot:
#   { "repository": "<owner>/<repo>",
#     "runs": { "<workflow file>": [ <GET /actions/workflows/<file>/runs objects> ] },
#     "jobs": { "<run id>": [ <GET /actions/runs/<id>/attempts/<n>/jobs objects> ] },
#     "unread": [ "<workflow file>", ... ] }       # its runs could not be listed
# `decide` prints one tab-separated line per failed watcher run:
#   workflow file  run id  attempt  event  conclusion  what failed  url
#
# `run` writes the issue through scripts/rolling-issue-lib.sh: the workflow's
# own issue, found by listing, opened with ISSUE_LABELS. --dry-run reads
# everything and writes nothing. Needs GH_TOKEN (actions: read, issues: write)
# and GH_REPO.
#
# Exit 0 whether or not a watcher failed (the issue is the report). Exit 2 when
# the check itself is broken: a workflow's runs could not be listed (what was
# found is still reported, but an open issue is neither closed nor told the
# set changed, since the set is not known), the issue could not be written, or
# it was opened without a label.
#
# Tests: scripts/watcher_runs_test.sh (//scripts:watcher_runs_test), which also
# holds WATCHERS to the workflow files: every file exists, every `job:` names a
# job and every `step:` matches a step, and every workflow that may write an
# issue is either here or named there with the reason it is not.
# shellcheck disable=SC2016 # single-quoted $names here are jq variables (and
# Markdown backticks), never shell expansions.
set -euo pipefail

JQ="${JQ:-jq}"
ISSUE_TITLE="CI: a watcher workflow's own run failed"
# Labels the repository has; AGENTS.md asks a kind and an area of every issue.
ISSUE_LABELS=(bug ci)
MARKER_PREFIX="<!-- watcher-runs:"
# How many of a workflow's newest completed runs are read.
WINDOW=20

# workflow file <TAB> which runs <TAB> what counts <TAB> what goes unreported
# while it fails <TAB> what to do about a failed run of it (%s: the run id).
WATCHERS="$(printf '%s\t%s\t%s\t%s\t%s\n' \
	main-watch.yaml each run \
	'the post-merge run it was judging: if that run did not pass, it is not on the "main-post-merge" issue' \
	'`gh run rerun %s`: it judges the same post-merge run again, and writes nothing twice' \
	publish-verify.yaml latest 'step:issue' \
	'a commit on main with missing artifacts, or a control that no longer goes red: the run is red, and no issue says so' \
	'read which issue step failed, then `gh run rerun %s`: it verifies the same commit again' \
	stuck-runs.yaml latest run \
	'runs stuck before they started, and the other rows of this table' \
	'read the failed step; the next tick runs by itself' \
	third-party-images.yaml latest run \
	'third-party image pins that fell behind their tags' \
	'read the failed step, then `gh workflow run third-party-images.yaml -f dry_run=false`, or wait for the next day' \
	e2e.yaml latest 'job:report-failure' \
	'a red nightly e2e run: it is not on the "Nightly e2e failing" issue' \
	'read the nightly run itself: it failed too, and nothing told anyone')"

# shellcheck source=scripts/rolling-issue-lib.sh
. "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/rolling-issue-lib.sh"

SCRATCH="$(mktemp -d)"
trap 'rm -rf "$SCRATCH"' EXIT

die() {
	echo "::error::watcher-runs: $*" >&2
	exit 2
}

watchers_json() {
	"$JQ" -R -s 'split("\n") | map(select(. != "") | split("\t")
		| {file: .[0], runs: .[1], counts: .[2], unreported: .[3], todo: .[4]})' <<<"$WATCHERS"
}

# The decision, as one jq program over the snapshot. Pure: no clock, no network.
DECIDE='
def bad: IN("failure", "timed_out", "startup_failure");
.repository as $repo
| (.jobs // {}) as $jobs
| (.runs // {}) as $runs
| $watchers[] as $w
| [ ($runs[$w.file] // [])[]
    | select(.status == "completed")
    | select(.event | IN("schedule", "workflow_run", "push"))
    | select((.head_repository.full_name // "") == $repo) ]
| sort_by(.created_at, .id) | reverse | .[0:$window]
| (if $w.runs == "latest" then .[0:1] else . end)[]
| select((.conclusion // "") | bad)
| . as $r
| ($jobs[($r.id | tostring)] // null) as $j
| (if $w.counts == "run" then
     "the run ended \($r.conclusion)"
   elif $j == null then
     "the run ended \($r.conclusion), and its jobs could not be read"
   elif ($w.counts | startswith("job:")) then
     ($w.counts | ltrimstr("job:")) as $name
     | ([$j[] | select(.name == $name and ((.conclusion // "") | bad))] | first) as $job
     | if $job == null then null else "the \($name) job ended \($job.conclusion)" end
   elif ($w.counts | startswith("step:")) then
     ($w.counts | ltrimstr("step:")) as $re
     | ([$j[] | (.steps // [])[] | select((.name | test($re; "i")) and ((.conclusion // "") | bad))] | first) as $step
     | if $step == null then null else "the step \"\($step.name)\" ended \($step.conclusion)" end
   else
     "watcher-runs has no rule for \($w.counts)"
   end) as $what
| select($what != null)
| [ $w.file, ($r.id | tostring), (($r.run_attempt // 1) | tostring), $r.event, $r.conclusion, $what, ($r.html_url // "") ]
| map(gsub("[\t\n\r|`]"; " "))
| @tsv
'

cmd_decide() {
	"$JQ" -r --argjson watchers "$(watchers_json)" --argjson window "$WINDOW" "$DECIDE"
}

# gh api, three tries: one 502 must not turn into a report that the watchdog
# itself failed. WATCHER_RETRY_SLEEP: seconds between tries (the test's seam).
api_get() {
	local try out
	for try in 1 2 3; do
		if out="$(gh api "$1" 2>"$SCRATCH/api.err")"; then
			printf '%s\n' "$out"
			return 0
		fi
		[ "$try" -eq 3 ] || sleep "${WATCHER_RETRY_SLEEP:-5}"
	done
	cat "$SCRATCH/api.err" >&2
	return 1
}

cmd_collect() {
	local repo="${GH_REPO:?GH_REPO must name the repository (owner/repo)}"
	local tmp="$SCRATCH/collect" file rest id attempt counts
	mkdir -p "$tmp"
	echo '{}' >"$tmp/runs.json"
	echo '{}' >"$tmp/jobs.json"
	echo '[]' >"$tmp/unread.json"
	while IFS=$'\t' read -r file rest; do
		[ -n "$file" ] || continue
		# More than the window: pull request and dispatch runs are in the list
		# too, and `decide` leaves them out.
		if api_get "repos/${repo}/actions/workflows/${file}/runs?status=completed&per_page=100" >"$tmp/page.json" &&
			"$JQ" -e '.workflow_runs | type == "array"' "$tmp/page.json" >/dev/null; then
			"$JQ" --arg f "$file" --slurpfile p "$tmp/page.json" '. + {($f): $p[0].workflow_runs}' "$tmp/runs.json" >"$tmp/next"
			mv "$tmp/next" "$tmp/runs.json"
		else
			echo "::warning::watcher-runs: could not list the runs of ${file}" >&2
			"$JQ" --arg f "$file" '. + [$f]' "$tmp/unread.json" >"$tmp/next"
			mv "$tmp/next" "$tmp/unread.json"
		fi
	done <<<"$WATCHERS"

	# The jobs of the runs a `job:` or `step:` rule has to look into: what
	# `decide` reports when every such rule is read as `run`. A listing that
	# fails leaves the run out, which `decide` reports as unread, not as fine.
	"$JQ" -n --arg repo "$repo" --slurpfile runs "$tmp/runs.json" '{repository: $repo, runs: $runs[0]}' >"$tmp/first.json"
	while IFS=$'\t' read -r file id attempt rest; do
		[ -n "$id" ] || continue
		counts="$(awk -F'\t' -v f="$file" '$1 == f { print $3 }' <<<"$WATCHERS")"
		[ "$counts" != run ] || continue
		if api_get "repos/${repo}/actions/runs/${id}/attempts/${attempt}/jobs?per_page=100" >"$tmp/page.json" &&
			"$JQ" -e '.jobs | type == "array"' "$tmp/page.json" >/dev/null; then
			"$JQ" --arg id "$id" --slurpfile p "$tmp/page.json" '. + {($id): $p[0].jobs}' "$tmp/jobs.json" >"$tmp/next"
			mv "$tmp/next" "$tmp/jobs.json"
		else
			echo "::warning::watcher-runs: could not list the jobs of run ${id} (${file})" >&2
		fi
	done < <("$JQ" -r --argjson window "$WINDOW" \
		--argjson watchers "$(watchers_json | "$JQ" 'map(.counts = "run")')" "$DECIDE" "$tmp/first.json")

	"$JQ" -n --arg repo "$repo" --slurpfile runs "$tmp/runs.json" --slurpfile jobs "$tmp/jobs.json" \
		--slurpfile unread "$tmp/unread.json" \
		'{repository: $repo, runs: $runs[0], jobs: $jobs[0], unread: $unread[0]}'
}

# The report, as Markdown, from the decisions on stdin.
render_report() {
	local file id attempt event what url unreported todo
	echo "A workflow that exists to report a failure has a failed run of its own. While it fails, what it reports is not reported:"
	echo
	echo "| workflow | run | started by | what failed | what goes unreported | what to do |"
	echo "|---|---|---|---|---|---|"
	while IFS=$'\t' read -r file id attempt event _ what url; do
		unreported="$(awk -F'\t' -v f="$file" '$1 == f { print $4 }' <<<"$WATCHERS")"
		todo="$(awk -F'\t' -v f="$file" '$1 == f { print $5 }' <<<"$WATCHERS")"
		# shellcheck disable=SC2059 # the format is the table's own text, with one %s for the run id
		printf '| `%s` | [%s](%s), attempt %s | %s | %s | %s | %s |\n' \
			"$file" "$id" "$url" "$attempt" "$event" "$what" "$unreported" "$(printf "$todo" "$id")"
	done
	echo
	echo "Read the failed step first: an API that answered 5xx is a re-run, a label that is gone or a script that broke is a fix. See docs/runbook.md, \"A watcher workflow's own run failed\"."
}

cmd_run() {
	local dry=0
	case "${1:-}" in
	--dry-run) dry=1 ;;
	"") ;;
	*) die "unknown argument '$1'" ;;
	esac
	: "${GH_REPO:?GH_REPO must name the repository (owner/repo)}"
	local tmp="$SCRATCH/run"
	mkdir -p "$tmp"

	cmd_collect >"$tmp/snapshot.json"
	cmd_decide <"$tmp/snapshot.json" >"$tmp/failed.tsv"
	local n unread
	n="$(wc -l <"$tmp/failed.tsv" | tr -d ' ')"
	unread="$("$JQ" -r '.unread | join(", ")' "$tmp/snapshot.json")"
	echo "watcher runs that failed: ${n}"
	sed 's/^/  /' "$tmp/failed.tsv"
	[ -z "$unread" ] || echo "not read (the listing failed): ${unread}"

	local file id attempt event what url ids marker
	ids="$(awk -F'\t' '{ print $1 ":" $2 "/" $3 }' "$tmp/failed.tsv" | LC_ALL=C sort | paste -sd, -)"
	marker="${MARKER_PREFIX} ${ids:-none} -->"
	if [ "$n" -gt 0 ]; then
		{
			render_report <"$tmp/failed.tsv"
			if [ -n "$unread" ]; then
				echo
				echo "Not read in this tick (the run listing failed), so not counted either way: ${unread}"
			fi
			echo
			echo "Reported by: ${RUN_URL:-n/a}"
			echo
			echo "_Filed automatically by \`stuck-runs\` (.github/workflows/stuck-runs.yaml, scripts/watcher-runs.sh); this issue is reused while a watcher's run is failing, and closed once none is._"
			echo
			echo "$marker"
		} >"$tmp/body.md"
		if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then cat "$tmp/body.md" >>"$GITHUB_STEP_SUMMARY"; fi
		while IFS=$'\t' read -r file id attempt event _ what url; do
			echo "::warning title=watcher run ${id} failed::${file}: ${what} (${url})"
		done <"$tmp/failed.tsv"
	fi

	if [ "$dry" -eq 1 ]; then
		if [ "$n" -gt 0 ]; then
			echo "DRY RUN: would open or update the issue \"${ISSUE_TITLE}\" with:"
			sed 's/^/  | /' "$tmp/body.md"
		elif [ -n "$unread" ]; then
			echo "DRY RUN: a listing failed; would leave an open \"${ISSUE_TITLE}\" issue as it is"
		else
			echo "DRY RUN: no watcher run failed; would close an open \"${ISSUE_TITLE}\" issue if there is one"
		fi
		[ -z "$unread" ] || die "could not list the runs of: ${unread}"
		return 0
	fi

	local numbers num rc=0
	numbers="$(rolling_issue_list open "$ISSUE_TITLE")" || die "could not list the open issues"
	num="$(sed -n 1p <<<"$numbers")"

	if [ -n "$unread" ]; then
		# An incomplete tick: what it found is real, what it did not reach is
		# unknown. It may open the issue; it may not close one, nor say the set
		# changed.
		if [ "$n" -gt 0 ] && [ -z "$num" ]; then
			num="$(rolling_issue_create "$ISSUE_TITLE" "$(cat "$tmp/body.md")" "${ISSUE_LABELS[@]}")" || rc=$?
			[ "$rc" -ne 1 ] || die "could not open the issue"
			echo "reported on #${num}"
		elif [ -n "$num" ]; then
			echo "#${num} left as it is: this tick does not know the whole set"
		fi
		die "could not list the runs of: ${unread}"
	fi

	if [ "$n" -eq 0 ]; then
		if [ -n "$num" ]; then
			rolling_issue_comment "$num" "No watcher run is failing any more. Closing; the next one opens a new issue. ${RUN_URL:-}" ||
				die "could not comment on #${num}"
			rolling_issue_close "$num" completed || die "could not close #${num}"
			echo "closed #${num}"
		fi
		return 0
	fi
	if [ -z "$num" ]; then
		num="$(rolling_issue_create "$ISSUE_TITLE" "$(cat "$tmp/body.md")" "${ISSUE_LABELS[@]}")" || rc=$?
		[ "$rc" -ne 1 ] || die "could not open the issue"
		echo "reported on #${num}"
		# The report is filed; an issue without its labels is not a clean run.
		[ "$rc" -eq 0 ] || die "#${num} was opened without its labels (${ISSUE_LABELS[*]})"
		return 0
	fi
	# The set this workflow last wrote there (nothing anyone else wrote is read).
	local last
	rolling_issue_text "$num" >"$tmp/issue.txt" || die "could not read #${num}"
	last="$({ grep -oE -- "${MARKER_PREFIX} [^ ]+ -->" "$tmp/issue.txt" || true; } | sed -n '$p')"
	if [ "$last" = "$marker" ]; then
		echo "#${num} already reports this set of failed watcher runs"
		return 0
	fi
	rolling_issue_comment "$num" "$(cat "$tmp/body.md")" || die "could not comment on #${num}"
	echo "commented on #${num}"
}

case "${1:-}" in
decide) cmd_decide ;;
collect) cmd_collect ;;
watchers) printf '%s\n' "$WATCHERS" ;;
title) printf '%s\n' "$ISSUE_TITLE" ;;
labels) printf '%s\n' "${ISSUE_LABELS[@]}" ;;
run)
	shift
	cmd_run "$@"
	;;
*)
	echo "usage: $0 decide < snapshot.json | collect | run [--dry-run] | watchers | title | labels" >&2
	exit 2
	;;
esac
