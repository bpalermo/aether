#!/usr/bin/env bash
# Hermetic test of scripts/watcher-runs.sh (#1533): a watcher workflow whose own
# run failed is reported. No network, no GitHub. Three halves:
#
#   1. `decide` over snapshots built here (run and job objects shaped as the
#      API returns them): which failed runs are a watcher failing and which are
#      not (a cancelled tick, a pull request's dry run, a fork, a publish-verify
#      that is red because artifacts are missing, a nightly that is red because
#      a suite is, an older failure a later run made good).
#   2. `run` end to end through a fake `gh`: scripts/fake-gh-issues.sh for the
#      issue (JSON, the script's own --jq filters through the real jq) and a
#      fake of the actions API here. What is written and what is not: one
#      issue, labelled; nothing for the same set; a comment when the set
#      changes; closed when none is failing; a stranger's issue left alone; a
#      listing that fails is exit 2 and never closes the issue.
#   3. the WATCHERS table against the workflow files: every file exists, every
#      `job:` names a job, every `step:` matches a step, every workflow that
#      may write an issue is in the table or named below with the reason it is
#      not, and stuck-runs.yaml runs this script whatever its first step did.
#
# Run: bazel test //scripts:watcher_runs_test, or
# bash scripts/watcher_runs_test.sh with jq on PATH.
# shellcheck disable=SC2016 # single-quoted $names here are jq variables.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/watcher-runs.sh"
WORKFLOWS="${TEST_SRCDIR:-}/${TEST_WORKSPACE:-_main}/.github/workflows"
[ -d "$WORKFLOWS" ] || WORKFLOWS="$HERE/../.github/workflows"

if [ -n "${JQ_RLOCATIONPATH:-}" ]; then
	JQ="${TEST_SRCDIR:-${RUNFILES_DIR:-$PWD/..}}/${JQ_RLOCATIONPATH}"
fi
JQ="${JQ:-$(command -v jq)}"
[ -x "$JQ" ] || {
	echo "FAIL: no jq (JQ=${JQ})"
	exit 1
}
export JQ
for f in "$SCRIPT" "$HERE/fake-gh-issues.sh" "$HERE/rolling-issue-lib.sh" "$WORKFLOWS/stuck-runs.yaml"; do
	[ -f "$f" ] || {
		echo "FAIL: $f not found"
		exit 1
	}
done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin" "$TMP/state"

FAILS=0
pass() { echo "PASS  $*"; }
fail() {
	echo "FAIL  $*"
	FAILS=$((FAILS + 1))
}

