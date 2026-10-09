#!/usr/bin/env bash
# Hermetic test of scripts/main-post-merge-watch.sh and of the workflow that
# runs it, .github/workflows/main-watch.yaml (#1506). No network, no GitHub:
# `gh` is a fake that keeps issues, comments, one run and its jobs as JSON in a
# scratch directory, answers the REST paths the script asks for, and applies
# the script's own `--jq` filters with the Bazel-pinned jq, so a filter that
# stopped selecting (the author, the exact title) hands the script the decoys.
#
#   1. the decision, as literals: which runs file, which clear, which are ignored;
#   2. the step through the fake: one issue, reused; which green run clears a
#      commit and which closes the issue; an older failure after a newer green
#      run; duplicates; decoys written by someone else; a failing API; dry run;
#   3. what reaches the issue from the run: nothing that is not validated;
#   4. the workflow file: trigger, permissions, pins, no expression in a script,
#      and the facts about main.yaml the decision rests on.
#
# Run: bazel test //scripts:main_post_merge_watch_test
#      bash scripts/main_post_merge_watch_test.sh with jq on PATH.
# shellcheck disable=SC2016 # single-quoted $names are jq variables, workflow
# expressions or the fake's own shell, never expansions of this one.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/main-post-merge-watch.sh"
WORKFLOW="${TEST_SRCDIR:-}/${TEST_WORKSPACE:-_main}/.github/workflows/main-watch.yaml"
[ -f "$WORKFLOW" ] || WORKFLOW="$HERE/../.github/workflows/main-watch.yaml"
MAIN_WORKFLOW="$(dirname -- "$WORKFLOW")/main.yaml"
for f in "$SCRIPT" "$WORKFLOW" "$MAIN_WORKFLOW"; do
	[ -f "$f" ] || {
		echo "FAIL: $f not found"
		exit 1
	}
done

# Under Bazel, JQ_RLOCATIONPATH names the pinned jq in the runfiles.
if [ -n "${JQ_RLOCATIONPATH:-}" ]; then
	JQ="${TEST_SRCDIR:-${RUNFILES_DIR:-$PWD/..}}/${JQ_RLOCATIONPATH}"
fi
JQ="${JQ:-$(command -v jq)}"
[ -x "$JQ" ] || {
	echo "FAIL: no jq (JQ=${JQ})"
	exit 1
}
export JQ

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FAILS=0
pass() { echo "PASS  $*"; }
fail() {
	echo "FAIL  $*"
	FAILS=$((FAILS + 1))
}
expect() { # name, got, want
	if [ "$2" = "$3" ]; then pass "$1"; else fail "$1: got '$2', want '$3'"; fi
}

TITLE="main-post-merge: a commit on main failed its post-merge run"
BOT='github-actions[bot]'
PATH_MAIN=".github/workflows/main.yaml"
A="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
B="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
C="cccccccccccccccccccccccccccccccccccccccc"

# --- 1. the decision --------------------------------------------------------------------------
# decide <event> <branch> <head repository> <repository> <workflow path> <status> <conclusion> <main job> <other jobs>
want() { # file|clear|ignore, name, the nine arguments
	local want="$1" name="$2" got
	shift 2
	got="$(bash "$SCRIPT" decide "$@")"
	case "$want:$got" in
	file:file:\ * | clear:clear | ignore:ignore:\ *) pass "decide: $name -> $got" ;;
	*) fail "decide: $name: got '$got', want $want" ;;
	esac
}
ours=(push main o/r o/r "$PATH_MAIN" completed)
want clear "a green run" "${ours[@]}" success "" ""
want file "a failed run" "${ours[@]}" failure failure ""
want file "a failed run whose gate passed (another job failed)" "${ours[@]}" failure success ""
want file "a run that timed out" "${ours[@]}" timed_out "" ""
want file "a run that never started (an invalid workflow file)" "${ours[@]}" startup_failure "" ""
want file "a run that was skipped: nothing validated the commit" "${ours[@]}" skipped "" ""
want file "a run waiting for an approval" "${ours[@]}" action_required "" ""
want file "a conclusion this script has never heard of: fail closed" "${ours[@]}" stale "" ""
want file "a completed run with no conclusion: fail closed" "${ours[@]}" "" "" ""
# main.yaml has no concurrency group: nothing supersedes a run. A cancelled
# run is a job that hit its time limit, or a person.
want file "cancelled, and its gate failed" "${ours[@]}" cancelled failure ok
want file "cancelled, and its gate was cancelled too" "${ours[@]}" cancelled cancelled ok
want file "cancelled, and its gate never ran" "${ours[@]}" cancelled "" ok
want file "cancelled, and the jobs could not be read: fail closed" "${ours[@]}" cancelled unknown unknown
want ignore "cancelled, but its gate passed: the commit was validated" "${ours[@]}" cancelled success ok
# The exception is for a run whose other work was cancelled, not for one that
# already holds a failure: that is the failure-with-a-green-gate case above,
# cut short. Anything not known about the other jobs files.
want file "cancelled, its gate passed, but another job failed" "${ours[@]}" cancelled success failed
want file "cancelled, its gate passed, and the other jobs are not known" "${ours[@]}" cancelled success unknown
want file "cancelled, its gate passed, and nothing is said of the other jobs" "${ours[@]}" cancelled success ""
want ignore "a run that is not finished (a newer attempt is running)" push main o/r o/r "$PATH_MAIN" in_progress "" "" ""
want ignore "a queued run" push main o/r o/r "$PATH_MAIN" queued "" "" ""
# Not a post-merge run of main: whatever it concluded.
for c in success failure; do
	want ignore "a pull request's run from a fork whose branch is called main ($c)" pull_request main mallory/r o/r "$PATH_MAIN" completed "$c" "" ""
	want ignore "a pull request's run in this repository ($c)" pull_request main o/r o/r "$PATH_MAIN" completed "$c" "" ""
	want ignore "a push to another branch ($c)" push feature o/r o/r "$PATH_MAIN" completed "$c" "" ""
	want ignore "a push in another repository ($c)" push main mallory/r o/r "$PATH_MAIN" completed "$c" "" ""
	want ignore "another workflow that took the name ($c)" push main o/r o/r .github/workflows/other.yaml completed "$c" "" ""
	want ignore "a manual run ($c)" workflow_dispatch main o/r o/r "$PATH_MAIN" completed "$c" "" ""
done
if bash "$SCRIPT" decide push main >/dev/null 2>&1; then
	fail "decide with too few arguments must be refused"
else
	pass "decide with too few arguments is refused"
fi
expect "the title" "$(bash "$SCRIPT" title)" "$TITLE"
expect "the labels are ones the repository has" "$(bash "$SCRIPT" labels | tr '\n' ' ')" "bug ci "

# --- the fake gh ------------------------------------------------------------------------------
# $FAKE_STATE/issues.json: [{number, title, state, state_reason, user.login,
# labels, body, pull_request?, comments: [{user.login, body}]}], run.json: the
# run, jobs.json: {jobs: [...]}. $FAKE_DOWN: the calls that answer 502.
mkdir -p "$TMP/bin"
cat >"$TMP/bin/gh" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$FAKE_LOG"
[ "$1" = api ] || { echo "fake gh: only 'gh api' is expected, got: $*" >&2; exit 1; }
shift
method=GET jqx="" path="" title="" body="" state="" reason="" labels="[]"
while [ "$#" -gt 0 ]; do
	case "$1" in
	--paginate) ;;
	-X) method="$2"; shift ;;
	--jq) jqx="$2"; shift ;;
	-f)
		case "$2" in
		title=*) title="${2#title=}" ;;
		body=*) body="${2#body=}" ;;
		state=*) state="${2#state=}" ;;
		state_reason=*) reason="${2#state_reason=}" ;;
		'labels[]='*) labels="$("$JQ" -c --arg l "${2#labels\[\]=}" '. + [$l]' <<<"$labels")" ;;
		*) echo "fake gh: unexpected field $2" >&2; exit 1 ;;
		esac
		shift
		;;
	-*) echo "fake gh: unexpected flag $1" >&2; exit 1 ;;
	*) path="$1" ;;
	esac
	shift
done
out() { if [ -n "$jqx" ]; then "$JQ" -r "$jqx"; else cat; fi; }
down() { case " ${FAKE_DOWN:-} " in *" $1 "*) echo "gh: HTTP 502" >&2; exit 1 ;; esac; }
# `read-after-comment`, `read-after-close`: an issue cannot be read once this
# step has written that.
read_issue() {
	down read
	[ ! -e "$FAKE_STATE/commented" ] || down read-after-comment
	[ ! -e "$FAKE_STATE/closed" ] || down read-after-close
}
I="$FAKE_STATE/issues.json"
edit() { "$JQ" "$@" "$I" >"$I.next" && mv "$I.next" "$I"; }
# Another watcher, running at the same time: $FAKE_HOOK names the moment
# (before-comment, after-comment, before-close), $FAKE_HOOK_JQ what it does to
# the issues. Once per step.
hook() {
	[ "${FAKE_HOOK:-}" = "$1" ] && [ ! -e "$FAKE_STATE/hook.done" ] || return 0
	: >"$FAKE_STATE/hook.done"
	edit "$FAKE_HOOK_JQ"
}
number="${path#repos/o/r/issues/}"
number="${number%%/*}"
case "$method $path" in
"GET repos/o/r/actions/runs/${FAKE_RUN_ID}/attempts/"*"/jobs?per_page=100")
	# jobs.<attempt>.json when the test wrote one for that attempt, else
	# jobs.json. `jobs@<attempt>`: that attempt's jobs cannot be listed.
	at="${path#*/attempts/}"
	at="${at%%/*}"
	down jobs
	down "jobs@${at}"
	if [ -e "$FAKE_STATE/jobs.${at}.json" ]; then out <"$FAKE_STATE/jobs.${at}.json"; else out <"$FAKE_STATE/jobs.json"; fi
	;;
