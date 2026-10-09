#!/usr/bin/env bash
# Someone to watch a red `main-post-merge` run (#1506).
#
# .github/workflows/main.yaml re-tests every push to `main`. No ruleset can
# require it (it runs after the merge), so a failure blocked nothing and was
# seen only by whoever opened the Actions page. This script is the step of
# .github/workflows/main-watch.yaml, which runs when a `main-post-merge` run
# completes: a run that did not pass is written on ONE rolling issue, and the
# issue closes when no commit named on it is failing any more.
#
# WHAT IS TRUSTED. A `workflow_run` workflow runs with a token that can write,
# for an event anyone who can start a workflow called `main-post-merge` can
# cause (a pull request from a fork can carry one). So exactly one value is
# taken from the event, the run id, and it must be digits. Everything else is
# read from the API for that id, and a run counts only when it is a `push` to
# `main` of this repository, of the workflow file .github/workflows/main.yaml.
# Whatever is then written on the issue is still reduced first: a sha is 40
# hex digits, a conclusion its letters, a job name plain characters in a code
# span, a job link one of this run's or nothing.
#
# WHICH RUNS FILE. Every completed run that did not conclude `success`, with
# one exception:
#   failure, timed_out      the commit failed its post-merge run, or another
#                           job of the run did while the gate, the `main` job,
#                           succeeded. Filed either way (the run is red until
#                           that job passes); the entry says which, so a
#                           validated commit is not read as a broken target.
#   startup_failure         the workflow file on main is not valid; no job ran,
#                           and every later push fails the same way.
#   skipped                 no job ran, so nothing validated the commit.
#                           main.yaml has no condition that skips every job;
#                           if this arrives, something changed.
#   action_required, stale, anything else, no conclusion at all
#                           not a verdict this script knows: fail closed.
#   cancelled               main.yaml has NO concurrency group, so a newer
#                           push never cancels an older run: cancelled means a
#                           job hit its time limit or never got a runner, or a
#                           person cancelled it. It is filed UNLESS the run's
#                           own gate, the `main` job, succeeded and every
#                           other job succeeded, was skipped or was cancelled:
#                           then `diff` and `test` did their work and the
#                           commit is validated, and what was cancelled is
#                           something else (run 37337609845: `refresh-pin-prs`
#                           waited 3.5 hours for a runner). A cancelled run
#                           in which another job had already FAILED is filed:
#                           it is the `failure` with a green gate of the first
#                           line, cut short. If the jobs cannot be read, it is
#                           filed.
#
# THE GATE, IN A RE-RUN OF SOME JOBS. `gh run rerun --failed` after an
# ancillary failure re-runs that job alone, and the jobs of the new attempt
# may then not include `main`. An absent gate is not a gate that did not run:
# nothing it rests on ran again, so its conclusion in the latest earlier
# attempt that has it stands, for the decision above and for what the entry
# says. When an earlier attempt cannot be read, the gate is `unknown`: a
# cancelled run is then filed, and the entry says validation is not known.
# What the listing does in fact (#1552, read on 2026-10-09 from the 21
# attempts that re-ran some of the jobs, in 19 of this repository's runs since
# 2026-09-28, e.g. run 37366070027 attempts 1 and 2):
# `/actions/runs/{id}/attempts/{n}/jobs` lists EVERY job of the run. A job the
# re-run did not run again is there as a copy: a new job id, `run_attempt` n,
# and the conclusion and the start time of the attempt it really ran in (all
# 152 such jobs). So the gate is present in an attempt that did not re-run it,
# with its earlier conclusion. That is an observation, not something the API
# reference says, and the walk back costs nothing when the gate is there: it
# stays.
#
# EVERY ATTEMPT, WHICHEVER WATCHER GETS TO IT. A run is re-run under the same
# id, as a new attempt, and the watcher of an attempt reads the run as it is
# when it runs, not as it was when the attempt ended. If the re-run had begun
# by then, the watcher of a failed attempt finds the run in progress and has
# nothing to judge; and of two watchers waiting on one run, GitHub keeps one.
# Left at that, whether a failure reached the issue depended on how fast its
# watcher started, and a failure followed by a re-run that was cancelled with
# the gate green (ignored, by the exception above) was never recorded at all.
# So a watcher judges the latest attempt AND, unless that attempt passed, each
# attempt before it, back to the first or to one that succeeded: whatever did
# not pass and is not on the issue yet is recorded then, oldest first, with a
# line saying it is recorded late. The rule, whatever the order of delivery:
#   an attempt that did not pass is recorded once, unless a later attempt of
#   the same run succeeded.
# "Once" rests on the marker of the attempt, and on the watchers of one run
# being serialised (the concurrency group of main-watch.yaml is the judged
# run). The attempts of a run, and what the issue says in the end:
#   attempt 1 \ attempt 2   none or   success   did not   cancelled,
#                           running             pass      validated
#   did not pass            1         closed    1 and 2   1
#   cancelled, validated    nothing   nothing   2         nothing
# (the same whether the watcher of attempt 1 ran before or after attempt 2
# began; //scripts:main_post_merge_watch_test runs every row both ways). An
# earlier attempt that cannot be read stops the walk there: what is known is
# recorded, and the step fails so that it is run again.
#
# WHICH COMMAND RE-RUNS IT. Each entry names one, from the run's jobs:
# `gh run rerun <id> --failed` when at least one job did not pass and every
# one of those concluded `failure` or `cancelled`, and the whole run
# (`gh run rerun <id>`) otherwise. `--failed` re-runs "the failed jobs and
# their dependent jobs", and neither the REST reference nor the how-to page
# says whether a cancelled job is one of them. It is (#1552), as observed on
# runs this repository already had, read on 2026-10-09:
#   run 37366070027 (`ci`), attempt 1: seven jobs `cancelled` (they never got a
#   runner), seven `success`, none `failure`. Attempt 2 ran the seven
#   cancelled jobs again, six of them independent of one another, and none of
#   the jobs they need (`changes`, `diff`, `test` kept their attempt-1 times).
#   Re-running one job re-runs it and its dependents, so no single job
#   explains that attempt: it is what `--failed` selects. Run 37367281258
#   (`proxy`) shows the same for two cancelled jobs.
# Which request made an attempt is not recorded by the API, so that is an
# inference from its shape; the shape is unambiguous. What was NOT observed:
# a job that ended any other way (`timed_out`, `action_required`, ...), and a
# run with no job at all (`startup_failure`, a run skipped as a whole). Those
# keep the whole run, which is always enough. So does a run in which the gate
# FAILED and is the only job that did not pass: the gate reads what `diff` and
# `test` wrote, and `--failed` would re-run it alone on the same results.
#
# WHICH GREEN RUN CLEARS WHAT. A post-merge run tests only what its own merge
# reaches (bazel-diff against the first parent), and a merge that reaches
# nothing passes without running a test. So a green run of a LATER commit says
# nothing about an earlier commit's failure, and never closes the issue: it
# would be closed by the next documentation-only merge while main was still
# broken. A commit is cleared by a green run of ITS OWN run (a re-run), and
# the issue closes when no commit recorded on it is failing. A failure that a
# later commit fixed is closed from that pull request (`Closes #<issue>`) or
# by hand. docs/runbook.md, "The post-merge run", says so to the reader.
#
# OUT OF ORDER. Runs finish in any order. An older commit's failure that
# arrives after a newer commit's green run is filed, for the reason above; the
# text says main has moved on and names its head. A closed issue is not
# written to: the next failure opens a new one.
#
# AT THE SAME TIME. Watchers of different runs are not serialised (one
# concurrency group for the workflow would drop a pending run), so another
# watcher can write between this one's reading of the issue and its own write.
# Nothing is decided from a reading older than this run's last write:
#   - a green run writes its pass, then reads the issue AGAIN and closes only
#     if nothing is failing in that second reading. Two commits that pass at
#     once each saw the other failing; the one that writes last sees both
#     passes and closes;
#   - after closing, it reads what it closed once more, and reopens it if a
#     failure is on it: one that was written between the second reading and
#     the close;
#   - a watcher that records a failure reads the issue's state after its
#     comment, and reopens the issue if it was closed in that moment.
# A failure comment is written either before the close, and then the closer's
# last reading has it, or after, and then its writer finds the issue closed.
# Either way the issue ends open. What remains rests on GitHub answering a
# read with every write that completed before it: a reading that lags can
# only be the closer's last one missing a comment, and then that comment's
# writer, who reads after the close, reopens. Both lagging at once would leave
# a failure on a closed issue; the other outcomes of a race are an issue left
# open with nothing failing (closed by hand, or by the next green re-run that
# finds it so) and a reopen done twice.
#
# THE RECORD. Each failure and each pass is a hidden marker on the issue,
#   <!-- main-post-merge-watch:failed|passed:<sha>:<run id>/<attempt> -->
# and the one with the highest run id and attempt for a commit is its state. Only the issue body and the
# comments written by `github-actions[bot]` are read, and only an issue that
# account opened counts as the rolling issue: anyone can open an issue with
# this title or comment a marker on a public repository. A run that is already
# recorded writes nothing, so a re-delivered event or a re-run of the watcher
# is harmless.
#
# NEVER DUPLICATED. The open issue is found by listing (not by search, whose
# index lags a few seconds behind a create). Two runs that fail at the same
# moment can both find nothing and both create: after a create the script
# lists again, and the run that finds an older issue moves its record there
# and closes its own. Issues left double by a failed API call are read as one
# and closed together.
#
#   scripts/main-post-merge-watch.sh decide <event> <branch> <head repository> \
#       <repository> <workflow path> <status> <conclusion> <main job conclusion> \
#       <other jobs: ok (each succeeded, was skipped or was cancelled) | failed | unknown>
#     prints `file: <why>`, `clear` or `ignore: <why>` (pure; the test drives it)
#   scripts/main-post-merge-watch.sh title | labels
#   scripts/main-post-merge-watch.sh
#     the workflow step. RUN_ID: the run to judge. GH_REPO, GH_TOKEN (issues:
#     write, actions: read, contents: read). DRY_RUN=true reads everything and
#     prints what it would write. WATCH_RUN_URL: this run, for the record.
#
# Exit 0 when the run was judged and the issue is as it should be; 1 when the
# API failed, or an earlier attempt of the run could not be read (the watcher's run goes red and can be re-run: it is idempotent);
# 2 on a RUN_ID that is not a number.
#
# Test: scripts/main_post_merge_watch_test.sh (//scripts:main_post_merge_watch_test).
set -euo pipefail