# --- building a snapshot -----------------------------------------------------------
# start: an empty snapshot. r <file> <id> <conclusion> [event] [created] [repo]
# [attempt]: one completed run. j <run id> <job> <conclusion> [<step>=<conclusion>...].
start() { echo '{"repository":"o/r","runs":{},"jobs":{},"unread":[]}' >"$TMP/snap.json"; }
edit() {
	"$JQ" "$@" "$TMP/snap.json" >"$TMP/snap.next" && mv "$TMP/snap.next" "$TMP/snap.json"
}
r() {
	edit --arg f "$1" --argjson id "$2" --arg c "$3" --arg e "${4:-schedule}" \
		--arg t "${5:-2026-10-09T10:00:00Z}" --arg repo "${6:-o/r}" --argjson a "${7:-1}" \
		'.runs[$f] += [{id: $id, status: "completed", conclusion: $c, event: $e, created_at: $t, run_attempt: $a,
		  head_repository: {full_name: $repo}, html_url: "https://github.com/o/r/actions/runs/\($id)"}]'
}
j() {
	local id="$1" job="$2" c="$3" steps='[]' s
	shift 3
	for s in "$@"; do
		steps="$("$JQ" -c --arg n "${s%%=*}" --arg c "${s#*=}" '. + [{name: $n, conclusion: $c}]' <<<"$steps")"
	done
	edit --arg id "$id" --arg n "$job" --arg c "$c" --argjson steps "$steps" \
		'.jobs[$id] += [{name: $n, conclusion: $c, steps: $steps}]'
}
# decided: "<file>:<id>" of every reported run, sorted, on one line.
decided() { bash "$SCRIPT" decide <"$TMP/snap.json" | awk -F'\t' '{ print $1 ":" $2 }' | LC_ALL=C sort | paste -sd' ' -; }
expect() { # expect <name> <want>
	local got
	got="$(decided)"
	if [ "$got" = "$2" ]; then pass "$1"; else fail "$1: got '${got}', want '$2'"; fi
}

# --- 1. decide ------------------------------------------------------------------------
start
for f in main-watch.yaml publish-verify.yaml stuck-runs.yaml third-party-images.yaml e2e.yaml; do
	r "$f" 1 success
done
expect "every watcher's newest run passed: nothing" ""

# A watcher that looks at the whole state each time: only its newest run.
start
r stuck-runs.yaml 10 failure schedule 2026-10-09T10:00:00Z
expect "stuck-runs' newest run failed: reported" "stuck-runs.yaml:10"
r stuck-runs.yaml 11 success schedule 2026-10-09T10:30:00Z
expect "... and a later tick passed: it did the failed one's work, nothing" ""
# Newest by its date, not by where the API put it in the list.
start
r third-party-images.yaml 21 success schedule 2026-10-09T06:00:00Z
r third-party-images.yaml 20 failure schedule 2026-10-08T06:00:00Z
expect "newest is decided by the run's date, not its place in the listing" ""

# Which conclusions are the reporter failing.
for c in failure timed_out startup_failure; do
	start
	r stuck-runs.yaml 10 "$c"
	expect "a run that ended ${c}: reported" "stuck-runs.yaml:10"
done
for c in cancelled skipped success neutral action_required; do
	start
	r stuck-runs.yaml 10 "$c"
	expect "a run that ended ${c}: not a watcher failing" ""
done

# Runs a person is looking at, and runs that are not this repository's.
start
r stuck-runs.yaml 12 failure pull_request 2026-10-09T12:00:00Z
r stuck-runs.yaml 13 failure workflow_dispatch 2026-10-09T12:10:00Z
r stuck-runs.yaml 14 failure schedule 2026-10-09T12:20:00Z someone/fork
r stuck-runs.yaml 11 success schedule 2026-10-09T10:30:00Z
expect "a pull request's dry run, a dispatch, a fork's run: none is the scheduled watcher" ""

# main-post-merge-watch judges ONE post-merge run per run of its own: a later
# green run for another commit does not make an earlier failure good.
start
r main-watch.yaml 30 failure workflow_run 2026-10-09T09:00:00Z
r main-watch.yaml 31 success workflow_run 2026-10-09T09:30:00Z
r main-watch.yaml 32 timed_out workflow_run 2026-10-09T09:40:00Z
r main-watch.yaml 33 cancelled workflow_run 2026-10-09T09:50:00Z
expect "main-watch: every failed run among the newest, whatever came after" "main-watch.yaml:30 main-watch.yaml:32"
# ... until it leaves the newest 20.
start
r main-watch.yaml 100 failure workflow_run 2026-10-01T00:00:00Z
for i in $(seq 1 19); do r main-watch.yaml "$((100 + i))" success workflow_run "2026-10-02T00:$(printf '%02d' "$i"):00Z"; done
expect "main-watch: a failure that is the 20th newest is still reported" "main-watch.yaml:100"
r main-watch.yaml 120 success workflow_run 2026-10-03T00:00:00Z
expect "main-watch: ... and the 21st is not" ""

# publish-verify goes red by design; only its issue steps failing is the
# reporter failing.
start
r publish-verify.yaml 40 failure workflow_run
j 40 verify failure "Gate (verify the artifacts for this commit exist in the registry)=failure" \
	"Open or update the missing-artifacts issue=success" "Open or update the vacuous-gate issue=skipped"
expect "publish-verify red because artifacts are missing, the issue filed: not a watcher failing" ""
start
r publish-verify.yaml 41 failure schedule
j 41 verify failure "Gate (verify the artifacts for this commit exist in the registry)=failure" \
	"Open or update the missing-artifacts issue=failure"
expect "publish-verify: the missing-artifacts issue step failed: reported" "publish-verify.yaml:41"
if bash "$SCRIPT" decide <"$TMP/snap.json" | grep -qF 'the step "Open or update the missing-artifacts issue" ended failure'; then
	pass "... and the line names the step"
else
	fail "the publish-verify line does not name the step: $(bash "$SCRIPT" decide <"$TMP/snap.json")"
fi
start
r publish-verify.yaml 42 failure workflow_run
j 42 verify failure "Expected-red control (must fail on a never-published commit)=failure" \
	"Open or update the vacuous-gate issue=failure"
expect "publish-verify: the vacuous-gate issue step failed: reported" "publish-verify.yaml:42"
start
r publish-verify.yaml 43 success schedule
j 43 verify success "Close the missing-artifacts issue (a green run for main's head, or a green sweep)=success"
expect "publish-verify green: nothing" ""
# A red run whose jobs cannot be read is not known to have reported: fail closed.
start
r publish-verify.yaml 44 failure schedule
expect "publish-verify red and its jobs unread: reported, not assumed fine" "publish-verify.yaml:44"

# The nightly e2e is red whenever a suite is; only report-failure failing is
# the reporter failing.
start
r e2e.yaml 50 failure schedule
j 50 build success
j 50 uds failure
j 50 report-failure success
expect "e2e red, report-failure filed it: not a watcher failing" ""
start
r e2e.yaml 51 failure schedule
j 51 uds failure
j 51 report-failure failure
expect "e2e red and report-failure failed: reported" "e2e.yaml:51"
start
r e2e.yaml 52 failure workflow_dispatch
j 52 uds failure
j 52 report-failure skipped
expect "a dispatched e2e (report-failure does not run for it): nothing" ""

# Every line is the documented 7-field record.
start
r stuck-runs.yaml 10 failure
r e2e.yaml 51 failure schedule
j 51 report-failure failure
if bash "$SCRIPT" decide <"$TMP/snap.json" | awk -F'\t' 'NF != 7 { bad = 1 } END { exit bad }'; then
	pass "a decision line is 7 tab-separated fields"
else
	fail "a decision line is not 7 tab-separated fields"
fi

# --- 2. run, end to end, through a fake gh ----------------------------------------------
cat >"$TMP/bin/gh" <<EOF
#!/usr/bin/env bash
FAKE_GH_ELSE="$TMP/bin/gh-actions" exec bash "$HERE/fake-gh-issues.sh" "\$@"
EOF
cat >"$TMP/bin/gh-actions" <<'EOF'
#!/usr/bin/env bash
# Fake gh, the actions half: the runs and jobs of $FAKE_SNAPSHOT.
# FAKE_UNREAD: workflow files whose listing answers 502. FAKE_FLAKY: workflow
# files whose listing answers 502 once, then works.
set -uo pipefail
[ "$1" = api ] && [ "$#" -eq 2 ] || { echo "fake gh: unexpected $*" >&2; exit 1; }
path="$2"
case "$path" in
*/actions/workflows/*/runs\?*)
	f="${path#*/actions/workflows/}"; f="${f%%/*}"
	case " ${FAKE_UNREAD:-} " in *" $f "*) echo "gh: Bad Gateway (HTTP 502)" >&2; exit 1 ;; esac
	case " ${FAKE_FLAKY:-} " in *" $f "*)
		[ -f "$FAKE_STATE/flaked.$f" ] || { : >"$FAKE_STATE/flaked.$f"; echo "gh: Bad Gateway (HTTP 502)" >&2; exit 1; } ;;
	esac
	case "$path" in *status=completed*) ;; *) echo "fake gh: the listing must ask for completed runs: $path" >&2; exit 1 ;; esac
	"$JQ" --arg f "$f" '{workflow_runs: (.runs[$f] // [])}' "$FAKE_SNAPSHOT" ;;
*/actions/runs/*/attempts/*/jobs*)
	id="${path#*/actions/runs/}"; id="${id%%/*}"
	"$JQ" -e --arg id "$id" '.jobs | has($id)' "$FAKE_SNAPSHOT" >/dev/null || { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
	"$JQ" --arg id "$id" '{jobs: .jobs[$id]}' "$FAKE_SNAPSHOT" ;;
*) echo "fake gh: unexpected api path $path" >&2; exit 1 ;;
esac
EOF
chmod +x "$TMP/bin/gh" "$TMP/bin/gh-actions"