"GET repos/o/r/actions/runs/${FAKE_RUN_ID}/attempts/"*)
	# One earlier attempt of the run: run.<attempt>.json when the test wrote
	# one, else an attempt that succeeded. `run@<attempt>`: it cannot be read.
	at="${path#*/attempts/}"
	down "run@${at}"
	if [ -e "$FAKE_STATE/run.${at}.json" ]; then out <"$FAKE_STATE/run.${at}.json"; else echo '{"status": "completed", "conclusion": "success"}' | out; fi
	;;
"GET repos/o/r/actions/runs/${FAKE_RUN_ID}")
	down run
	out <"$FAKE_STATE/run.json"
	;;
"GET repos/o/r/git/ref/heads/main")
	down head
	"$JQ" -n --arg s "$FAKE_HEAD" '{object: {sha: $s}}' | out
	;;
"GET repos/o/r/issues?state=open&creator=github-actions%5Bbot%5D&per_page=100")
	# Every open issue, whoever wrote it: the script's filter has to hold
	# without the server's.
	down list
	"$JQ" '[.[] | select(.state == "open") | del(.comments)]' "$I" | out
	;;
"GET repos/o/r/issues/${number}/comments?per_page=100")
	read_issue
	"$JQ" --argjson n "$number" '.[] | select(.number == $n) | .comments' "$I" | out
	;;
"GET repos/o/r/issues/${number}")
	read_issue
	"$JQ" --argjson n "$number" '.[] | select(.number == $n) | del(.comments)' "$I" | out
	;;
"POST repos/o/r/issues")
	down create
	if [ -n "${FAKE_NO_LABELS:-}" ] && [ "$labels" != "[]" ]; then
		echo 'gh: Validation Failed (HTTP 422): label does not exist' >&2
		exit 1
	fi
	if [ -n "${FAKE_RACE:-}" ]; then
		# Another run filed the same issue between this one's listing and its create.
		edit --arg t "$title" --arg bot 'github-actions[bot]' \
			'. + [{number: ((map(.number) | max // 100) + 1), title: $t, state: "open", state_reason: null, user: {login: $bot}, labels: [], body: "<!-- main-post-merge-watch:failed:dddddddddddddddddddddddddddddddddddddddd:9/1 -->", comments: []}]'
	fi
	edit --arg t "$title" --arg b "$body" --argjson l "$labels" --arg bot 'github-actions[bot]' \
		'. + [{number: ((map(.number) | max // 100) + 1), title: $t, state: "open", state_reason: null, user: {login: $bot}, labels: $l, body: $b, comments: []}]'
	"$JQ" 'max_by(.number)' "$I" | out
	;;
"POST repos/o/r/issues/${number}/comments")
	down comment
	hook before-comment
	edit --argjson n "$number" --arg b "$body" --arg bot 'github-actions[bot]' \
		'map(if .number == $n then .comments += [{user: {login: $bot}, body: $b}] else . end)'
	: >"$FAKE_STATE/commented"
	hook after-comment
	echo '{}' | out
	;;
"PATCH repos/o/r/issues/${number}")
	if [ "$state" = closed ]; then down close; else down reopen; fi
	[ "$state" != closed ] || hook before-close
	edit --argjson n "$number" --arg s "$state" --arg r "$reason" \
		'map(if .number == $n then .state = $s | .state_reason = (if $r == "" then null else $r end) else . end)'
	[ "$state" != closed ] || : >"$FAKE_STATE/closed"
	echo '{}' | out
	;;
*)
	echo "fake gh: unexpected $method $path" >&2
	exit 1
	;;
esac
FAKE
chmod +x "$TMP/bin/gh"

S="$TMP/state"
mkdir -p "$S"
RUN_ID=4242
HEAD="$A"
no_issues() { echo '[]' >"$S/issues.json"; } # the run and its attempts stay
reset() {
	no_issues
	rm -f "$S"/jobs.*.json "$S"/run.*.json
}
# run <sha> <conclusion> [attempt] [status] [event] [branch] [head repo] [path]
run() {
	"$JQ" -n --arg sha "$1" --arg c "$2" --argjson a "${3:-1}" --arg st "${4:-completed}" \
		--arg e "${5:-push}" --arg b "${6:-main}" --arg hr "${7:-o/r}" --arg p "${8:-$PATH_MAIN}" \
		'{event: $e, head_branch: $b, head_repository: {full_name: $hr}, path: $p, status: $st,
		  conclusion: (if $c == "" then null else $c end), head_sha: $sha, run_attempt: $a}' >"$S/run.json"
}
# jobs <name>=<conclusion>...
jobs() {
	local j n=0 list='[]'
	for j in "$@"; do
		n=$((n + 1))
		list="$("$JQ" -c --arg name "${j%%=*}" --arg c "${j#*=}" --arg u "https://github.com/o/r/actions/runs/${RUN_ID}/job/90${n}" \
			'. + [{name: $name, conclusion: $c, html_url: $u}]' <<<"$list")"
	done
	"$JQ" -n --argjson j "$list" '{jobs: $j}' >"$S/jobs.json"
}
# jobs_at <attempt> <name>=<conclusion>...: the jobs of that attempt only.
jobs_at() {
	local at="$1"
	shift
	jobs "$@"
	mv "$S/jobs.json" "$S/jobs.${at}.json"
}
# run_at <attempt> <conclusion>: how that earlier attempt of the run ended.
run_at() {
	"$JQ" -n --arg c "$2" '{status: "completed", conclusion: (if $c == "" then null else $c end)}' >"$S/run.$1.json"
}
# issue <number> <title> <author> [body] [pull request: yes]
issue() {
	"$JQ" --argjson n "$1" --arg t "$2" --arg u "$3" --arg b "${4:-}" --arg pr "${5:-}" \
		'. + [{number: $n, title: $t, state: "open", state_reason: null, user: {login: $u}, labels: [], body: $b, comments: []}
		      | if $pr == "yes" then .pull_request = {url: "x"} else . end]' "$S/issues.json" >"$S/i.next" && mv "$S/i.next" "$S/issues.json"
}
comment() { # number, author, body
	"$JQ" --argjson n "$1" --arg u "$2" --arg b "$3" \
		'map(if .number == $n then .comments += [{user: {login: $u}, body: $b}] else . end)' "$S/issues.json" >"$S/i.next" && mv "$S/i.next" "$S/issues.json"
}
q() { "$JQ" -r "$@" "$S/issues.json"; }
open_numbers() { q '[.[] | select(.state == "open") | .number] | sort | map(tostring) | join(" ")'; }
ncomments() { q --argjson n "$1" '.[] | select(.number == $n) | .comments | length'; }
last_comment() { q --argjson n "$1" '.[] | select(.number == $n) | .comments | last | .body'; }
writes() { grep -cE -- '-X (POST|PATCH) ' "$TMP/gh.log" || true; }
# step <want exit> <name> [VAR=value...]: the workflow step; stdout+stderr in $TMP/log.
step() {
	local want="$1" name="$2" rc
	shift 2
	: >"$TMP/gh.log"
	rm -f "$S/hook.done" "$S/commented" "$S/closed"
	env PATH="$TMP/bin:$PATH" GH_TOKEN=x GH_REPO=o/r GITHUB_SERVER_URL=https://github.com \
		FAKE_LOG="$TMP/gh.log" FAKE_STATE="$S" FAKE_RUN_ID="$RUN_ID" FAKE_HEAD="$HEAD" \
		RUN_ID="$RUN_ID" DRY_RUN=false WATCH_RUN_URL=https://github.com/o/r/actions/runs/7 "$@" \
		bash "$SCRIPT" >"$TMP/log" 2>&1
	rc=$?
	if [ "$rc" -ne "$want" ]; then
		fail "$name: exit $rc, wanted $want: $(cat "$TMP/log")"
		return 1
	fi
}
check() {
	local what="$1"
	shift
	if "$@"; then pass "$what"; else
		fail "$what"
		sed 's/^/      /' "$TMP/log" "$TMP/gh.log"
		"$JQ" -c '.[]' "$S/issues.json" | sed 's/^/      /'
	fi
}
body_of() { q --argjson n "$1" '.[] | select(.number == $n) | .body'; }
all_of() { q --argjson n "$1" '.[] | select(.number == $n) | ([.body] + [.comments[].body]) | join("\n")'; } # body and comments
# The attempts recorded as failed, `<run>/<attempt>` each, in order, one per
# record: on the open issues, and on every issue.
MARKS='([.body] + [.comments[].body]) | .[] | scan("main-post-merge-watch:failed:[0-9a-f]{40}:([0-9]+/[0-9]+)") | .[0]'
failed_open() { q "[.[] | select(.state == \"open\") | ${MARKS}] | sort | join(\" \")"; }
failed_twice() { q "[.[] | ${MARKS}] | group_by(.) | map(select(length > 1) | .[0]) | join(\" \")"; }
has() { grep -qF -- "$2" <<<"$1"; }
lacks() { ! grep -qF -- "$2" <<<"$1"; }

