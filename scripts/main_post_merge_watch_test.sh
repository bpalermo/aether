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
# decide <event> <branch> <head repository> <repository> <workflow path> <status> <conclusion> <main job>
want() { # file|clear|ignore, name, the eight arguments
	local want="$1" name="$2" got
	shift 2
	got="$(bash "$SCRIPT" decide "$@")"
	case "$want:$got" in
	file:file:\ * | clear:clear | ignore:ignore:\ *) pass "decide: $name -> $got" ;;
	*) fail "decide: $name: got '$got', want $want" ;;
	esac
}
ours=(push main o/r o/r "$PATH_MAIN" completed)
want clear "a green run" "${ours[@]}" success ""
want file "a failed run" "${ours[@]}" failure failure
want file "a failed run whose gate passed (another job failed)" "${ours[@]}" failure success
want file "a run that timed out" "${ours[@]}" timed_out ""
want file "a run that never started (an invalid workflow file)" "${ours[@]}" startup_failure ""
want file "a run that was skipped: nothing validated the commit" "${ours[@]}" skipped ""
want file "a run waiting for an approval" "${ours[@]}" action_required ""
want file "a conclusion this script has never heard of: fail closed" "${ours[@]}" stale ""
want file "a completed run with no conclusion: fail closed" "${ours[@]}" "" ""
# main.yaml has no concurrency group: nothing supersedes a run. A cancelled
# run is a job that hit its time limit, or a person.
want file "cancelled, and its gate failed" "${ours[@]}" cancelled failure
want file "cancelled, and its gate was cancelled too" "${ours[@]}" cancelled cancelled
want file "cancelled, and its gate never ran" "${ours[@]}" cancelled ""
want file "cancelled, and the jobs could not be read: fail closed" "${ours[@]}" cancelled unknown
want ignore "cancelled, but its gate passed: the commit was validated" "${ours[@]}" cancelled success
want ignore "a run that is not finished (a newer attempt is running)" push main o/r o/r "$PATH_MAIN" in_progress "" ""
want ignore "a queued run" push main o/r o/r "$PATH_MAIN" queued "" ""
# Not a post-merge run of main: whatever it concluded.
for c in success failure; do
	want ignore "a pull request's run from a fork whose branch is called main ($c)" pull_request main mallory/r o/r "$PATH_MAIN" completed "$c" ""
	want ignore "a pull request's run in this repository ($c)" pull_request main o/r o/r "$PATH_MAIN" completed "$c" ""
	want ignore "a push to another branch ($c)" push feature o/r o/r "$PATH_MAIN" completed "$c" ""
	want ignore "a push in another repository ($c)" push main mallory/r o/r "$PATH_MAIN" completed "$c" ""
	want ignore "another workflow that took the name ($c)" push main o/r o/r .github/workflows/other.yaml completed "$c" ""
	want ignore "a manual run ($c)" workflow_dispatch main o/r o/r "$PATH_MAIN" completed "$c" ""
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
	down jobs
	out <"$FAKE_STATE/jobs.json"
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
reset() { echo '[]' >"$S/issues.json"; }
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
done
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
jobs diff=success test=cancelled main=failure
step 0 "cancelled, the gate failed (test hit its time limit)" &&
	check "cancelled, not validated: filed" has "$(body_of 101)" "ended **cancelled**"
check "cancelled, not validated: the cancelled job is named" has "$(body_of 101)" '`test`](https://github.com/o/r/actions/runs/4242/job/902) | cancelled |'
reset
step 0 "cancelled, and the jobs cannot be read" FAKE_DOWN=jobs &&
	check "cancelled, jobs unreadable: filed (fail closed)" test "$(open_numbers)" = 101
check "cancelled, jobs unreadable: the body says the jobs could not be listed" has "$(body_of 101)" "the jobs of the run could not be listed"
reset
run "$A" failure
step 0 "failed, and the jobs cannot be read" FAKE_DOWN=jobs &&
	check "failed, jobs unreadable: still filed" test "$(open_numbers)" = 101

# --- runs that are not a post-merge run of main, or not finished ---
reset
issue 101 "$TITLE" "$BOT" "<!-- main-post-merge-watch:failed:${A}:4242/1 -->"
for spec in "failure:completed:pull_request:main:mallory/r:$PATH_MAIN" "success:completed:pull_request:main:mallory/r:$PATH_MAIN" \
	"success:completed:push:feature:o/r:$PATH_MAIN" "success:completed:push:main:o/r:.github/workflows/evil.yaml" \
	"success:completed:workflow_dispatch:main:o/r:$PATH_MAIN" ":in_progress:push:main:o/r:$PATH_MAIN"; do
	IFS=: read -r c st e b hr p <<<"$spec"
	run "$A" "$c" 2 "$st" "$e" "$b" "$hr" "$p"
	step 0 "not ours: $spec" &&
		check "not ours ($e $b $hr $p $st ${c:-none}): one read of the run, nothing else" test "$(wc -l <"$TMP/gh.log")" = 1
done
expect "not ours: the open issue is as it was" "$(open_numbers) $(ncomments 101)" "101 0"

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
	'DRY_RUN: ${{ github.event_name == '"'workflow_dispatch'"' && inputs.dry_run }}|GH_REPO: ${{ github.repository }}|GH_TOKEN: ${{ github.token }}|RUN_ID: ${{ github.event.workflow_run.id || inputs.run_id }}|WATCH_RUN_URL: ${{ github.server_url }}/${{ github.repository }}/actions/runs/${{ github.run_id }}|group: main-post-merge-watch-${{ github.event.workflow_run.id || inputs.run_id }}|'
expect "runs are not cancelled or replaced within the group" "$(grep -c '^  cancel-in-progress: false$' <<<"$wf")" 1
expect "every action is pinned by a full commit sha, with its version" \
	"$(grep -E '^[[:space:]]+(- )?uses:' <<<"$wf" | grep -cvE 'uses: [A-Za-z0-9_./-]+@[0-9a-f]{40} # v?[0-9][0-9.]*$')" 0
expect "the one action is the checkout, at the sha main.yaml uses" \
	"$(grep -oE 'uses: [^ ]+' <<<"$wf" | sort -u)" "$(grep -oE 'uses: actions/checkout@[0-9a-f]{40}' "$MAIN_WORKFLOW" | sort -u)"
expect "the checkout names no ref: the default branch's script, never the run's commit" "$(grep -cE '^[[:space:]]+(ref|repository):' <<<"$wf")" 0
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