TITLE="CI: a watcher workflow's own run failed"
reset_state() {
	rm -rf "$TMP/state"
	mkdir -p "$TMP/state"
	echo '[]' >"$TMP/state/issues.json"
	printf '%s\n' bug ci enhancement >"$TMP/state/labels"
}
seed() { # seed <number> <user json> <title> <body>
	"$JQ" --argjson n "$1" --argjson u "$2" --arg t "$3" --arg b "$4" \
		'. + [{number: $n, state: "open", user: $u, title: $t, body: $b, labels: [], comments: []}]' \
		"$TMP/state/issues.json" >"$TMP/state/issues.next" && mv "$TMP/state/issues.next" "$TMP/state/issues.json"
}
field() { "$JQ" -r --argjson n "$1" ".[] | select(.number == \$n) | $2" "$TMP/state/issues.json"; }
has() { # has <text> <grep arguments...>: never `producer | grep -q` under pipefail
	local text="$1"
	shift
	grep -q "$@" <<<"$text"
}
run_fake() { # run_fake [--dry-run] -> RC, $TMP/out, $TMP/log
	: >"$TMP/log"
	PATH="$TMP/bin:$PATH" FAKE_SNAPSHOT="$TMP/snap.json" FAKE_LOG="$TMP/log" FAKE_STATE="$TMP/state" \
		FAKE_UNREAD="${FAKE_UNREAD:-}" FAKE_FLAKY="${FAKE_FLAKY:-}" FAKE_FAIL="${FAKE_FAIL:-}" \
		FAKE_LABELS="${FAKE_LABELS:-reject}" WATCHER_RETRY_SLEEP=0 \
		GH_REPO=o/r RUN_URL=https://example.invalid/run GITHUB_STEP_SUMMARY="$TMP/summary" \
		bash "$SCRIPT" run "$@" >"$TMP/out" 2>&1
	RC=$?
}
writes() { grep -c '^WRITE' "$TMP/log"; }
dump() { sed 's/^/    /' "$TMP/out" "$TMP/log" | cut -c1-260; }