ISSUE_TITLE="main-post-merge: a commit on main failed its post-merge run"
# Labels the repository has (`gh label list`). A label that is gone must not
# cost the report: the issue is then filed without labels, with a warning.
ISSUE_LABELS=(bug ci)
WORKFLOW_PATH=".github/workflows/main.yaml"
# The job of main.yaml that decides the run (scripts/ci-gate.sh main).
GATE_JOB="main"
BOT='github-actions[bot]'
MARK="main-post-merge-watch"

is_sha() {
	case "$1" in
	*[!0-9a-f]* | "") return 1 ;;
	esac
	[ "${#1}" -eq 40 ]
}

# A post-merge run of main: a push to `main` of this repository, of main.yaml.
ours() { # event, branch, head repository, repository, workflow path
	[ "$1" = push ] && [ "$2" = main ] && [ "$3" = "$4" ] && [ "$5" = "$WORKFLOW_PATH" ]
}

decide() {
	local event="$1" branch="$2" head_repo="$3" repo="$4" path="$5" status="$6" conclusion="$7" gate="$8" others="$9"
	if ! ours "$event" "$branch" "$head_repo" "$repo" "$path"; then
		echo "ignore: not a post-merge run of main (event ${event:-none}, branch ${branch:-none}, repository ${head_repo:-none}, workflow ${path:-none})"
	elif [ "$status" != completed ]; then
		echo "ignore: the run is ${status:-in an unknown state}, not completed (a newer attempt is running; its own completion is judged, and the attempts before it are looked at now)"
	elif [ "$conclusion" = success ]; then
		echo clear
	elif [ "$conclusion" = cancelled ] && [ "$gate" = success ] && [ "$others" = ok ]; then
		echo "ignore: the run was cancelled, but its ${GATE_JOB} job succeeded and no other job failed: the commit was validated"
	else
		echo "file: the run ended ${conclusion:-with no conclusion}"
	fi
}