# --- 2. the step ------------------------------------------------------------------------------
# A fails: one issue, with what a reader needs.
reset
RUN_ID=4242
HEAD="$A"
run "$A" failure
jobs diff=success test=failure main=failure refresh-pin-prs=success lint=skipped
step 0 "A fails, nothing open" &&
	check "A fails: one issue is opened" test "$(open_numbers)" = 101
body="$(body_of 101)"
expect "A fails: under the fixed title" "$(q '.[0].title')" "$TITLE"
expect "A fails: labelled bug and ci" "$(q '.[0].labels | join(" ")')" "bug ci"
check "A fails: the body names the commit" has "$body" "$A"
check "A fails: ... the run, by its attempt" has "$body" "https://github.com/o/r/actions/runs/4242/attempts/1"
check "A fails: ... the conclusion" has "$body" "ended **failure**"
check "A fails: ... the failed job, linked" has "$body" '| [`test`](https://github.com/o/r/actions/runs/4242/job/902) | failure |'
check "A fails: ... and the gate" has "$body" '| [`main`](https://github.com/o/r/actions/runs/4242/job/903) | failure |'
check "A fails: a job that succeeded is not listed" lacks "$body" '`diff`'
check "A fails: a job that was skipped is not listed" lacks "$body" '`lint`'
check "A fails: the body records it for the next run to read" has "$body" "<!-- main-post-merge-watch:failed:${A}:4242/1 -->"
check "A fails: A is main's head, so the body does not say main moved on" lacks "$body" "has moved on"
check "A fails: the body says what closes the issue" has "$body" "docs/runbook.md"
check "A fails: the body names the run that filed it" has "$body" "https://github.com/o/r/actions/runs/7"
# What a reader does next. A job concluded `failure` and nothing else did not
# pass: re-running the failed jobs is enough, and the command names this run.
check "A fails: every job that did not pass failed, so the command re-runs the failed jobs" has "$body" '`gh run rerun 4242 --failed`'
check "A fails: no command with a placeholder for the run id" lacks "$body" 'gh run rerun <'
check "A fails: the gate failed, and the body says the commit is not validated" has "$body" 'The `main` job, the gate of this run, ended **failure**: the commit is not validated'

# The same event again (a re-delivered event, a re-run of the watcher): nothing.
step 0 "the same failure again" &&
	check "delivered twice: nothing is written" test "$(writes)" = 0
check "delivered twice: says so" grep -q 'already recorded' "$TMP/log"

# B fails too: the same issue, a comment.
RUN_ID=4343
HEAD="$B"
run "$B" timed_out
jobs diff=success test=cancelled main=failure
step 0 "B fails while the issue is open" &&
	check "B fails: still one issue" test "$(open_numbers)" = 101
expect "B fails: one comment on it" "$(ncomments 101)" 1
check "B fails: the comment names B and its conclusion" has "$(last_comment 101)" "ended **timed_out**"
check "B fails: ... and records it" has "$(last_comment 101)" "<!-- main-post-merge-watch:failed:${B}:4343/1 -->"
# `test` was cancelled and `main` failed: `--failed` takes a cancelled job as
# it takes a failed one (#1552, observed on run 37366070027), so it is enough.
check "B fails: a job failed and one was cancelled, so the command re-runs those" has "$(last_comment 101)" '`gh run rerun 4343 --failed`'
check "B fails: ... and the whole run is not what is offered" lacks "$(last_comment 101)" 're-run the whole run'

# C, a newer commit, is green. Each run tests only what its own merge reaches:
# that says nothing about A or B.
RUN_ID=4444
HEAD="$C"
run "$C" success
step 0 "C, a newer commit, is green" &&
	check "newer green run: the issue stays open" test "$(open_numbers)" = 101
check "newer green run: nothing is written" test "$(writes)" = 0
check "newer green run: the log says why, and names what is failing" grep -q "leaves .*#101 open.*${A:0:12}.*${B:0:12}" "$TMP/log"

# A's own run is re-run and passes: A is cleared, B keeps the issue open.
RUN_ID=4242
run "$A" success 2
step 0 "A's run passes on a re-run" &&
	check "A passes: the issue stays open for B" test "$(open_numbers)" = 101
expect "A passes: a comment says so" "$(ncomments 101)" 2
check "A passes: the comment names the attempt that passed" has "$(last_comment 101)" "https://github.com/o/r/actions/runs/4242/attempts/2"
check "A passes: ... records it" has "$(last_comment 101)" "<!-- main-post-merge-watch:passed:${A}:4242/2 -->"
check "A passes: ... and names what is still failing" has "$(last_comment 101)" "Still failing: \`${B:0:12}\`"
check "A passes: nothing is closed" test "$(grep -c -- '-X PATCH' "$TMP/gh.log")" = 0

# The same green event again: A is no longer failing, B still is.
step 0 "A's green run again" &&
	check "green delivered twice: nothing is written" test "$(writes)" = 0

# A's run is run a third time and fails: failing again.
run "$A" failure 3
jobs test=failure main=failure
step 0 "A's run fails again on a third attempt" &&
	check "A fails again: recorded on the same issue" has "$(last_comment 101)" "<!-- main-post-merge-watch:failed:${A}:4242/3 -->"
run "$A" success 4
step 0 "A's run passes on a fourth attempt" &&
	check "A passes again: still open for B" test "$(open_numbers)" = 101

# B's own run passes: nothing left, the issue closes.
RUN_ID=4343
run "$B" success 2
step 0 "B's run passes on a re-run" &&
	check "B passes: the issue is closed" test "$(open_numbers)" = ""
expect "B passes: closed as completed" "$(q '.[0].state_reason')" completed
check "B passes: the closing comment names the run" has "$(last_comment 101)" "https://github.com/o/r/actions/runs/4343/attempts/2"
check "B passes: ... and says nothing is failing" has "$(last_comment 101)" "No other commit recorded here is failing: the issue closes"

# The next failure opens a NEW issue: a closed one is never reopened or written to.
before="$(ncomments 101)"
RUN_ID=4444
run "$C" failure
jobs test=failure main=failure
step 0 "C fails after the issue was closed" &&
	check "after a close: a new issue" test "$(open_numbers)" = 102
expect "after a close: the closed issue is untouched" "$(ncomments 101) $(q '.[0].state')" "$before closed"
check "after a close: the new issue does not inherit A or B" lacks "$(body_of 102)" "$B"

# --- an older failure that arrives after a newer green run ---
# main is at C and C's run was green (nothing is open). A's run, slower, now
# fails. It is filed: C's run did not test what A's merge reached. And the
# issue says main has moved on, so nobody reads it as "main's head is red".
reset
RUN_ID=4242
HEAD="$C"
run "$A" failure
jobs test=failure main=failure
step 0 "A fails after C, a newer commit, went green" &&
	check "older failure: filed" test "$(open_numbers)" = 101
check "older failure: says main has moved on, and to what" has "$(body_of 101)" "\`main\` has moved on since: its head is now \`${C:0:12}\`"
check "older failure: says a later green run does not clear it" has "$(body_of 101)" "A green run of a later commit does not clear this one"
reset
step 0 "main's head cannot be read" FAKE_DOWN=head &&
	check "head unreadable: the failure is still filed" test "$(open_numbers)" = 101
check "head unreadable: the body claims nothing about main's head" lacks "$(body_of 101)" "has moved on"

# --- every other conclusion ---
for c in timed_out startup_failure skipped action_required stale; do
	reset
	run "$A" "$c"
	jobs
	step 0 "a run that ended $c" &&
		check "$c: filed, and the body says so" has "$(body_of 101)" "ended **${c}**"
	check "$c: with no job to name, the body says no job reported" has "$(body_of 101)" "no job of the run reported a failure"
	# No job failed, so `--failed` has nothing to re-run and the issue would
	# never clear: the whole run.
	check "$c: no failed job, so the command re-runs the whole run" has "$(body_of 101)" '`gh run rerun 4242`'
	check "$c: ... and --failed is not offered as the command" lacks "$(body_of 101)" 'gh run rerun 4242 --failed'
	check "$c: the gate did not run, and the body says the commit is not validated" has "$(body_of 101)" 'The `main` job, the gate of this run, did not run: the commit is not validated'
done
reset
run "$A" startup_failure
jobs
step 0 "a run that never started" &&
	check "startup_failure: the body says when a re-run cannot pass" has "$(body_of 101)" "a re-run repeats it"
reset
run "$A" skipped
step 0 "a run that was skipped" &&
	check "skipped: nothing about an invalid workflow file" lacks "$(body_of 101)" "a re-run repeats it"
# The run is red, but its gate passed: an ancillary job failed. It is filed
# all the same (the run did not pass), and the entry says the commit WAS
# validated, so nobody looks for a broken target.
reset
run "$A" failure
jobs diff=success test=success main=success refresh-pin-prs=failure
step 0 "failed, but the gate passed (refresh-pin-prs failed)" &&
	check "ancillary failure: filed" test "$(open_numbers)" = 101