# One watcher failing: main-watch 30 (each) and the nightly's report-failure.
two_failing() {
	start
	r main-watch.yaml 30 failure workflow_run 2026-10-09T09:00:00Z
	r main-watch.yaml 31 success workflow_run 2026-10-09T09:30:00Z
	r stuck-runs.yaml 11 success
	r e2e.yaml 51 failure schedule
	j 51 report-failure failure
}

reset_state
two_failing
run_fake --dry-run
if [ "$RC" -eq 0 ] && [ "$(writes)" -eq 0 ] && grep -q 'DRY RUN: would open or update the issue' "$TMP/out" &&
	! grep -q 'issues' "$TMP/log"; then
	pass "run --dry-run: prints the report, never touches the issues"
else
	fail "run --dry-run (rc=$RC, writes=$(writes))"
	dump
fi

run_fake
body="$(field 7 .body)"
if [ "$RC" -eq 0 ] && grep -qxF "WRITE issue create 7 [bug+ci]: ${TITLE}" "$TMP/log" && [ "$(writes)" -eq 1 ] &&
	has "$body" -F '[30](https://github.com/o/r/actions/runs/30), attempt 1' &&
	has "$body" -F '`gh run rerun 30`' && has "$body" -F 'the report-failure job ended failure' &&
	has "$body" -F '<!-- watcher-runs: e2e.yaml:51/1,main-watch.yaml:30/1 -->' &&
	has "$body" -F 'Reported by: https://example.invalid/run'; then
	pass "run: one issue, labelled, naming each failed run, what it leaves unreported and its command"