case "${1:-}" in
decide)
	[ "$#" -eq 10 ] || {
		echo "usage: $0 decide <event> <branch> <head repository> <repository> <workflow path> <status> <conclusion> <main job conclusion> <other jobs: ok|failed|unknown>" >&2
		exit 2
	}
	shift
	decide "$@"
	exit 0
	;;
title)
	printf '%s\n' "$ISSUE_TITLE"
	exit 0
	;;
labels)
	printf '%s\n' "${ISSUE_LABELS[@]}"
	exit 0
	;;
esac

die() {
	echo "::error title=main-post-merge-watch::$*"
	exit 1
}

id="${RUN_ID:-}"
[[ "$id" =~ ^[1-9][0-9]*$ ]] || {
	echo "::error title=main-post-merge-watch::RUN_ID is not a run id"
	exit 2
}
repo="${GH_REPO:?GH_REPO is not set}"
server="${GITHUB_SERVER_URL:-https://github.com}"
dry="${DRY_RUN:-false}"

# --- the run, from the API -----------------------------------------------------------------
fields="$(gh api "repos/${repo}/actions/runs/${id}" \
	--jq '[.event, .head_branch, .head_repository.full_name, .path, .status, .conclusion, .head_sha, .run_attempt] | .[] | (. // "") | tostring | gsub("[\\n\\r]"; " ")')" ||
	die "could not read run ${id}; re-run this job"