body="$(body_of 101)"
check "ancillary failure: the body says the gate succeeded and the commit was validated" has "$body" 'The `main` job, the gate of this run, **succeeded**: the commit was validated'
check "ancillary failure: ... and never that it is not validated" lacks "$body" "not validated"
check "ancillary failure: the job that failed is named" has "$body" '`refresh-pin-prs`](https://github.com/o/r/actions/runs/4242/job/904) | failure |'
check "ancillary failure: the command re-runs the failed job" has "$body" '`gh run rerun 4242 --failed`'
# A failed job and a cancelled one: `--failed` re-runs both (#1552).
reset
jobs diff=success test=failure main=failure refresh-pin-prs=cancelled
step 0 "one job failed and another was cancelled" &&
	check "failed and cancelled: the command re-runs the jobs that did not pass" has "$(body_of 101)" '`gh run rerun 4242 --failed`'
check "failed and cancelled: ... and the whole run is not what is offered" lacks "$(body_of 101)" 're-run the whole run'
# Only cancelled jobs, none failed: still `--failed` (the observed case: every
# job that did not pass had been cancelled before it got a runner).
reset
jobs diff=success test=cancelled main=cancelled
step 0 "jobs were cancelled, none failed" &&
	check "only cancelled: the command re-runs the jobs that did not pass" has "$(body_of 101)" '`gh run rerun 4242 --failed`'
# A job that ended any other way (its time limit, a conclusion this script does
# not know): what `--failed` does with it was not observed, so the whole run.
for other in timed_out action_required neutral; do
	reset
	jobs diff=success test="$other" main=failure
	step 0 "a job ended ${other}" &&
		check "a job ended ${other}: the command re-runs the whole run" has "$(body_of 101)" '`gh run rerun 4242`'
	check "a job ended ${other}: ... and --failed is not offered as the command" lacks "$(body_of 101)" 'gh run rerun 4242 --failed'
done
# The gate is the ONLY job that did not pass (`diff` succeeded and wrote no
# decision, `test` skipped on it): `--failed` would re-run `main` alone, on the
# same outputs. The whole run.
reset
jobs diff=success test=skipped main=failure
step 0 "only the gate did not pass" &&
	check "only the gate failed: the command re-runs the whole run" has "$(body_of 101)" '`gh run rerun 4242`'
check "only the gate failed: ... and --failed is not offered as the command" lacks "$(body_of 101)" 'gh run rerun 4242 --failed'
check "only the gate failed: ... and the entry says why" has "$(body_of 101)" 'is the only job that did not pass'
# A gate that was only cancelled (it never got a runner) gave no verdict:
# re-running it alone is enough.
reset
jobs diff=success test=success main=cancelled
step 0 "only the gate was cancelled" &&
	check "only the gate cancelled: the command re-runs it" has "$(body_of 101)" '`gh run rerun 4242 --failed`'
reset
run "$A" ""
jobs test=failure
step 0 "a completed run with no conclusion" &&
	check "no conclusion: filed as unknown" has "$(body_of 101)" "ended **unknown**"

# Cancelled: decided by the run's own gate, the `main` job.
reset
run "$A" cancelled
jobs diff=success test=success main=success refresh-pin-prs=cancelled
step 0 "cancelled, the gate passed (refresh-pin-prs never got a runner)" &&
	check "cancelled, validated: nothing is filed" test "$(open_numbers) $(writes)" = " 0"
check "cancelled, validated: the log says why" grep -q 'ignore: .*cancelled.*main.*succeeded' "$TMP/log"
# A merge that reaches nothing: `test` is skipped and the gate passes on what
# `diff` wrote. A skipped job is not a failed one.
jobs diff=success test=skipped main=success refresh-pin-prs=cancelled
step 0 "cancelled, the gate passed, test was skipped" &&
	check "cancelled, validated with a skipped job: nothing is filed" test "$(open_numbers) $(writes)" = " 0"
jobs diff=success test=cancelled main=failure
step 0 "cancelled, the gate failed (test hit its time limit)" &&
	check "cancelled, not validated: filed" has "$(body_of 101)" "ended **cancelled**"
check "cancelled, not validated: the cancelled job is named" has "$(body_of 101)" '`test`](https://github.com/o/r/actions/runs/4242/job/902) | cancelled |'
# Cancelled with the gate green, but a job had already FAILED when the run was
# cancelled (another always() job was still finishing). That is the red run
# with a green gate, cut short: filed, like the same run left to end `failure`.
reset
jobs diff=success test=success main=success refresh-pin-prs=failure other=cancelled
step 0 "cancelled, the gate passed, but refresh-pin-prs had failed" &&
	check "cancelled over an ancillary failure: filed" test "$(open_numbers)" = 101
body="$(body_of 101)"
check "cancelled over an ancillary failure: the failed job is named" has "$body" '`refresh-pin-prs`](https://github.com/o/r/actions/runs/4242/job/904) | failure |'
check "cancelled over an ancillary failure: the body says the commit was validated" has "$body" 'The `main` job, the gate of this run, **succeeded**: the commit was validated'
check "cancelled over an ancillary failure: the failed and the cancelled job are what is re-run" has "$body" '`gh run rerun 4242 --failed`'

# --- a re-run of some of the jobs ---
# `gh run rerun --failed` after an ancillary failure re-runs `refresh-pin-prs`
# alone. If the jobs of the new attempt are then only that one, `main` is not
# among them, although it succeeded in attempt 1 and nothing it rests on ran
# again. Its last conclusion in an earlier attempt stands.
reset
run "$A" failure 2
run_at 1 failure
jobs_at 1 diff=success test=success main=success refresh-pin-prs=failure
jobs_at 2 refresh-pin-prs=failure
step 0 "attempt 2 re-ran only refresh-pin-prs, and it failed again" &&
	check "partial re-run: filed" test "$(open_numbers)" = 101
body="$(all_of 101)"
check "partial re-run: the gate's conclusion is carried from attempt 1" has "$body" 'The `main` job, the gate of this run, **succeeded** in attempt 1 and was not re-run in this attempt: the commit was validated'
check "partial re-run: never that the gate did not run" lacks "$body" "did not run"
check "partial re-run: never that the commit is not validated" lacks "$body" "not validated"
# Two partial re-runs: the gate is found two attempts back.
reset
run "$A" failure 3
run_at 1 failure
run_at 2 failure
jobs_at 1 diff=success test=success main=success refresh-pin-prs=failure
jobs_at 2 refresh-pin-prs=failure
jobs_at 3 refresh-pin-prs=failure
step 0 "attempt 3 re-ran only refresh-pin-prs again" &&
	check "two partial re-runs: carried from attempt 1" has "$(all_of 101)" '**succeeded** in attempt 1 and was not re-run in this attempt'
# The latest attempt that has the gate decides, not the first.
reset
run_at 1 failure
run_at 2 failure
jobs_at 1 diff=success test=failure main=failure refresh-pin-prs=failure
jobs_at 2 test=success main=success refresh-pin-prs=failure
jobs_at 3 refresh-pin-prs=failure
step 0 "attempt 2 fixed the gate, attempt 3 re-ran only refresh-pin-prs" &&
	check "the latest attempt with the gate decides" has "$(all_of 101)" '**succeeded** in attempt 2 and was not re-run in this attempt'
# A gate that did not succeed, carried the same way.
reset
run "$A" failure 2
run_at 1 failure
jobs_at 1 diff=success test=failure main=failure
jobs_at 2 refresh-pin-prs=failure
step 0 "the gate failed in attempt 1 and is not in attempt 2" &&
	check "partial re-run, gate failed earlier: says so, and where" has "$(all_of 101)" 'The `main` job, the gate of this run, ended **failure** in attempt 1 and was not re-run in this attempt: the commit is not validated'
# The earlier attempt cannot be read: not known, and said so. Never "did not run".
reset
run_at 1 failure
jobs_at 1 diff=success test=success main=success refresh-pin-prs=failure
jobs_at 2 refresh-pin-prs=failure
step 0 "a partial re-run, and attempt 1 cannot be read" FAKE_DOWN=jobs@1 &&
	check "partial re-run, earlier attempt unreadable: still filed" test "$(open_numbers)" = 101
body="$(all_of 101)"
check "partial re-run, earlier attempt unreadable: validation is reported as not known" has "$body" "whether the commit was validated is not known"
check "partial re-run, earlier attempt unreadable: never that the gate did not run" lacks "$body" "did not run"
check "partial re-run, earlier attempt unreadable: nor that it is not validated" lacks "$body" "is not validated"
# No attempt has the gate: it never ran.
reset
run_at 1 failure
jobs_at 1 refresh-pin-prs=failure
jobs_at 2 refresh-pin-prs=failure
step 0 "no attempt has the gate" &&
	check "no attempt has the gate: it did not run" has "$(all_of 101)" 'The `main` job, the gate of this run, did not run: the commit is not validated'
# A first attempt has no earlier one to ask.
reset
run "$A" failure 1
jobs refresh-pin-prs=failure
step 0 "attempt 1 without the gate" &&
	check "attempt 1 without the gate: one listing of jobs, no earlier attempt is asked" test "$(grep -c '/jobs?per_page' "$TMP/gh.log")" = 1