else
	fail "run: the issue (rc=$RC)"
	dump
fi

run_fake
if [ "$RC" -eq 0 ] && [ "$(writes)" -eq 0 ]; then
	pass "run again, the same set: nothing is written"
else
	fail "run again, the same set (rc=$RC, writes=$(writes))"
	dump
fi

# The nightly's next run is fine; main-watch 30 is still failed.
r e2e.yaml 53 success schedule 2026-10-10T04:17:00Z
run_fake
if [ "$RC" -eq 0 ] && grep -qx 'WRITE issue comment 7' "$TMP/log" && [ "$(writes)" -eq 1 ] &&
	has "$(field 7 '.comments[-1].body')" -F '<!-- watcher-runs: main-watch.yaml:30/1 -->'; then
	pass "the set changed: one comment with the new set"
else
	fail "the set changed (rc=$RC)"
	dump
fi

# A marker a stranger commented does not pass for the workflow's own.
"$JQ" '.[0].comments += [{user: {login: "mallory", type: "User"}, body: "<!-- watcher-runs: none -->"}]' \
	"$TMP/state/issues.json" >"$TMP/state/issues.next" && mv "$TMP/state/issues.next" "$TMP/state/issues.json"
run_fake
if [ "$RC" -eq 0 ] && [ "$(writes)" -eq 0 ]; then
	pass "a marker in a stranger's comment is not read: the set is still the one the workflow wrote"
else
	fail "a stranger's marker was read (rc=$RC, writes=$(writes))"
	dump
fi

# main-watch 30 was re-run green (the same run id): nothing is failing.
start
r main-watch.yaml 30 success workflow_run 2026-10-09T09:00:00Z o/r 2
r stuck-runs.yaml 11 success
run_fake
if [ "$RC" -eq 0 ] && grep -qx 'WRITE issue close 7 completed' "$TMP/log" && [ "$(field 7 .state)" = closed ]; then
	pass "no watcher run is failing any more: the issue is closed"
else
	fail "nothing failing: not closed (rc=$RC)"
	dump
fi
run_fake
if [ "$RC" -eq 0 ] && [ "$(writes)" -eq 0 ]; then pass "nothing failing, nothing open: nothing is written"; else
	fail "nothing failing, nothing open (rc=$RC, writes=$(writes))"
	dump
fi

# Whose issue: one a person opened under the title is theirs.
reset_state
seed 90 '{"login":"mallory","type":"User"}' "$TITLE" "not the workflow's"
run_fake
if [ "$RC" -eq 0 ] && [ "$(writes)" -eq 0 ]; then pass "a stranger's issue under the title: a clear tick leaves it alone"; else
	fail "a stranger's issue was written to (rc=$RC)"
	dump
fi
two_failing
run_fake
if [ "$RC" -eq 0 ] && grep -qxF "WRITE issue create 91 [bug+ci]: ${TITLE}" "$TMP/log" && [ "$(writes)" -eq 1 ]; then
	pass "... and does not stand in for the workflow's own: a failed watcher opens one"
else
	fail "a stranger's issue stood in for the workflow's (rc=$RC)"
	dump
fi