mapfile -t f <<<"$fields"
[ "${#f[@]}" -eq 8 ] || die "run ${id} was read as ${#f[@]} fields, not 8"
event="${f[0]}" branch="${f[1]}" head_repo="${f[2]}" path="${f[3]}" status="${f[4]}" conclusion="${f[5]}" sha="${f[6]}" attempt="${f[7]}"

if ! ours "$event" "$branch" "$head_repo" "$repo" "$path"; then
	echo "run ${id}: $(decide "$event" "$branch" "$head_repo" "$repo" "$path" "$status" "$conclusion" unknown unknown)"
	exit 0
fi
# Read from the API, and still checked before either is put in a request path
# or on the issue.
is_sha "$sha" || die "run ${id} names a commit that is not a sha"
[[ "$attempt" =~ ^[1-9][0-9]*$ ]] || die "run ${id} has an attempt that is not a number"

# The gate's conclusion among jobs given as name<TAB>conclusion lines.
gate_of() { awk -F'\t' -v g="$GATE_JOB" '$1 == g { c = $2 } END { print c }'; }
# Judge one attempt of the run: judged (what decide says), and for an attempt
# that did not pass its jobs, jobs_read, gate, gate_attempt.
judge() { # attempt, status, conclusion
	local k="$1" a earlier others
	jobs="" jobs_read=false gate="" gate_attempt=""
	judged="$(decide "$event" "$branch" "$head_repo" "$repo" "$path" "$2" "$3" unknown unknown)"
	[ "${judged%%:*}" = file ] || return 0
	# The jobs of the attempt: the gate's conclusion decides a cancelled run,
	# and the ones that did not pass are named on the issue. Not being able to
	# read them never stops a report.
	if jobs="$(gh api --paginate "repos/${repo}/actions/runs/${id}/attempts/${k}/jobs?per_page=100" \
		--jq '.jobs[] | [.name, (.conclusion // "none"), (.html_url // "")] | @tsv')"; then
		jobs_read=true
		gate="$(gate_of <<<"$jobs")"
		gate_attempt="$k"
		# A re-run of some of the jobs: the gate is not among this attempt's.
		# Its conclusion in the latest earlier attempt that has it stands.
		for ((a = k - 1; a >= 1 && ${#gate} == 0; a--)); do
			if earlier="$(gh api --paginate "repos/${repo}/actions/runs/${id}/attempts/${a}/jobs?per_page=100" \
				--jq '.jobs[] | [.name, (.conclusion // "none")] | @tsv')"; then
				gate="$(gate_of <<<"$earlier")"
				gate_attempt="$a"
			else
				echo "::warning title=main-post-merge-watch::the ${GATE_JOB} job is not among the jobs of attempt ${k} of run ${id}, and the jobs of attempt ${a} could not be listed; whether the commit was validated is not known"
				gate=unknown
			fi
		done
		# The other jobs of this attempt: one that neither succeeded, was
		# skipped nor was cancelled is a failure the run already holds. (The
		# gate needs no exception here: it only matters when it succeeded.)
		others="$(awk -F'\t' '$2 != "success" && $2 != "skipped" && $2 != "cancelled" { bad = 1 } END { print (bad ? "failed" : "ok") }' <<<"$jobs")"
		judged="$(decide "$event" "$branch" "$head_repo" "$repo" "$path" "$2" "$3" "$gate" "$others")"
	else
		echo "::warning title=main-post-merge-watch::could not list the jobs of attempt ${k} of run ${id}; reporting without them"
	fi
}

# Runs finish in any order: an entry says so when main is no longer at this
# commit. Read once.
moved="" moved_read=false
main_moved() {
	local head_sha
	[ "$moved_read" = false ] || return 0
	moved_read=true
	if head_sha="$(gh api "repos/${repo}/git/ref/heads/main" --jq '.object.sha // empty')" && is_sha "$head_sha"; then
		if [ "$head_sha" != "$sha" ]; then
			moved="\`main\` has moved on since: its head is now \`${head_sha:0:12}\`. A green run of a later commit does not clear this one: each post-merge run tests only what its own merge reaches."
		fi
	else
		echo "::notice title=main-post-merge-watch::could not read main's head; the report does not say whether main has moved on"
	fi
}

failed_marker() { printf '%s' "<!-- ${MARK}:failed:${sha}:${id}/$1 -->"; } # attempt

# The entry for the attempt judge was last called for: entry.
build_entry() { # attempt, conclusion, [why it is recorded now and not when it ended]
	local k="$1" c late="${3:-}" rows="" failed_jobs=0 other_jobs=0 beside_gate=0 name result url table g validated="" carried="" rerun
	c="$(printf '%s' "$2" | tr -cd 'a-z_')"
	# The jobs that did not pass, as table rows. A name is free text: keep plain
	# characters only, in a code span. A link is kept only when it is one of
	# this run's jobs.
	if [ "$jobs_read" = true ]; then
		while IFS=$'\t' read -r name result url; do
			result="$(printf '%s' "$result" | tr -cd 'a-z_')"
			case "$result" in
			success | skipped | "") continue ;;
			failure | cancelled) failed_jobs=$((failed_jobs + 1)) ;;
			*) other_jobs=$((other_jobs + 1)) ;;
			esac
			# The gate having FAILED is its verdict on the other jobs; the gate
			# cancelled is a job that did not run, like any other.
			{ [ "$name" = "$GATE_JOB" ] && [ "$result" = failure ]; } || beside_gate=$((beside_gate + 1))
			name="$(printf '%s' "$name" | tr -cd 'A-Za-z0-9 ._/()-' | tr -s ' ' | sed 's/^ //; s/ $//')"
			[ -n "$name" ] || name="(unnamed)"
			if [[ "$url" =~ ^${server}/${repo}/actions/runs/${id}/job/[0-9]+$ ]]; then
				rows+="| [\`${name}\`](${url}) | ${result} |"$'\n'
			else
				rows+="| \`${name}\` | ${result} |"$'\n'
			fi
		done <<<"$jobs"
	fi
	if [ -n "$rows" ]; then
		table="$(printf '| Job | Result |\n|---|---|\n%s' "$rows")"
	elif [ "$jobs_read" = true ]; then
		table="(no job of the run reported a failure: it did not start, or was skipped as a whole; read the run page)"
	else
		table="(the jobs of the run could not be listed; read the run page)"
	fi

	# Was the commit validated? The run's gate says, when the jobs were read. A
	# run is filed whenever it did not pass, and that includes a run whose gate
	# succeeded while another job (`refresh-pin-prs`) failed: then no target on
	# main is broken, and the reader must not go looking for one.
	g="$(printf '%s' "$gate" | tr -cd 'a-z_')"
	if [ "$jobs_read" = true ]; then
		[ "$gate_attempt" = "$k" ] || carried=" in attempt ${gate_attempt} and was not re-run in this attempt"
		case "$g" in
		unknown) validated="The \`${GATE_JOB}\` job, the gate of this run, is not among the jobs of this attempt (a re-run of some of the jobs), and an earlier attempt could not be read: whether the commit was validated is not known. Read the \`${GATE_JOB}\` job on the run page." ;;
		success) validated="The \`${GATE_JOB}\` job, the gate of this run, **succeeded**${carried}: the commit was validated (\`diff\` and \`test\` did their work), and this is not a broken target on \`main\`. What did not pass is another job of the run, named above; the run stays red, and this entry open, until that job passes." ;;
		"") validated="The \`${GATE_JOB}\` job, the gate of this run, did not run: the commit is not validated." ;;
		*) validated="The \`${GATE_JOB}\` job, the gate of this run, ended **${g}**${carried}: the commit is not validated." ;;
		esac
	fi

	# The command that can clear this commit. `gh run rerun --failed` asks
	# GitHub to re-run "the failed jobs and their dependent jobs", and that
	# takes the jobs that concluded `failure` and the ones that concluded
	# `cancelled` (WHICH COMMAND RE-RUNS IT, above). With no such job (nothing
	# started) it has nothing to select, no new attempt is made, and the entry
	# would never clear. So it is offered when at least one job did not pass
	# and each of those failed or was cancelled; in every other case, the whole
	# run (jobs that could not be listed count none as failed).
	# One more case for the whole run: the gate FAILED and is the only job that
	# did not pass. The gate does no work of its own; it failed on what `diff`
	# and `test` gave it (an output that was not set, a job skipped that had to
	# run), those jobs passed or were skipped, and `--failed` would re-run the
	# gate alone on the same results. (A gate that was only cancelled never
	# gave a verdict: `--failed` re-runs it, and that is enough.)
	if [ "$failed_jobs" -gt 0 ] && [ "$other_jobs" -eq 0 ] && [ "$beside_gate" -gt 0 ]; then
		rerun="If the cause is not the commit's own (a registry or GitHub answered 5xx, a step never ran, a job never got a runner or was cancelled), re-run the jobs that failed or were cancelled, and the jobs that depend on them: \`gh run rerun ${id} --failed\`"
	elif [ "$failed_jobs" -gt 0 ] && [ "$other_jobs" -eq 0 ]; then
		rerun="The \`${GATE_JOB}\` job is the only job that did not pass, and it decides from what the jobs before it wrote: read its log for which one. If the cause is not the commit's own, re-run the whole run: \`gh run rerun ${id}\`. Not \`--failed\`: that would re-run \`${GATE_JOB}\` alone, on the same results."
	else
		rerun="If the cause is not the commit's own (no job started, a job hit its time limit, the jobs could not be listed), re-run the whole run: \`gh run rerun ${id}\`. Not \`--failed\`: that re-runs the jobs that concluded \`failure\` or \`cancelled\`, which may be none of this run's."
	fi
	if [ "$c" = startup_failure ]; then
		rerun+=" If the workflow file of this commit is not valid, a re-run repeats it (a re-run executes the commit's own workflow): fix it in a new commit."
	fi

	main_moved
	entry="$(
		printf '%s\n' "$(failed_marker "$k")" \
			"The post-merge run of \`${sha:0:12}\` on \`main\` ended **${c:-unknown}**: ${server}/${repo}/actions/runs/${id}/attempts/${k}" "" \
			"$table" "" \
			"Commit: ${server}/${repo}/commit/${sha}"
		[ -z "$validated" ] || printf '\n%s\n' "$validated"
		[ -z "$late" ] || printf '\n%s\n' "$late"
		printf '\n%s\n' "$rerun"
		[ -z "$moved" ] || printf '\n%s\n' "$moved"
		printf '\n%s\n' "Recorded by ${WATCH_RUN_URL:-a run of main-post-merge-watch}."
	)"
}