# The same premise decides a cancelled re-run: the gate succeeded in attempt 1,
# the re-run of refresh-pin-prs never got a runner. Attempt 2 is validated and
# not filed. Attempt 1 FAILED, though, and its own watcher may have found the
# re-run already running and recorded nothing: attempt 2's watcher records it.
reset
run "$A" cancelled 2
run_at 1 failure
jobs_at 1 diff=success test=success main=success refresh-pin-prs=failure
jobs_at 2 refresh-pin-prs=cancelled
step 0 "a failed attempt, then a cancelled partial re-run; only the second watcher sees a finished run" &&
	check "failure, then a cancelled partial re-run: the failure of attempt 1 is recorded, the cancelled attempt is not" test "$(failed_open)" = "4242/1"
check "failure, then a cancelled partial re-run: the entry is attempt 1's" has "$(body_of 101)" "ended **failure**: https://github.com/o/r/actions/runs/4242/attempts/1"
check "failure, then a cancelled partial re-run: ... and says why it comes now" has "$(body_of 101)" "Recorded late: this attempt ended before a newer attempt of the run began (attempt 2 is the latest)"
check "failure, then a cancelled partial re-run: the log still says attempt 2 is ignored" grep -q 'attempt 2, completed/cancelled): ignore: ' "$TMP/log"
step 0 "the same again" &&
	check "failure, then a cancelled partial re-run, delivered twice: recorded once" test "$(failed_open) $(writes)" = "4242/1 0"
step 0 "a cancelled partial re-run, and the jobs of attempt 1 cannot be read" FAKE_DOWN=jobs@1 &&
	check "cancelled partial re-run, gate not known: filed too (fail closed)" test "$(failed_open)" = "4242/1 4242/2"
check "cancelled partial re-run, gate not known: says validation is not known" has "$(all_of 101)" "whether the commit was validated is not known"

# --- every order of two attempts ---
# An attempt's watcher judges the run as it is WHEN IT RUNS, not as it was when
# the attempt ended. So a failed attempt 1 has two histories: its watcher ran
# before the re-run began (and judged attempt 1), or after (and found attempt 2,
# running or finished). What is on the issue in the end must not depend on
# which. The rule: an attempt that did not pass is recorded once, unless a
# later attempt of the same run succeeded.
#   attempt 1   G: failure, the gate failed      N: failure, the gate passed, refresh-pin-prs failed
#               C: cancelled, not validated      V: cancelled, validated (not filed by itself)
#   attempt 2   none | run: still running | P*: only the jobs that did not pass | F*: every job
#               *s success  *f failure  Fn failure of refresh-pin-prs alone
#               Pc, Fc cancelled  Fv cancelled, validated
#   expect      the attempts on the open issue as failed; `-`: no open issue
a1_conclusion() { case "$1" in G | N) echo failure ;; *) echo cancelled ;; esac }
a1_jobs() {
	case "$1" in
	G) echo "diff=success test=failure main=failure refresh-pin-prs=success" ;;
	N) echo "diff=success test=success main=success refresh-pin-prs=failure" ;;
	C) echo "diff=success test=cancelled main=failure refresh-pin-prs=success" ;;
	V) echo "diff=success test=success main=success refresh-pin-prs=cancelled" ;;
	esac
}
a2_conclusion() {
	case "$1" in
	Ps | Fs) echo success ;;
	Pf | Ff | Fn) echo failure ;;
	Pc | Fc | Fv) echo cancelled ;;
	esac
}
a2_jobs() { # attempt 1, attempt 2
	case "$2:$1" in
	Ps:G | Ps:C) echo "test=success main=success" ;;
	Ps:N | Ps:V) echo "refresh-pin-prs=success" ;;
	Pf:G | Pf:C) echo "test=failure main=failure" ;;
	Pf:N | Pf:V) echo "refresh-pin-prs=failure" ;;
	Pc:G) echo "test=cancelled main=failure" ;;
	Pc:C) echo "test=cancelled main=cancelled" ;;
	Pc:N | Pc:V) echo "refresh-pin-prs=cancelled" ;;
	Fs:*) echo "diff=success test=success main=success refresh-pin-prs=success" ;;
	Ff:*) echo "diff=success test=failure main=failure refresh-pin-prs=success" ;;
	Fn:*) echo "diff=success test=success main=success refresh-pin-prs=failure" ;;
	Fc:*) echo "diff=success test=cancelled main=failure refresh-pin-prs=success" ;;
	Fv:*) echo "diff=success test=success main=success refresh-pin-prs=cancelled" ;;
	esac
}
# The run as the API shows it once attempt 2 exists.
second_attempt() { # attempt 1, attempt 2
	if [ "$2" = run ]; then
		run "$A" "" 2 in_progress
	else
		run "$A" "$(a2_conclusion "$2")" 2
		# shellcheck disable=SC2046 # the words are the jobs
		jobs_at 2 $(a2_jobs "$1" "$2")
	fi
}
RUN_ID=4242
HEAD="$A"
rows=0
while read -r a1 a2 expect; do
	[ -n "$a1" ] || continue
	rows=$((rows + 1))
	want_open="${expect//,/ }"
	[ "$want_open" != - ] || want_open=""
	want_open="$(for k in $want_open; do printf '4242/%s ' "$k"; done)"
	want_open="${want_open% }"
	for order in "its watcher ran before the re-run" "its watcher ran after the re-run began"; do
		[ "$a2" != none ] || [ "$order" = "its watcher ran before the re-run" ] || continue
		reset
		run_at 1 "$(a1_conclusion "$a1")"
		# shellcheck disable=SC2046 # the words are the jobs
		jobs_at 1 $(a1_jobs "$a1")
		ok=true
		if [ "$order" = "its watcher ran before the re-run" ]; then
			run "$A" "$(a1_conclusion "$a1")" 1
			step 0 "$a1/$a2: the watcher of attempt 1, on time" || ok=false
			if [ "$a2" != none ]; then
				second_attempt "$a1" "$a2"
				step 0 "$a1/$a2: the watcher of attempt 2" || ok=false
			fi
		else
			second_attempt "$a1" "$a2"
			step 0 "$a1/$a2: the watcher of attempt 1, late" || ok=false
			step 0 "$a1/$a2: the watcher of attempt 2" || ok=false
		fi
		[ "$ok" = true ] || continue
		expect "attempt 1 $a1, attempt 2 $a2, $order: failed on the open issue; recorded twice" \
			"[$(failed_open)] [$(failed_twice)] $(open_numbers | wc -w)" "[$want_open] [] $([ -n "$want_open" ] && echo 1 || echo 0)"
	done
done <<'ROWS'
G none 1
G run  1
G Ps   -
G Fs   -
G Pf   1,2
G Ff   1,2
G Fn   1,2
G Pc   1,2
G Fc   1,2
G Fv   1
N none 1
N run  1
N Ps   -
N Fs   -
N Pf   1,2
N Ff   1,2
N Fn   1,2
N Pc   1
N Fc   1,2
N Fv   1
C none 1
C run  1
C Ps   -
C Fs   -
C Pf   1,2
C Ff   1,2
C Fn   1,2
C Pc   1,2
C Fc   1,2
C Fv   1
V none -
V run  -
V Ps   -
V Fs   -
V Pf   2
V Ff   2
V Fn   2
V Pc   -
V Fc   2
V Fv   -
ROWS
expect "every row of the table ran" "$rows" 40

# Three attempts, and only the last one's watcher finds a finished run.
# A failure, a green re-run, a failure: the green attempt answered the first.
reset
run "$A" failure 3
run_at 1 failure
run_at 2 success
jobs_at 1 diff=success test=failure main=failure
jobs_at 3 diff=success test=failure main=failure
step 0 "failure, success, failure; one watcher" &&
	check "an attempt answered by a later green one is not dug up" test "$(failed_open)" = "4242/3"
# Two cancelled re-runs of refresh-pin-prs after its failure: both validated,
# both ignored, and the failure two attempts back is still found, once.
reset
run "$A" cancelled 3
run_at 1 failure
run_at 2 cancelled
jobs_at 1 diff=success test=success main=success refresh-pin-prs=failure
jobs_at 2 refresh-pin-prs=cancelled
jobs_at 3 refresh-pin-prs=cancelled
step 0 "failure, cancelled, cancelled; one watcher" &&
	check "a failure two attempts back, behind two ignored ones, is recorded" test "$(failed_open)" = "4242/1"
step 0 "the same again" &&
	check "... once" test "$(failed_open) $(writes)" = "4242/1 0"
# Two unrecorded failures before a cancelled, validated attempt: both, oldest first.
no_issues
run_at 2 failure
jobs_at 2 refresh-pin-prs=failure
step 0 "failure, failure, cancelled; one watcher" &&
	check "two unrecorded failures: both recorded" test "$(failed_open)" = "4242/1 4242/2"
check "two unrecorded failures: the oldest opens the issue" has "$(body_of 101)" "<!-- main-post-merge-watch:failed:${A}:4242/1 -->"
check "two unrecorded failures: the next is a comment on it" has "$(last_comment 101)" "<!-- main-post-merge-watch:failed:${A}:4242/2 -->"
# An earlier attempt cannot be read: what is known is recorded, the step is
# red, and a re-run of it adds the rest without repeating anything.
reset
run "$A" failure 2
run_at 1 failure
jobs_at 1 diff=success test=failure main=failure
jobs_at 2 diff=success test=failure main=failure
step 1 "failure, failure; attempt 1 cannot be read" FAKE_DOWN=run@1 &&
	check "earlier attempt unreadable: this attempt is recorded all the same" test "$(failed_open)" = "4242/2"