# A label that is gone: filed all the same, and the step fails.
reset_state
printf '%s\n' bug >"$TMP/state/labels"
run_fake
if [ "$RC" -eq 2 ] && [ "$(field 7 .state)" = open ] && grep -q 'opened without its labels' "$TMP/out"; then
	pass "a label is gone: the issue is filed, and the step exits 2"
else
	fail "a label is gone (rc=$RC)"
	dump
fi

# A listing that fails once is retried, and is not a failure.
reset_state
FAKE_FLAKY="main-watch.yaml e2e.yaml" run_fake
if [ "$RC" -eq 0 ] && grep -qxF "WRITE issue create 7 [bug+ci]: ${TITLE}" "$TMP/log"; then
	pass "a listing that answers 502 once: retried, the tick is whole"
else
	fail "a flaky listing (rc=$RC)"
	dump
fi
# A listing that keeps failing: this tick does not know the whole set. What it
# found may open the issue; it never closes one, and never says the set changed.
reset_state
FAKE_UNREAD=e2e.yaml run_fake
if [ "$RC" -eq 2 ] && grep -qxF "WRITE issue create 7 [bug+ci]: ${TITLE}" "$TMP/log" &&
	has "$(field 7 .body)" -F 'Not read in this tick (the run listing failed), so not counted either way: e2e.yaml' &&
	! has "$(field 7 .body)" -F '[51]'; then
	pass "a listing that keeps failing: exit 2, what was found opens the issue and says what was not read"
else
	fail "an unread listing, nothing open (rc=$RC)"
	dump
fi
FAKE_UNREAD=main-watch.yaml run_fake
if [ "$RC" -eq 2 ] && [ "$(writes)" -eq 0 ] && grep -q '#7 left as it is' "$TMP/out"; then
	pass "an unread listing, the issue open: exit 2, the issue is left as it is"
else
	fail "an unread listing, the issue open (rc=$RC, writes=$(writes))"
	dump
fi
start
r stuck-runs.yaml 11 success
FAKE_UNREAD=main-watch.yaml run_fake
if [ "$RC" -eq 2 ] && [ "$(writes)" -eq 0 ] && [ "$(field 7 .state)" = open ]; then
	pass "an unread listing and nothing found: the open issue is NOT closed"
else
	fail "an unread listing closed the issue (rc=$RC, writes=$(writes))"
	dump
fi
# The issue API failing is a broken check, never "nothing to report".
two_failing
reset_state
FAKE_FAIL=list run_fake
if [ "$RC" -eq 2 ] && [ "$(writes)" -eq 0 ]; then pass "the issue listing fails: exit 2, nothing written blind"; else
	fail "the issue listing fails (rc=$RC)"
	dump
fi

# --- 3. the table and the workflow files ---------------------------------------------------
# Workflows that may write an issue and are NOT watchers, each with its reason.
# A new workflow with `issues: write` must go into WATCHERS, or here.
NOT_WATCHED="$(printf '%s\t%s\n' \
	main.yaml 'refresh-pin-prs notes a conflict on an issue; a failed run of main.yaml is what main-watch.yaml reports' \
	proxy-release.yml 'a publisher: its issue is a note on a release it made, not a report of something else failing')"