judge "$attempt" "$status" "$conclusion"
verdict="$judged"
echo "run ${id} (${event:-no event} on ${branch:-no branch}, ${sha:-no commit}, attempt ${attempt:-?}, ${status:-no status}/${conclusion:-no conclusion}): ${verdict}"

# What this step has to record: the attempts that did not pass, oldest first.
to_attempt=() to_entry=() earlier_unread=false
if [ "${verdict%%:*}" = file ]; then
	build_entry "$attempt" "$conclusion"
	to_attempt=("$attempt") to_entry=("$entry")
fi
# THE ATTEMPTS BEFORE THIS ONE. An attempt's own watcher reads the run as it is
# when it runs: if a re-run had started by then, it found the run in progress
# and recorded nothing (and a pending watcher is replaced by the next one of
# the same run). So unless this attempt passed, every earlier attempt is judged
# here as well, back to the first, or to one that succeeded: what did not pass
# and is not yet on the issue is recorded now. A failed attempt is so recorded
# once, whichever watcher gets to it, and whether the attempt after it failed,
# was cancelled (its own verdict may be "validated, ignore") or is still
# running. One request per earlier attempt, and its jobs when it did not pass.
if [ "$verdict" != clear ]; then
	for ((k = attempt - 1; k >= 1; k--)); do
		if ! was="$(gh api "repos/${repo}/actions/runs/${id}/attempts/${k}" --jq '.conclusion // ""')"; then
			echo "::warning title=main-post-merge-watch::could not read attempt ${k} of run ${id}; the attempts up to it are not looked at"
			earlier_unread=true
			break
		fi
		[ "$was" != success ] || break
		judge "$k" completed "$was"
		[ "${judged%%:*}" = file ] || continue
		build_entry "$k" "$was" "Recorded late: this attempt ended before a newer attempt of the run began (attempt ${attempt} is the latest), and it was not on the issue."
		to_attempt=("$k" "${to_attempt[@]}") to_entry=("$entry" "${to_entry[@]}")
	done