check "earlier attempt unreadable: the step fails, and says what may be missing" \
	grep -q '^::error title=main-post-merge-watch::an earlier attempt of run 4242 could not be read' "$TMP/log"
step 0 "the watcher is re-run" &&
	check "earlier attempt unreadable, then read: attempt 1 is added, nothing twice" test "[$(failed_open)] [$(failed_twice)]" = "[4242/1 4242/2] []"
# A dry run looks at the earlier attempts too, and writes nothing.
no_issues
step 0 "dry run of a run with an unrecorded earlier failure" DRY_RUN=true &&
	check "dry run with an earlier failure: nothing is written" test "$(writes) $(open_numbers)" = "0 "
check "dry run with an earlier failure: prints both" test "$(grep -c '^<!-- main-post-merge-watch:failed:' "$TMP/log")" = 2
# ... as the writes the real step makes: ONE issue for the older attempt, and
# the newer one as a comment on it. Not two issues.
check "dry run with an earlier failure: one issue would be opened" test "$(grep -c '^DRY RUN: would open ' "$TMP/log")" = 1
check "dry run with an earlier failure: the other attempt would be a comment on that issue" \
	test "$(grep -c '^DRY RUN: would comment on the issue it would open:$' "$TMP/log")" = 1
check "dry run with an earlier failure: the issue is opened first" \
	test "$(grep -n '^DRY RUN: would ' "$TMP/log" | head -n 1 | cut -d: -f2-)" = "$(grep '^DRY RUN: would open ' "$TMP/log")"
no_issues
step 0 "the same, for real" &&
	check "the real step makes those writes: one issue, one comment" test "$(open_numbers) $(ncomments 101) $(grep -c -- '-X POST repos/o/r/issues ' "$TMP/gh.log")" = "101 1 1"
jobs diff=success test=cancelled main=failure

reset
run "$A" cancelled
jobs diff=success test=cancelled main=failure
step 0 "cancelled, and the jobs cannot be read" FAKE_DOWN=jobs &&
	check "cancelled, jobs unreadable: filed (fail closed)" test "$(open_numbers)" = 101
check "cancelled, jobs unreadable: the body says the jobs could not be listed" has "$(body_of 101)" "the jobs of the run could not be listed"
check "cancelled, jobs unreadable: the command re-runs the whole run" has "$(body_of 101)" '`gh run rerun 4242`'
check "cancelled, jobs unreadable: ... not only the failed jobs" lacks "$(body_of 101)" 'gh run rerun 4242 --failed'
check "cancelled, jobs unreadable: the body claims nothing about the gate" lacks "$(body_of 101)" "the gate of this run"
reset
run "$A" failure
step 0 "failed, and the jobs cannot be read" FAKE_DOWN=jobs &&
	check "failed, jobs unreadable: still filed" test "$(open_numbers)" = 101

# --- runs that are not a post-merge run of main, or not finished ---
reset
issue 101 "$TITLE" "$BOT" "<!-- main-post-merge-watch:failed:${A}:4242/1 -->"
for spec in "failure:completed:pull_request:main:mallory/r:$PATH_MAIN" "success:completed:pull_request:main:mallory/r:$PATH_MAIN" \
	"success:completed:push:feature:o/r:$PATH_MAIN" "success:completed:push:main:o/r:.github/workflows/evil.yaml" \
	"success:completed:workflow_dispatch:main:o/r:$PATH_MAIN"; do
	IFS=: read -r c st e b hr p <<<"$spec"
	run "$A" "$c" 2 "$st" "$e" "$b" "$hr" "$p"
	step 0 "not ours: $spec" &&
		check "not ours ($e $b $hr $p $st ${c:-none}): one read of the run, nothing else" test "$(wc -l <"$TMP/gh.log")" = 1
done
expect "not ours: the open issue is as it was" "$(open_numbers) $(ncomments 101)" "101 0"
# A first attempt that is still running: nothing to judge, nothing before it.
run "$A" "" 1 in_progress
step 0 "a first attempt that is not finished" &&
	check "not finished, attempt 1: one read of the run, nothing else" test "$(wc -l <"$TMP/gh.log")" = 1
# A re-run that is still running: its own completion is judged later; the
# attempt before it succeeded here, so there is nothing to record.
run "$A" "" 2 in_progress
step 0 "a re-run that is not finished, after a green attempt" &&
	check "not finished, attempt 2 after a green one: nothing is written" test "$(writes) $(open_numbers) $(ncomments 101)" = "0 101 0"

# --- decoys: only what this workflow wrote counts ---
# The same title, opened by someone else; a pull request with the title; an
# issue that quotes the title. None of them is the rolling issue.
reset
issue 98 "$TITLE" mallory "<!-- main-post-merge-watch:failed:${A}:4242/1 -->"
issue 99 "$TITLE" "$BOT" "" yes
issue 100 "Re: ${TITLE} (discussion)" "$BOT"
run "$A" failure
jobs test=failure main=failure
step 0 "A fails with three decoys open" &&
	check "decoys: a new issue is opened, none of the decoys is written to" \
		test "$(open_numbers) $(ncomments 98) $(ncomments 99) $(ncomments 100)" = "98 99 100 101 0 0 0"
# Someone comments a `passed` marker for B on the real issue. B stays failing.
RUN_ID=4343
run "$B" failure
step 0 "B fails" && comment 101 mallory "<!-- main-post-merge-watch:passed:${B}:4343/2 -->"
RUN_ID=4242
run "$A" success 2
step 0 "A passes; a stranger's comment claims B passed" &&
	check "a stranger's marker: B still keeps the issue open" test "$(open_numbers)" = "98 99 100 101"
check "a stranger's marker: B is still named as failing" has "$(last_comment 101)" "Still failing: \`${B:0:12}\`"
# ... and a stranger's issue with the title is never closed by a green run.
RUN_ID=4343
run "$B" success 2
step 0 "B passes" &&
	check "decoys: the rolling issue closes, the decoys stay" test "$(open_numbers)" = "98 99 100"

# --- duplicates ---
# Two runs fail at the same moment: both list nothing, both create. The one
# that finds an older issue after its create moves its record there and closes
# its own.
reset
RUN_ID=4242
run "$A" failure
jobs test=failure main=failure
step 0 "another run files the issue between the listing and the create" FAKE_RACE=1 &&
	check "race: one issue stays open, the older one" test "$(open_numbers)" = 101
check "race: this run's failure is recorded on it" has "$(last_comment 101)" "<!-- main-post-merge-watch:failed:${A}:4242/1 -->"
expect "race: the newer one is closed as not planned" "$(q '.[1].state + " " + .[1].state_reason')" "closed not_planned"
check "race: ... with a comment pointing at the older one" has "$(last_comment 102)" "Duplicate of #101"
# Two open already (the fold failed half way): read as one, closed together.
reset
issue 101 "$TITLE" "$BOT" "<!-- main-post-merge-watch:failed:${A}:4242/1 -->"
issue 105 "$TITLE" "$BOT" "<!-- main-post-merge-watch:failed:${B}:4343/1 -->"
run "$A" success 2
step 0 "two open issues, A passes" &&
	check "two open: B, recorded on the second, keeps both open" test "$(open_numbers)" = "101 105"
check "two open: the comment goes to the older one" test "$(ncomments 101) $(ncomments 105)" = "1 0"
RUN_ID=4343
run "$B" failure 3
jobs test=failure
step 0 "two open issues, B fails again" &&
	check "two open: recorded on the older one, no third issue" test "$(open_numbers) $(ncomments 101)" = "101 105 2"
run "$B" success 4
step 0 "two open issues, B passes" &&
	check "two open: both close" test "$(open_numbers)" = ""

# The same, the other way round: B's failure is in the body of the NEWER copy
# and its pass is written on the older one, so the pass comes first in the
# text. The attempt decides, not the position: B stays cleared when A passes.
reset
issue 101 "$TITLE" "$BOT" "<!-- main-post-merge-watch:failed:${A}:4242/1 -->"
issue 105 "$TITLE" "$BOT" "<!-- main-post-merge-watch:failed:${B}:4343/1 -->"
RUN_ID=4343
run "$B" success 2
step 0 "two open issues, B (recorded on the newer) passes first" &&
	check "two open, reversed: A keeps both open" test "$(open_numbers) $(ncomments 101)" = "101 105 1"
RUN_ID=4242
run "$A" success 2
step 0 "two open issues, then A passes" &&
	check "two open, reversed: B's older failure does not outlive its pass; both close" test "$(open_numbers)" = ""