table="$(bash "$SCRIPT" watchers)"
while IFS=$'\t' read -r file which counts unreported todo; do
	wf="$WORKFLOWS/$file"
	if [ ! -f "$wf" ]; then
		fail "WATCHERS names ${file}, which is not a workflow file"
		continue
	fi
	case "$which" in latest | each) ;; *) fail "${file}: '${which}' is neither latest nor each" ;; esac
	[ -n "$unreported" ] && [ -n "$todo" ] || fail "${file}: the table does not say what goes unreported, or what to do"
	case "$counts" in
	run) pass "${file}: any failed run counts" ;;
	job:*)
		if grep -qE "^  ${counts#job:}:\$" "$wf"; then pass "${file}: ${counts#job:} is a job of it"; else
			fail "${file} has no job called ${counts#job:}"
		fi
		;;
	step:*)
		n="$(sed -n 's/^ *- name: //p' "$wf" | grep -ciE -- "${counts#step:}" || true)"
		if [ "$n" -ge 1 ]; then pass "${file}: ${n} step(s) match '${counts#step:}'"; else
			fail "${file}: no step name matches '${counts#step:}'"
		fi
		# Every step that runs an issue script is a step the rule sees.
		while IFS= read -r name; do
			grep -qiE -- "${counts#step:}" <<<"$name" || fail "${file}: the step '${name}' writes an issue and does not match '${counts#step:}'"
		done < <(awk '/^ *- name: / { sub(/^ *- name: /, ""); name = $0 } /gh issue |-issue\.sh/ && !/^ *#/ { print name }' "$wf" | sort -u)
		;;
	*) fail "${file}: watcher-runs has no rule for '${counts}'" ;;
	esac
done <<<"$table"
for wf in "$WORKFLOWS"/*.yaml "$WORKFLOWS"/*.yml; do
	[ -f "$wf" ] || continue
	grep -qE '^ +issues: write' "$wf" || continue
	file="$(basename "$wf")"
	if awk -F'\t' -v f="$file" '$1 == f { found = 1 } END { exit !found }' <<<"$table"; then
		pass "${file} may write an issue and is watched"
	elif awk -F'\t' -v f="$file" '$1 == f { found = 1 } END { exit !found }' <<<"$NOT_WATCHED"; then
		pass "${file} may write an issue and is not a watcher (named with its reason)"
	else
		fail "${file} may write an issue: add it to WATCHERS in scripts/watcher-runs.sh, or to NOT_WATCHED here with the reason"
	fi
done
# The mode of main-watch is the one that matters most: each run of it judges
# one post-merge run.
if awk -F'\t' '$1 == "main-watch.yaml" && $2 == "each" { ok = 1 } END { exit !ok }' <<<"$table"; then
	pass "main-watch.yaml: every failed run is reported, not only the newest"
else
	fail "main-watch.yaml must be read as 'each'"
fi

# stuck-runs.yaml runs the step whatever the step before it did, as a dry run
# where the stuck-run step is one, and a pull request that changes any of the
# scripts runs it.
SR="$WORKFLOWS/stuck-runs.yaml"
step="$(awk '/^      - name: / { on = ($0 ~ /watcher/) } on' "$SR")"
if has "$step" -F 'if: ${{ !cancelled() }}'; then pass "stuck-runs.yaml: the watcher step runs even when the step before it failed"; else
	fail 'stuck-runs.yaml: the watcher step has no "if: ${{ !cancelled() }}"'
fi
if has "$step" -F 'scripts/watcher-runs.sh run --dry-run' && has "$step" -E '^ +scripts/watcher-runs\.sh run$'; then
	pass "stuck-runs.yaml: the step runs the script, and as a dry run when DRY_RUN says so"
else
	fail "stuck-runs.yaml: the watcher step does not run scripts/watcher-runs.sh both ways"
fi
missing=""
for f in scripts/stuck-runs.sh scripts/watcher-runs.sh scripts/rolling-issue-lib.sh; do
	grep -qE "^ +- ${f//./\\.}\$" "$SR" || missing+=" $f"
done
if [ -z "$missing" ]; then pass "stuck-runs.yaml: a pull request that changes either script or the library runs the dry run"; else
	fail "stuck-runs.yaml: the pull_request paths lack:${missing}"
fi
if grep -qE '^ +actions: write$' "$SR" && grep -qE '^ +issues: write$' "$SR"; then
	pass "stuck-runs.yaml: the job can read runs and write issues"
else
	fail "stuck-runs.yaml: the job's permissions changed"
fi

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS check(s) failed"
	exit 1
fi
echo "all checks passed"