fi
unread_earlier() {
	[ "$earlier_unread" = false ] || die "an earlier attempt of run ${id} could not be read, so a failure of it may be unrecorded; re-run this job"
}
if [ "$verdict" != clear ] && [ "${#to_attempt[@]}" -eq 0 ]; then
	unread_earlier
	exit 0
fi

run_url="${server}/${repo}/actions/runs/${id}/attempts/${attempt}"
marker_passed="<!-- ${MARK}:passed:${sha}:${id}/${attempt} -->"

# --- the issue -----------------------------------------------------------------------------
# Open issues with exactly the title, opened by the bot, oldest first.
open_issues() {
	gh api --paginate "repos/${repo}/issues?state=open&creator=github-actions%5Bbot%5D&per_page=100" \
		--jq ".[] | select(.pull_request == null) | select(.user.login == \"${BOT}\") | select(.title == \"${ISSUE_TITLE}\") | .number" | sort -n
}
# What the bot wrote on one of its issues (open_issues lists no other): the
# body, then its own comments, oldest first.
issue_text() {
	gh api "repos/${repo}/issues/$1" --jq '.body // ""' &&
		gh api --paginate "repos/${repo}/issues/$1/comments?per_page=100" --jq ".[] | select(.user.login == \"${BOT}\") | .body"
}
# The commits whose latest record is `failed`, in the order they are first
# named. Latest is by run id and attempt, not by position in the text: with
# two open copies of the issue, a pass written on the older one comes before
# the failure it answers, in the body of the newer one.
failing() {
	{ grep -oE -- "<!-- ${MARK}:(failed|passed):[0-9a-f]{40}:[0-9]+/[0-9]+ -->" || true; } |
		awk -F: '{
				split($4, at, "[/ ]"); run = at[1] + 0; attempt = at[2] + 0
				if (!($3 in seen)) { seen[$3] = 1; order[++n] = $3 }
				else if (run < r[$3] || (run == r[$3] && attempt < a[$3])) next
				state[$3] = $2; r[$3] = run; a[$3] = attempt
			}
			END { for (i = 1; i <= n; i++) if (state[order[i]] == "failed") print order[i] }'
}
short_list() { # shas on stdin -> `aaaaaaaaaaaa`, `bbbbbbbbbbbb`
	awk '{ printf "%s`%s`", (NR > 1 ? ", " : ""), substr($0, 1, 12) }'
}
# What a dry run prints is the writes the real step would make, one line
# `DRY RUN: would <open|comment on|close> ...` for each, in order. An issue it
# would have opened has no number: NEW_ISSUE stands for it afterwards, so that
# a second entry is a comment on that issue, as it is for real, and not a
# second issue.
NEW_ISSUE="new"
comment() { # issue, body
	if [ "$dry" = true ]; then
		# NEW_ISSUE: the issue this dry run said it would open.
		if [ "$1" = "$NEW_ISSUE" ]; then
			printf 'DRY RUN: would comment on the issue it would open:\n%s\n' "$2"
		else
			printf 'DRY RUN: would comment on #%s:\n%s\n' "$1" "$2"
		fi
	else
		gh api -X POST "repos/${repo}/issues/$1/comments" -f "body=$2" --jq '.id // empty' >/dev/null ||
			die "could not comment on #$1; re-run this job"
		echo "commented on #$1"
	fi
}
close_issue() { # issue, reason
	if [ "$dry" = true ]; then
		echo "DRY RUN: would close #$1 ($2)"
	else
		gh api -X PATCH "repos/${repo}/issues/$1" -f state=closed -f "state_reason=$2" --jq '.id // empty' >/dev/null ||
			die "could not close #$1; re-run this job"
		echo "closed #$1"
	fi
}