# --- two watchers at once ---
# Watchers of different runs are not serialised (one group for the workflow
# would drop a pending one). Each case is one step of this watcher with the
# other's write placed at the worst moment by the fake.
others_comment() { # issue, body -> a jq program: the bot comments on it
	printf 'map(if .number == %s then .comments += [{user: {login: "github-actions[bot]"}, body: "%s"}] else . end)' "$1" "$2"
}
# A and B are failing and both pass at once. Each read the issue before the
# other wrote, so each saw the other still failing. The one that writes last
# reads again and closes: the issue is not left open with nothing failing.
reset
issue 101 "$TITLE" "$BOT" "<!-- main-post-merge-watch:failed:${A}:4242/1 --> <!-- main-post-merge-watch:failed:${B}:4343/1 -->"
RUN_ID=4242
run "$A" success 2
step 0 "A and B pass at once; B's pass lands just before A's comment" \
	FAKE_HOOK=before-comment FAKE_HOOK_JQ="$(others_comment 101 "<!-- main-post-merge-watch:passed:${B}:4343/2 -->")" &&
	check "two passes at once: the later writer closes the issue" test "$(open_numbers)" = ""
# A is the only failure and passes; B's failure lands after A's watcher read
# the issue, before its comment. Read again after writing: not closed.
reset
issue 101 "$TITLE" "$BOT" "<!-- main-post-merge-watch:failed:${A}:4242/1 -->"
step 0 "A passes; B's failure lands just before A's comment" \
	FAKE_HOOK=before-comment FAKE_HOOK_JQ="$(others_comment 101 "<!-- main-post-merge-watch:failed:${B}:4343/1 -->")" &&
	check "a failure before the pass is written: the issue is never closed" test "$(open_numbers) $(grep -c -- '-X PATCH' "$TMP/gh.log")" = "101 0"
# ... and when it lands in the last gap, between that second reading and the
# close: the closer looks once more at what it closed, and reopens it.
reset
issue 101 "$TITLE" "$BOT" "<!-- main-post-merge-watch:failed:${A}:4242/1 -->"
step 0 "A passes; B's failure lands just before the close" \
	FAKE_HOOK=before-close FAKE_HOOK_JQ="$(others_comment 101 "<!-- main-post-merge-watch:failed:${B}:4343/1 -->")" &&
	check "a failure just before the close: the issue is open again" test "$(open_numbers) $(q '.[0].state_reason')" = "101 null"
check "a failure just before the close: the log says why it was reopened" grep -q "reopened #101: .*${B:0:12}" "$TMP/log"
# The same race seen from the watcher that files: its comment went onto an
# issue that another watcher closes in that moment. It finds it closed, and
# reopens it: a failure is never left on a closed issue.
reset
issue 101 "$TITLE" "$BOT" "<!-- main-post-merge-watch:failed:${A}:4242/1 --> <!-- main-post-merge-watch:passed:${A}:4242/2 -->"
RUN_ID=4343
run "$B" failure
jobs test=failure main=failure
step 0 "B fails; the issue is closed just after B's comment" \
	FAKE_HOOK=after-comment FAKE_HOOK_JQ='map(if .number == 101 then .state = "closed" | .state_reason = "completed" else . end)' &&
	check "closed under a failure: the issue is open again, with the failure on it" test "$(open_numbers) $(ncomments 101)" = "101 1"
check "closed under a failure: the log says so" grep -q 'reopened #101' "$TMP/log"
# Nothing raced: a plain close stays closed, and costs no reopen.
reset
issue 101 "$TITLE" "$BOT" "<!-- main-post-merge-watch:failed:${A}:4242/1 -->"
RUN_ID=4242
run "$A" success 2
step 0 "A passes, alone" &&
	check "no race: closed, and not reopened" test "$(open_numbers) $(grep -c -- '-X PATCH' "$TMP/gh.log")" = " 1"
# The reopen fails: the watcher's run is red, not silent.
reset
issue 101 "$TITLE" "$BOT" "<!-- main-post-merge-watch:failed:${A}:4242/1 -->"
step 1 "B's failure lands before the close, and the reopen fails" FAKE_DOWN=reopen \
	FAKE_HOOK=before-close FAKE_HOOK_JQ="$(others_comment 101 "<!-- main-post-merge-watch:failed:${B}:4343/1 -->")"
# The check itself cannot be made: red, and it says what was left unchecked.
reset
issue 101 "$TITLE" "$BOT" "<!-- main-post-merge-watch:failed:${A}:4242/1 -->"
RUN_ID=4343
run "$B" failure
step 1 "B fails; the issue cannot be read back after the comment" FAKE_DOWN=read-after-comment &&
	check "no read back after a failure: an error that says to check the issue is open" \
		grep -q '^::error title=main-post-merge-watch::the failure is recorded on #101, but the issue could not be read back' "$TMP/log"
reset
issue 101 "$TITLE" "$BOT" "<!-- main-post-merge-watch:failed:${A}:4242/1 -->"
RUN_ID=4242
run "$A" success 2
step 1 "A passes; the issue cannot be read back after the close" FAKE_DOWN=read-after-close &&
	check "no read back after a close: an error that says what to check" \
		grep -q '^::error title=main-post-merge-watch::closed #101, but could not read it back' "$TMP/log"

# --- a label that does not exist ---
reset
RUN_ID=4242
run "$A" failure
step 0 "the labels do not exist" FAKE_NO_LABELS=1 &&
	check "no labels: the issue is still filed, without them" test "$(open_numbers) $(q '.[0].labels | length')" = "101 0"
check "no labels: with a warning" grep -q '^::warning title=main-post-merge-watch::.*label' "$TMP/log"

# --- the API fails: the watcher's own run goes red, and writes nothing blind ---
reset
step 1 "the run cannot be read" FAKE_DOWN=run &&
	check "run unreadable: an error, nothing written" test "$(grep -c '^::error title=main-post-merge-watch::' "$TMP/log") $(writes)" = "1 0"
step 1 "the issues cannot be listed" FAKE_DOWN=list &&
	check "listing down: nothing is filed blind" test "$(writes) $(open_numbers)" = "0 "
step 1 "the issue cannot be created" FAKE_DOWN=create &&
	check "create down: an error" grep -q '^::error title=main-post-merge-watch::' "$TMP/log"
issue 101 "$TITLE" "$BOT" "<!-- main-post-merge-watch:failed:${B}:4343/1 -->"
step 1 "the open issue cannot be read" FAKE_DOWN=read &&
	check "issue unreadable: nothing is written" test "$(writes)" = 0
step 1 "the comment cannot be posted" FAKE_DOWN=comment
expect "comment down: nothing recorded" "$(ncomments 101)" 0
# The close fails after the comment went through. The next delivery of the
# same green run finds nothing failing and closes it.
RUN_ID=4343
run "$B" success 2
step 1 "the issue cannot be closed" FAKE_DOWN=close &&
	check "close down: the commit is recorded as passing, the issue is still open" test "$(ncomments 101) $(open_numbers)" = "1 101"
step 0 "the same green run again" &&
	check "close down, then again: closed, no second comment" test "$(ncomments 101) $(open_numbers)" = "1 "

# --- dry run ---
reset
RUN_ID=4242
run "$A" failure
jobs test=failure main=failure
step 0 "dry run of a failure" DRY_RUN=true &&
	check "dry run: nothing is written" test "$(writes) $(open_numbers)" = "0 "
check "dry run: prints the issue it would open" grep -qF "<!-- main-post-merge-watch:failed:${A}:4242/1 -->" "$TMP/log"
issue 101 "$TITLE" "$BOT" "<!-- main-post-merge-watch:failed:${A}:4242/1 -->"
run "$A" success 2
step 0 "dry run of the green run that would close it" DRY_RUN=true &&
	check "dry run: nothing is closed" test "$(writes) $(open_numbers)" = "0 101"
check "dry run: says what it would do" grep -q '^DRY RUN: would close #101' "$TMP/log"
check "dry run of a green run: the pass would be a comment on the issue, before the close" \
	test "$(grep '^DRY RUN: would ' "$TMP/log" | tr '\n' '|')" = "DRY RUN: would comment on #101:|DRY RUN: would close #101 (completed)|"
check "dry run of a green run: prints the record it would write" grep -qFx "<!-- main-post-merge-watch:passed:${A}:4242/2 -->" "$TMP/log"
# Every other write the step has, dry, next to what the real step does.
# A failure with the issue open: a comment on it, no second issue.
RUN_ID=4343
run "$B" failure
jobs test=failure main=failure
step 0 "dry run of a failure with the issue open" DRY_RUN=true &&
	check "dry run, issue open: one comment on it, nothing opened, nothing written" \
		test "$(grep '^DRY RUN: would ' "$TMP/log" | tr '\n' '|') $(writes)" = "DRY RUN: would comment on #101:| 0"
step 0 "the same, for real" &&
	check "the real step: one comment on the open issue" test "$(open_numbers) $(ncomments 101)" = "101 1"
# A green run that clears one commit of two: a comment, and no close; the log
# says the issue stays open, as the real step's does.
RUN_ID=4242
run "$A" success 2
step 0 "dry run of a green run that leaves another commit failing" DRY_RUN=true &&
	check "dry run, one of two cleared: one comment, no close" \
		test "$(grep '^DRY RUN: would ' "$TMP/log" | tr '\n' '|') $(writes)" = "DRY RUN: would comment on #101:| 0"
check "dry run, one of two cleared: says the issue stays open, and for what" grep -q "^#101 stays open. Still failing: \`${B:0:12}\`$" "$TMP/log"
step 0 "the same, for real" &&
	check "the real step: the pass is a comment, the issue stays open" test "$(open_numbers) $(ncomments 101)" = "101 2"
