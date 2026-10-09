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
#   failure, timed_out      the commit failed its post-merge run.
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
#                           own gate, the `main` job, succeeded: then `diff`
#                           and `test` did their work and the commit is
#                           validated, and what was cancelled is something
#                           else (run 37337609845: `refresh-pin-prs` waited
#                           3.5 hours for a runner). If the jobs cannot be
#                           read, it is filed.
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
# text says main has moved on and names its head. A closed issue is never
# reopened or written to: the next failure opens a new one.
#
# THE RECORD. Each failure and each pass is a hidden marker on the issue,
#   <!-- main-post-merge-watch:failed|passed:<sha>:<run id>/<attempt> -->
# and the last one for a commit is its state. Only the issue body and the
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
#       <repository> <workflow path> <status> <conclusion> <main job conclusion>
#     prints `file: <why>`, `clear` or `ignore: <why>` (pure; the test drives it)
#   scripts/main-post-merge-watch.sh title | labels
#   scripts/main-post-merge-watch.sh
#     the workflow step. RUN_ID: the run to judge. GH_REPO, GH_TOKEN (issues:
#     write, actions: read, contents: read). DRY_RUN=true reads everything and
#     prints what it would write. WATCH_RUN_URL: this run, for the record.
#
# Exit 0 when the run was judged and the issue is as it should be; 1 when the
# API failed (the watcher's run goes red and can be re-run: it is idempotent);
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

decide() {
	local event="$1" branch="$2" head_repo="$3" repo="$4" path="$5" status="$6" conclusion="$7" gate="$8"
	if [ "$event" != push ] || [ "$branch" != main ] || [ "$head_repo" != "$repo" ] || [ "$path" != "$WORKFLOW_PATH" ]; then
		echo "ignore: not a post-merge run of main (event ${event:-none}, branch ${branch:-none}, repository ${head_repo:-none}, workflow ${path:-none})"
	elif [ "$status" != completed ]; then
		echo "ignore: the run is ${status:-in an unknown state}, not completed (a newer attempt is running; its own completion is judged)"
	elif [ "$conclusion" = success ]; then
		echo clear
	elif [ "$conclusion" = cancelled ] && [ "$gate" = success ]; then
		echo "ignore: the run was cancelled, but its ${GATE_JOB} job succeeded: the commit was validated"
	else
		echo "file: the run ended ${conclusion:-with no conclusion}"
	fi
}