reopen_issue() { # issue, why
	gh api -X PATCH "repos/${repo}/issues/$1" -f state=open --jq '.id // empty' >/dev/null ||
		die "could not reopen #$1 ($2); reopen it by hand, or re-run this job"
	echo "reopened #$1: $2"
}
# Record a failure on an open issue, and make sure it is still open afterwards:
# a green run's watcher may have closed it in that moment.
record() { # issue, body
	local state
	comment "$1" "$2"
	[ "$dry" != true ] || return 0
	state="$(gh api "repos/${repo}/issues/$1" --jq '.state // ""')" ||
		die "the failure is recorded on #$1, but the issue could not be read back; check that it is open"
	[ "$state" = open ] || reopen_issue "$1" "it was closed while this failure was being recorded on it"
}
# The open issues and what is recorded on them, as of now: numbers, oldest,
# text, still.
read_issues() {
	local n part
	numbers="$(open_issues)" || die "could not list the open issues; re-run this job"
	oldest="$(head -n 1 <<<"$numbers")"
	text=""
	for n in $numbers; do
		part="$(issue_text "$n")" || die "could not read #${n}; re-run this job"
		text+="${part}"$'\n'
	done
	still="$(failing <<<"$text")"
}

read_issues

# --- a green run -----------------------------------------------------------------------------
if [ "$verdict" = clear ]; then
	if [ -z "$numbers" ]; then
		echo "no open \"${ISSUE_TITLE}\" issue; nothing to clear"
		exit 0
	fi
	if grep -qx -- "$sha" <<<"$still"; then
		still="$(grep -vx -- "$sha" <<<"$still" || true)"
		if [ -z "$still" ]; then
			rest="No other commit recorded here is failing: the issue closes, unless a failure is being recorded at this moment. The next failure after that opens a new issue."
		else
			rest="Still failing: $(short_list <<<"$still"). The issue stays open until each has passed, or is closed by hand."
		fi
		comment "$oldest" "$(printf '%s\n' "$marker_passed" \
			"The post-merge run of \`${sha:0:12}\` passes now: ${run_url}" "" "$rest")"
		# Decide from what is on the issue AFTER this write, not from the
		# reading before it: another watcher may have written since.
		# (A dry run wrote nothing: what it worked out above is all it has.)
		[ "$dry" = true ] || read_issues
		[ -z "$still" ] || echo "#${oldest} stays open. Still failing: $(short_list <<<"$still")"
	elif [ -n "$still" ]; then
		echo "${sha:0:12} is not recorded as failing: its green run leaves #${oldest} open (a run tests only what its own merge reaches). Still failing: $(short_list <<<"$still")"
	fi
	[ -z "$still" ] || exit 0
	# Nothing is failing: close every open copy. Also reached when an earlier
	# run recorded the last pass and then could not close.
	for n in $numbers; do
		close_issue "$n" completed
	done
	[ "$dry" != true ] || exit 0
	# A failure written between that reading and the close is now on a closed
	# issue. Look once more at what was closed, and undo the close if so.
	text=""
	for n in $numbers; do
		part="$(issue_text "$n")" || die "closed #${n}, but could not read it back; check that no failure was recorded on it meanwhile"
		text+="${part}"$'\n'
	done
	still="$(failing <<<"$text")"
	if [ -n "$still" ]; then
		for n in $numbers; do
			reopen_issue "$n" "$(short_list <<<"$still") was recorded as failing while it was being closed"
		done
	fi
	exit 0
fi

# --- attempts that did not pass --------------------------------------------------------------
# Record one entry: on the open issue, or on a new one, which is then the open
# issue for the next entry.
file_entry() { # entry
	local body created first l label_args=()
	if [ -n "$oldest" ]; then
		record "$oldest" "$1"
		return 0
	fi
	body="$(printf '%s\n\n%s\n\n%s\n' \
		"A commit on \`main\` did not pass its post-merge run (\`main-post-merge\`, .github/workflows/main.yaml). Nothing blocks on that run, so this issue is where it is seen." \
		"$1" \
		"_Filed automatically by \`main-post-merge-watch\`; this issue is reused while any commit named on it is failing. A commit is cleared when a re-run of its own run passes (each entry gives the command for its run), and the issue closes itself when none is left. A green run of another commit never closes it. When a later commit fixed the failure, close this issue from that pull request (\`Closes #<this issue>\`) or by hand. See docs/runbook.md, \"The post-merge run\"._")"
	if [ "$dry" = true ]; then
		printf 'DRY RUN: would open "%s" (labels: %s) with:\n%s\n' "$ISSUE_TITLE" "${ISSUE_LABELS[*]}" "$body"
		oldest="$NEW_ISSUE"
		return 0
	fi
	for l in "${ISSUE_LABELS[@]}"; do
		label_args+=(-f "labels[]=${l}")
	done
	if ! created="$(gh api -X POST "repos/${repo}/issues" -f "title=${ISSUE_TITLE}" -f "body=${body}" "${label_args[@]}" --jq '.number')"; then
		echo "::warning title=main-post-merge-watch::could not open the issue with the labels ${ISSUE_LABELS[*]} (is a label gone?); opening it without labels"
		created="$(gh api -X POST "repos/${repo}/issues" -f "title=${ISSUE_TITLE}" -f "body=${body}" --jq '.number')" ||
			die "could not open the issue; re-run this job"
	fi
	[[ "$created" =~ ^[0-9]+$ ]] || die "the issue was opened, but its number was not returned"
	echo "opened #${created}"
	oldest="$created"

	# Another run may have opened one in the same moment. The older issue wins.
	if ! numbers="$(open_issues)"; then
		echo "::warning title=main-post-merge-watch::could not list the issues again; if two were opened at once, the next run reads them as one"
		return 0
	fi
	first="$(head -n 1 <<<"$numbers")"
	if [ -n "$first" ] && [ "$first" -lt "$created" ]; then
		record "$first" "$1"
		comment "$created" "Duplicate of #${first}: two post-merge runs failed at the same moment. The failure is recorded there."
		close_issue "$created" not_planned
		oldest="$first"
	fi
}

for i in "${!to_attempt[@]}"; do
	if grep -qF -- "$(failed_marker "${to_attempt[$i]}")" <<<"$text"; then
		echo "attempt ${to_attempt[$i]} of run ${id} is already recorded on #${oldest}; nothing to write"
	else
		file_entry "${to_entry[$i]}"
	fi
done
unread_earlier