check "the real step says the same about the issue" grep -q "^#101 stays open. Still failing: \`${B:0:12}\`$" "$TMP/log"
# The same green run again: the commit is not failing any more, nothing to say.
step 0 "dry run of a green run already recorded" DRY_RUN=true &&
	check "dry run, already cleared: no write is announced" test "$(grep -c '^DRY RUN: would ' "$TMP/log") $(writes)" = "0 0"
# A failure already recorded: nothing.
RUN_ID=4343
run "$B" failure
step 0 "dry run of a failure already recorded" DRY_RUN=true &&
	check "dry run, already recorded: no write is announced" test "$(grep -c '^DRY RUN: would ' "$TMP/log") $(writes)" = "0 0"
RUN_ID=4242

# --- 3. what reaches the issue ------------------------------------------------------------------
# The run id is the one value taken from the event. Anything but digits is refused
# before gh is called.
for bad in "" "4242; echo pwned" '$(id)' "42 43" "-1" "0x10"; do
	step 2 "RUN_ID='${bad}'" RUN_ID="$bad" &&
		check "RUN_ID='${bad}': refused, gh is never called" test ! -s "$TMP/gh.log"
done
# The rest is read from the API, and still checked before it is written down.
reset
run "not-a-sha" failure
step 1 "a run whose commit is not a sha" &&
	check "bad sha: nothing is written" test "$(writes)" = 0
run "$A" failure
"$JQ" '.run_attempt = "1/../../../issues"' "$S/run.json" >"$S/r.next" && mv "$S/r.next" "$S/run.json"
step 1 "a run whose attempt is not a number" &&
	check "bad attempt: no request is built from it, nothing is written" test "$(wc -l <"$TMP/gh.log") $(writes)" = "1 0"
# A job name is free text. It must not close the code span, mention anyone, or
# forge a record; a job URL that is not this run's is dropped.
run "$A" 'failure**
<!-- x -->'
"$JQ" -n --arg a "$A" '{jobs: [
	{name: ("te`st @everyone <!-- main-post-merge-watch:passed:" + $a + ":4242/9 --> | x |"), conclusion: "failure", html_url: "https://evil.example/actions/runs/4242/job/1"},
	{name: "ok", conclusion: "fail`ure | @x |", html_url: "https://github.com/o/r/actions/runs/4242/job/7"}]}' >"$S/jobs.json"
step 0 "a run with hostile job names and a hostile conclusion" &&
	check "hostile: filed" test "$(open_numbers)" = 101
body="$(body_of 101)"
check "hostile: no mention" lacks "$body" "@"
check "hostile: no forged record" lacks "$body" ":passed:"
check "hostile: no foreign link" lacks "$body" "evil.example"
check "hostile: the conclusion is reduced to its letters" has "$body" "ended **failurex**"
check "hostile: the job is still named, harmlessly, without a link" has "$body" '| `test everyone -- main-post-merge-watchpassed'"$A"'4242/9 -- x` | failure |'
check "hostile: a job conclusion is reduced to its letters" has "$body" '| [`ok`](https://github.com/o/r/actions/runs/4242/job/7) | failurex |'
expect "hostile: the body records exactly one thing" "$(grep -c 'main-post-merge-watch:[a-z]*:[0-9a-f]\{40\}:[0-9]*/[0-9]* -->' <<<"$body")" 1

# --- 4. the workflow ----------------------------------------------------------------------------
code() { grep -vE '^[[:space:]]*#' "$1"; } # the file without its comment lines
wf="$(code "$WORKFLOW")"
expect "main.yaml is still named main-post-merge" "$(sed -n 's/^name: //p' "$MAIN_WORKFLOW")" "main-post-merge"
expect "the watcher is triggered by that name, on completion, and can be run by hand" \
	"$(sed -n '/^on:$/,/^permissions:/p' <<<"$wf" | grep -E '^  [a-z_]+:|^    (workflows|types):' | tr -s ' ' | tr '\n' '|')" \
	" workflow_run:| workflows: [main-post-merge]| types: [completed]| workflow_dispatch:|"
expect "the workflow itself has no permission" "$(grep -cx 'permissions: {}' <<<"$wf")" 1
expect "its one job has exactly the three it needs" \
	"$(sed -n '/^    permissions:$/,/^    [a-z]/p' <<<"$wf" | grep -E '^      [a-z-]+: ' | sed 's/^ *//' | sort | tr '\n' '|')" \
	"actions: read|contents: read|issues: write|"
expect "one job" "$(grep -cE '^  [A-Za-z0-9_-]+:$' <<<"$(sed -n '/^jobs:$/,$p' <<<"$wf")")" 1
expect "the job has a time limit" "$(grep -cE '^    timeout-minutes: [0-9]+$' <<<"$wf")" 1
expect "the job has no environment" "$(grep -c 'environment:' <<<"$wf")" 0
expect "the only script is the one under scripts/" "$(grep -E '^[[:space:]]+run:' <<<"$wf" | sed 's/^ *//')" "run: ./scripts/main-post-merge-watch.sh"
# Every expression in the file, by the key it is the value of. One field of
# the triggering run is read, its id; nothing of the run is checked out.
expect "the expressions, each handed over through env (or the concurrency group)" \
	"$(grep -F '${{' <<<"$wf" | sed 's/^ *//' | LC_ALL=C sort | tr '\n' '|')" \
	'DRY_RUN: ${{ github.event_name == '"'workflow_dispatch'"' && inputs.dry_run }}|GH_REPO: ${{ github.repository }}|GH_TOKEN: ${{ github.token }}|RUN_ID: ${{ github.event.workflow_run.id || inputs.run_id }}|WATCH_RUN_URL: ${{ github.server_url }}/${{ github.repository }}/actions/runs/${{ github.run_id }}|group: main-post-merge-watch-${{ github.event.workflow_run.id || inputs.run_id }}${{ github.event_name == '"'workflow_dispatch'"' && inputs.dry_run && format('"'-dry-run-{0}'"', github.run_id) || '"''"' }}|'
# GitHub keeps ONE pending run per group and cancels the one that was waiting.
# A manual dry run in the judged run's group could so replace a pending
# automatic watcher of that run, and a dry run writes nothing: the verdict
# would be lost. A dry run gets a group of its own (this watcher run's id).
# Whatever can write stays in the judged run's group, where the run that
# survives judges the run as it is then, and a second judgment writes nothing.
group="$(sed -n 's/^  group: //p' <<<"$wf")"
dry_cond="$(sed -n 's/^          DRY_RUN: \${{ \(.*\) }}$/\1/p' <<<"$wf")"
expect "the group starts with the judged run, for every event" "${group%%\}\}*}}}" \
	'main-post-merge-watch-${{ github.event.workflow_run.id || inputs.run_id }}'
expect "a dry run adds this watcher run's own id to the group; nothing else adds anything" "\${{${group#*\}\}\$\{\{}" \
	'${{ '"$dry_cond"' && format('"'-dry-run-{0}'"', github.run_id) || '"''"' }}'
expect "the group's dry-run condition is the one DRY_RUN is set from" "$dry_cond" "github.event_name == 'workflow_dispatch' && inputs.dry_run"
expect "runs are not cancelled or replaced within the group" "$(grep -c '^  cancel-in-progress: false$' <<<"$wf")" 1
expect "every action is pinned by a full commit sha, with its version" \
	"$(grep -E '^[[:space:]]+(- )?uses:' <<<"$wf" | grep -cvE 'uses: [A-Za-z0-9_./-]+@[0-9a-f]{40} # v?[0-9][0-9.]*$')" 0
expect "the one action is the checkout, at the sha main.yaml uses" \
	"$(grep -oE 'uses: [^ ]+' <<<"$wf" | sort -u)" "$(grep -oE 'uses: actions/checkout@[0-9a-f]{40}' "$MAIN_WORKFLOW" | sort -u)"
expect "the checkout is main's script whatever started the run: never the run's commit, nor the ref of a manual run" \
	"$(grep -E '^[[:space:]]+(ref|repository):' <<<"$wf" | sed 's/^ *//' | tr '\n' '|')" "ref: main|"
expect "the checkout leaves no token in the workspace" "$(grep -c '^          persist-credentials: false$' <<<"$wf")" 1
expect "the checkout fetches the script and nothing else" \
	"$(sed -n '/sparse-checkout: |/,/sparse-checkout-cone-mode/p' <<<"$wf" | sed 's/^ *//' | tr '\n' '|')" \
	"sparse-checkout: ||scripts/main-post-merge-watch.sh|sparse-checkout-cone-mode: false|"
# What the decision rests on, in main.yaml.
expect "main.yaml has no concurrency group (nothing supersedes a run: a cancelled run is not a superseded one)" \
	"$(code "$MAIN_WORKFLOW" | grep -c 'concurrency:')" 0
expect "main.yaml runs on a push to main and nothing else" \
	"$(code "$MAIN_WORKFLOW" | sed -n '/^on:$/,/^permissions:/p' | grep -vE '^(on|permissions):$' | tr -s ' \n' ' ')" " push: branches: - main "
expect "main.yaml's gate is the job called main, which the script reads by that name" \
	"$(code "$MAIN_WORKFLOW" | sed -n '/^  main:$/,/^  [a-z-]*:$/p' | grep -cE '^  main:$|^    name:')" 1

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS check(s) failed"
	exit 1
fi
echo "all checks passed"