case "${1:-}" in
decide)
	[ "$#" -eq 9 ] || {
		echo "usage: $0 decide <event> <branch> <head repository> <repository> <workflow path> <status> <conclusion> <main job conclusion>" >&2
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

verdict="$(decide "$event" "$branch" "$head_repo" "$repo" "$path" "$status" "$conclusion" unknown)"
if [ "${verdict%%:*}" != ignore ]; then
	# Read from the API, and still checked before either is put in a request
	# path or on the issue.
	is_sha "$sha" || die "run ${id} names a commit that is not a sha"
	[[ "$attempt" =~ ^[1-9][0-9]*$ ]] || die "run ${id} has an attempt that is not a number"
fi
jobs="" jobs_read=false
if [ "${verdict%%:*}" = file ]; then
	# The jobs of this attempt: the gate's conclusion decides a cancelled run,
	# and the ones that did not pass are named on the issue. Not being able to
	# read them never stops a report.
	if jobs="$(gh api --paginate "repos/${repo}/actions/runs/${id}/attempts/${attempt}/jobs?per_page=100" \
		--jq '.jobs[] | [.name, (.conclusion // "none"), (.html_url // "")] | @tsv')"; then
		jobs_read=true
		gate="$(awk -F'\t' -v g="$GATE_JOB" '$1 == g { c = $2 } END { print c }' <<<"$jobs")"
		verdict="$(decide "$event" "$branch" "$head_repo" "$repo" "$path" "$status" "$conclusion" "$gate")"
	else
		echo "::warning title=main-post-merge-watch::could not list the jobs of run ${id}; reporting without them"
	fi
fi
echo "run ${id} (${event:-no event} on ${branch:-no branch}, ${sha:-no commit}, attempt ${attempt:-?}, ${status:-no status}/${conclusion:-no conclusion}): ${verdict}"
[ "${verdict%%:*}" != ignore ] || exit 0

conclusion="$(printf '%s' "$conclusion" | tr -cd 'a-z_')"
run_url="${server}/${repo}/actions/runs/${id}/attempts/${attempt}"
marker_failed="<!-- ${MARK}:failed:${sha}:${id}/${attempt} -->"
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
# The commits whose last record is `failed`, in the order they first failed.
failing() {
	{ grep -oE -- "<!-- ${MARK}:(failed|passed):[0-9a-f]{40}:[0-9]+/[0-9]+ -->" || true; } |
		awk -F: '{ state[$3] = $2; if (!($3 in seen)) { seen[$3] = 1; order[++n] = $3 } }
			END { for (i = 1; i <= n; i++) if (state[order[i]] == "failed") print order[i] }'
}
short_list() { # shas on stdin -> `aaaaaaaaaaaa`, `bbbbbbbbbbbb`
	awk '{ printf "%s`%s`", (NR > 1 ? ", " : ""), substr($0, 1, 12) }'
}
comment() { # issue, body
	if [ "$dry" = true ]; then
		printf 'DRY RUN: would comment on #%s:\n%s\n' "$1" "$2"
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

numbers="$(open_issues)" || die "could not list the open issues; nothing was written. Re-run this job"
oldest="$(head -n 1 <<<"$numbers")"
text=""
for n in $numbers; do
	part="$(issue_text "$n")" || die "could not read #${n}; nothing was written. Re-run this job"
	text+="${part}"$'\n'
done
still="$(failing <<<"$text")"

# --- a green run -----------------------------------------------------------------------------
if [ "$verdict" = clear ]; then
	if [ -z "$numbers" ]; then
		echo "no open \"${ISSUE_TITLE}\" issue; nothing to clear"
		exit 0
	fi
	if grep -qx -- "$sha" <<<"$still"; then
		still="$(grep -vx -- "$sha" <<<"$still" || true)"
		if [ -z "$still" ]; then
			rest="Nothing recorded here is failing any more; closing. The next failure opens a new issue."
		else
			rest="Still failing: $(short_list <<<"$still"). The issue stays open until each has passed, or is closed by hand."
		fi
		comment "$oldest" "$(printf '%s\n' "$marker_passed" \
			"The post-merge run of \`${sha:0:12}\` passes now: ${run_url}" "" "$rest")"
	elif [ -n "$still" ]; then
		echo "${sha:0:12} is not recorded as failing: its green run leaves #${oldest} open (a run tests only what its own merge reaches). Still failing: $(short_list <<<"$still")"
	fi
	[ -z "$still" ] || exit 0
	# Nothing is failing: close every open copy. Also reached when an earlier
	# run recorded the last pass and then could not close.
	for n in $numbers; do
		close_issue "$n" completed
	done
	exit 0
fi

# --- a run that did not pass -----------------------------------------------------------------
if grep -qF -- "$marker_failed" <<<"$text"; then
	echo "attempt ${attempt} of run ${id} is already recorded on #${oldest}; nothing to write"
	exit 0
fi

# The jobs that did not pass, as table rows. A name is free text: keep plain
# characters only, in a code span. A link is kept only when it is one of this
# run's jobs.
rows=""
if [ "$jobs_read" = true ]; then
	while IFS=$'\t' read -r name result url; do
		result="$(printf '%s' "$result" | tr -cd 'a-z_')"
		case "$result" in success | skipped | "") continue ;; esac
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

# Runs finish in any order: say so when main is no longer at this commit.
moved=""
if head_sha="$(gh api "repos/${repo}/git/ref/heads/main" --jq '.object.sha // empty')" && is_sha "$head_sha"; then
	if [ "$head_sha" != "$sha" ]; then
		moved="\`main\` has moved on since: its head is now \`${head_sha:0:12}\`. A green run of a later commit does not clear this one: each post-merge run tests only what its own merge reaches."
	fi
else
	echo "::notice title=main-post-merge-watch::could not read main's head; the report does not say whether main has moved on"
fi

entry="$(
	printf '%s\n' "$marker_failed" \
		"The post-merge run of \`${sha:0:12}\` on \`main\` ended **${conclusion:-unknown}**: ${run_url}" "" \
		"$table" "" \
		"Commit: ${server}/${repo}/commit/${sha}"
	[ -z "$moved" ] || printf '\n%s\n' "$moved"
	printf '\n%s\n' "Recorded by ${WATCH_RUN_URL:-a run of main-post-merge-watch}."
)"

if [ -n "$oldest" ]; then
	comment "$oldest" "$entry"
	exit 0
fi

body="$(printf '%s\n\n%s\n\n%s\n' \
	"A commit on \`main\` did not pass its post-merge run (\`main-post-merge\`, .github/workflows/main.yaml). Nothing blocks on that run, so this issue is where it is seen." \
	"$entry" \
	"_Filed automatically by \`main-post-merge-watch\`; this issue is reused while any commit named on it is failing. A commit is cleared when a re-run of its own run passes (\`gh run rerun <run id> --failed\`), and the issue closes itself when none is left. A green run of another commit never closes it. When a later commit fixed the failure, close this issue from that pull request (\`Closes #<this issue>\`) or by hand. See docs/runbook.md, \"The post-merge run\"._")"
if [ "$dry" = true ]; then
	printf 'DRY RUN: would open "%s" (labels: %s) with:\n%s\n' "$ISSUE_TITLE" "${ISSUE_LABELS[*]}" "$body"
	exit 0
fi
label_args=()
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

# Another run may have opened one in the same moment. The older issue wins.
if ! numbers="$(open_issues)"; then
	echo "::warning title=main-post-merge-watch::could not list the issues again; if two were opened at once, the next run reads them as one"
	exit 0
fi
oldest="$(head -n 1 <<<"$numbers")"
if [ -n "$oldest" ] && [ "$oldest" -lt "$created" ]; then
	comment "$oldest" "$entry"
	comment "$created" "Duplicate of #${oldest}: two post-merge runs failed at the same moment. The failure is recorded there."
	close_issue "$created" not_planned
fi
