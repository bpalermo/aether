#!/usr/bin/env bash
# Hermetic test of scripts/stuck-runs.sh (the stuck-runs watchdog): no network,
# no GitHub. Two halves:
#
#   1. `decide` over every canned snapshot in testdata/stuck-runs/<case>.json
#      (raw GitHub API run/job objects plus branch heads) must print exactly the
#      "<action>\t<run id>" lines in <case>.want. The cases pin what is cancelled
#      (a superseded run with a newer replacement, a run on a deleted branch) and
#      everything that must never be: the head's own run, a run with no newer
#      replacement (path-filtered pages), re-runs, schedule/dispatch runs, forks,
#      unknown heads.
#   2. `run` end to end through a fake `gh` that serves the API from a snapshot
#      and logs every write: --dry-run must write NOTHING (no cancel, no issue);
#      a real run cancels exactly the superseded run and opens one issue; the
#      same stuck set again adds no comment; a clear tick closes the issue.
#   3. a run GitHub refuses to cancel (#1301): cancel and force-cancel both 409,
#      so it is reported ONCE as uncancellable, remembered in a hidden marker on
#      the issue (read back even once the issue is closed), left out of the
#      stuck set after that, and a NEW stuck run is still reported; a transient
#      cancel failure is not remembered.
#
# Run: bazel test //scripts:stuck_runs_test (jq is the Bazel-pinned one), or
#      bash scripts/stuck_runs_test.sh with jq on PATH.
# shellcheck disable=SC2016 # single-quoted $names here are jq variables (and
# Markdown backticks), never shell expansions.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/stuck-runs.sh"
FIX="$HERE/testdata/stuck-runs"

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
echo "jq: $JQ ($("$JQ" --version))"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FAILS=0
CASES=0
pass() { echo "PASS  $*"; }
fail() {
	echo "FAIL  $*"
	FAILS=$((FAILS + 1))
}

# --- 1. decide over every fixture ---------------------------------------------
for json in "$FIX"/*.json; do
	name="$(basename "$json" .json)"
	want="$FIX/$name.want"
	CASES=$((CASES + 1))
	if [ ! -f "$want" ]; then
		fail "$name: no $name.want"
		continue
	fi
	if ! bash "$SCRIPT" decide <"$json" >"$TMP/$name.out" 2>"$TMP/$name.err"; then
		fail "$name: decide exited non-zero: $(cat "$TMP/$name.err")"
		continue
	fi
	cut -f1,2 "$TMP/$name.out" >"$TMP/$name.got"
	if diff -u "$want" "$TMP/$name.got" >"$TMP/$name.diff"; then
		pass "$name ($(wc -l <"$want" | tr -d ' ') decision(s))"
	else
		fail "$name"
		sed 's/^/    /' "$TMP/$name.diff"
	fi
	# Every line is the documented 10-field record.
	if awk -F'\t' 'NF != 10 { bad = 1 } END { exit bad }' "$TMP/$name.out"; then :; else
		fail "$name: a decision line is not 10 tab-separated fields"
	fi
done
# The anti-vacuity half: the fixture set must contain both verdicts.
grep -hq '^cancel' "$FIX"/*.want || fail "no fixture expects a cancel"
grep -hq '^report' "$FIX"/*.want || fail "no fixture expects a report"
[ "$CASES" -ge 8 ] || fail "only $CASES fixtures found under $FIX"

# The threshold is honoured: at 400 minutes nothing in the publish incident
# (stuck 308m, pending 142m) qualifies.
if [ -z "$(STUCK_THRESHOLD_MINUTES=400 bash "$SCRIPT" decide <"$FIX/publish-superseded.json")" ]; then
	pass "threshold 400m: publish-superseded reports nothing"
else
	fail "threshold 400m: publish-superseded still reported something"
fi
# A bad threshold is a broken check, not "nothing stuck".
STUCK_THRESHOLD_MINUTES=1h bash "$SCRIPT" decide <"$FIX/empty.json" >/dev/null 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then pass "bad threshold exits 2"; else fail "bad threshold exited $rc, want 2"; fi

# --- 2. run, end to end, through a fake gh --------------------------------------
# The issues are scripts/fake-gh-issues.sh (JSON, the script's own --jq filters
# through the real jq); the actions API is the fake below, which that one hands
# every other call to.
mkdir -p "$TMP/bin" "$TMP/state"
cat >"$TMP/bin/gh" <<EOF
#!/usr/bin/env bash
FAKE_GH_ELSE="$TMP/bin/gh-actions" exec bash "$HERE/fake-gh-issues.sh" "\$@"
EOF
cat >"$TMP/bin/gh-actions" <<'EOF'
#!/usr/bin/env bash
# Fake gh, the actions half: reads the API from $FAKE_SNAPSHOT and logs every
# cancel to $FAKE_LOG.
set -uo pipefail
snap="$FAKE_SNAPSHOT"
case "$1" in
api)
	shift
	if [ "$1" = "-X" ]; then
		echo "WRITE cancel ${3}" >>"$FAKE_LOG"
		# FAKE_CANCEL_409 / FAKE_CANCEL_500: run ids whose cancel AND force-cancel
		# GitHub refuses (the #1301 zombie) / fails transiently.
		id="${3#*/actions/runs/}"; id="${id%%/*}"
		case " ${FAKE_CANCEL_409:-} " in *" $id "*)
			echo "gh: Cannot cancel a workflow run that is not in progress (HTTP 409)" >&2; exit 1 ;;
		esac
		case " ${FAKE_CANCEL_500:-} " in *" $id "*)
			echo "gh: Server Error (HTTP 500)" >&2; exit 1 ;;
		esac
		exit 0
	fi
	path="$1"
	case "$path" in
	*/actions/runs\?status=*)
		st="${path#*status=}"; st="${st%%&*}"
		"$JQ" --arg s "$st" '{workflow_runs: [.runs[] | select(.status == $s)]}' "$snap" ;;
	*/actions/runs/*/jobs*)
		id="${path#*/actions/runs/}"; id="${id%%/*}"
		"$JQ" -e --arg id "$id" '.jobs | has($id)' "$snap" >/dev/null || { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
		"$JQ" --arg id "$id" '{jobs: .jobs[$id]}' "$snap" ;;
	*/actions/workflows/*/runs\?branch=*)
		wf="${path#*/actions/workflows/}"; wf="${wf%%/*}"
		b="${path#*branch=}"; b="${b%%&*}"; b="$("$JQ" -rn --arg b "$b" '$b | gsub("%2F"; "/")')"
		"$JQ" --arg k "$wf:$b" '{workflow_runs: (.workflow_runs[$k] // [])}' "$snap" ;;
	*/git/ref/heads/*)
		b="${path#*/git/ref/heads/}"
		if ! "$JQ" -e --arg b "$b" '.branches | has($b)' "$snap" >/dev/null; then
			echo "gh: Server Error (HTTP 502)" >&2; exit 1
		fi
		sha="$("$JQ" -r --arg b "$b" '.branches[$b] // empty' "$snap")"
		[ -n "$sha" ] || { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
		printf '{"object":{"sha":"%s"}}\n' "$sha" ;;
	*) echo "fake gh: unexpected api path $path" >&2; exit 1 ;;
	esac
	;;
*) echo "fake gh: unexpected $*" >&2; exit 1 ;;
esac
EOF
chmod +x "$TMP/bin/gh" "$TMP/bin/gh-actions"

TITLE="CI: workflow runs stuck before they started"
BOT='{"login":"github-actions[bot]","type":"Bot"}'
MALLORY='{"login":"mallory","type":"User"}'
reset_state() {
	rm -rf "$TMP/state"
	mkdir -p "$TMP/state"
	echo '[]' >"$TMP/state/issues.json"
	printf '%s\n' bug ci enhancement >"$TMP/state/labels"
}
# seed <number> <open|closed> <user json> <title> <body> [pull_request]
seed() {
	"$JQ" --argjson n "$1" --arg s "$2" --argjson u "$3" --arg t "$4" --arg b "$5" --arg pr "${6:-}" \
		'. + [{number: $n, state: $s, user: $u, title: $t, body: $b, labels: [], comments: []}
		      | if $pr != "" then .pull_request = {} else . end]' \
		"$TMP/state/issues.json" >"$TMP/state/issues.next" && mv "$TMP/state/issues.next" "$TMP/state/issues.json"
}
# say <number> <user json> <comment>
say() {
	"$JQ" --argjson n "$1" --argjson u "$2" --arg b "$3" \
		'map(if .number == $n then .comments += [{user: $u, body: $b}] else . end)' \
		"$TMP/state/issues.json" >"$TMP/state/issues.next" && mv "$TMP/state/issues.next" "$TMP/state/issues.json"
}
body_of() { "$JQ" -r --argjson n "$1" '.[] | select(.number == $n) | .body' "$TMP/state/issues.json"; }
comments_of() { "$JQ" -r --argjson n "$1" '.[] | select(.number == $n) | .comments[].body' "$TMP/state/issues.json"; }
# Never `producer | grep -q` under pipefail: grep leaves at the first match and
# the producer dies of SIGPIPE. These read the whole text first.
in_body() {
	local n="$1"
	shift
	grep "$@" <<<"$(body_of "$n")"
}
in_comments() {
	local n="$1"
	shift
	grep "$@" <<<"$(comments_of "$n")"
}
state_of() { "$JQ" -r --argjson n "$1" '.[] | select(.number == $n) | .state' "$TMP/state/issues.json"; }
reset_state

run_fake() { # run_fake <snapshot> [args...] -> log in $TMP/log, exit status in RC
	local snap="$1"
	shift
	: >"$TMP/log"
	PATH="$TMP/bin:$PATH" FAKE_SNAPSHOT="$snap" FAKE_LOG="$TMP/log" FAKE_STATE="$TMP/state" \
		FAKE_CANCEL_409="${FAKE_CANCEL_409:-}" FAKE_CANCEL_500="${FAKE_CANCEL_500:-}" \
		FAKE_FAIL="${FAKE_FAIL:-}" FAKE_LABELS="${FAKE_LABELS:-reject}" \
		GH_REPO=bpalermo/aether STUCK_NOW=2026-10-05T19:20:00Z RUN_URL=https://example.invalid/run \
		GITHUB_STEP_SUMMARY="$TMP/summary" \
		bash "$SCRIPT" run "$@" >"$TMP/out" 2>&1
	RC=$?
}
writes() { grep -c '^WRITE' "$TMP/log"; }
dump() { sed 's/^/    /' "$TMP/out" "$TMP/log"; }

run_fake "$FIX/publish-superseded.json" --dry-run
if [ "$RC" -eq 0 ] && [ "$(writes)" -eq 0 ] && grep -q 'DRY RUN: would cancel 37322742027' "$TMP/out"; then
	pass "run --dry-run: names the cancel, writes nothing"
else
	fail "run --dry-run (rc=$RC, writes=$(writes))"
	dump
fi

run_fake "$FIX/publish-superseded.json"
if [ "$RC" -eq 0 ] && grep -qx 'WRITE cancel repos/bpalermo/aether/actions/runs/37322742027/cancel' "$TMP/log" &&
	[ "$(grep -c '^WRITE cancel' "$TMP/log")" -eq 1 ] &&
	grep -qxF "WRITE issue create 7 [bug+ci]: ${TITLE}" "$TMP/log" &&
	in_body 7 -q '37344778036' && in_body 7 -q 'CANCELLED'; then
	pass "run: cancels only the superseded run, opens the issue naming both, labelled bug and ci (#1568)"
else
	fail "run: cancel/issue (rc=$RC)"
	dump
fi

run_fake "$FIX/publish-superseded.json"
if [ "$RC" -eq 0 ] && [ "$(grep -c '^WRITE issue' "$TMP/log")" -eq 0 ]; then
	pass "run again, same stuck set: no new issue comment"
else
	fail "run again, same stuck set: wrote to the issue"
	dump
fi

run_fake "$FIX/head-run.json"
if grep -qx 'WRITE issue comment 7' "$TMP/log" && [ "$(grep -c '^WRITE cancel' "$TMP/log")" -eq 0 ]; then
	pass "run, a different stuck set: one comment, no cancel of the head's run"
else
	fail "run, different set"
	dump
fi

run_fake "$FIX/empty.json"
if grep -qx 'WRITE issue close 7 completed' "$TMP/log" && [ "$(state_of 7)" = closed ]; then
	pass "run, nothing stuck: closes the issue"
else
	fail "run, nothing stuck: issue not closed"
	dump
fi

run_fake "$FIX/empty.json"
if [ "$(writes)" -eq 0 ]; then
	pass "run, nothing stuck and no issue: writes nothing"
else
	fail "run, nothing stuck and no issue: wrote something"
fi

# --- 3. a run GitHub will not cancel (#1301) ----------------------------------
# deleted-branch.json is the real zombie: a proxy run queued since 2026-09-12 on
# a deleted branch, whose cancel and force-cancel both answer HTTP 409.
Z=34723047990
ZMARK="<!-- stuck-runs-uncancellable: ${Z} -->"

# The live state when this shipped: #7 open, already reporting the zombie under
# the pre-#1301 marker, so the stuck set alone has NOT changed.
reset_state
seed 7 open "$BOT" "$TITLE" "$(printf 'old report\n\n<!-- stuck-runs: %s -->\n' "$Z")"

FAKE_CANCEL_409="$Z" run_fake "$FIX/deleted-branch.json"
if grep -qx "WRITE cancel repos/bpalermo/aether/actions/runs/${Z}/cancel" "$TMP/log" &&
	grep -qx "WRITE cancel repos/bpalermo/aether/actions/runs/${Z}/force-cancel" "$TMP/log" &&
	grep -qx 'WRITE issue comment 7' "$TMP/log" && ! grep -q '^WRITE issue close' "$TMP/log" &&
	in_comments 7 -qF "uncancellable — needs GitHub support" &&
	in_comments 7 -qF -- "$ZMARK"; then
	pass "uncancellable, tick 1: cancel + force-cancel refused, reported once with the marker (same stuck set or not)"
else
	fail "uncancellable, tick 1"
	dump
fi

FAKE_CANCEL_409="$Z" run_fake "$FIX/deleted-branch.json"
if [ "$(grep -c '^WRITE cancel' "$TMP/log")" -eq 0 ] &&
	grep -qx 'WRITE issue close 7 completed' "$TMP/log" &&
	grep -q 'stuck runs (threshold 60m): 0' "$TMP/out" && grep -q "^ignored ${Z} " "$TMP/out" &&
	[ "$(comments_of 7 | grep -cF -- "$ZMARK")" -ge 2 ]; then
	pass "uncancellable, tick 2: not retried, not stuck, issue closed with the marker carried"
else
	fail "uncancellable, tick 2"
	dump
fi

FAKE_CANCEL_409="$Z" run_fake "$FIX/deleted-branch.json"
if [ "$(writes)" -eq 0 ] && grep -q "^ignored ${Z} " "$TMP/out"; then
	pass "uncancellable, tick 3: remembered from the CLOSED issue, writes nothing"
else
	fail "uncancellable, tick 3"
	dump
fi

FAKE_CANCEL_409="$Z" run_fake "$FIX/deleted-branch.json" --dry-run
if [ "$(writes)" -eq 0 ] && ! grep -q "would cancel ${Z}" "$TMP/out" && grep -q 'DRY RUN: nothing stuck' "$TMP/out"; then
	pass "uncancellable, dry run: reads the memory too"
else
	fail "uncancellable, dry run"
	dump
fi

# A NEW stuck run while GitHub still lists the zombie: reported as usual, on a
# new issue that carries the memory forward and does not count the zombie.
"$JQ" -s '.[0] + {runs: (.[0].runs + .[1].runs), jobs: (.[0].jobs + .[1].jobs),
	branches: (.[0].branches + .[1].branches), workflow_runs: (.[0].workflow_runs + .[1].workflow_runs)}' \
	"$FIX/deleted-branch.json" "$FIX/head-run.json" >"$TMP/zombie-plus-new.json"
FAKE_CANCEL_409="$Z" run_fake "$TMP/zombie-plus-new.json"
if grep -qxF "WRITE issue create 8 [bug+ci]: ${TITLE}" "$TMP/log" && [ "$(grep -c '^WRITE cancel' "$TMP/log")" -eq 0 ] &&
	in_body 8 -q '37000000004' && ! in_body 8 -q "^| \[${Z}\]" &&
	in_body 8 -q "Not counted (already reported as uncancellable" &&
	in_body 8 -qF -- "$ZMARK" && in_body 8 -qF -- '<!-- stuck-runs: 37000000004 -->'; then
	pass "uncancellable + a new stuck run: the new one is reported, the zombie only carried"
else
	fail "uncancellable + a new stuck run"
	dump
	body_of 8 | sed 's/^/    | /'
fi

# Once GitHub stops listing the zombie, the memory is dropped.
run_fake "$FIX/empty.json"
if grep -qx 'WRITE issue close 8 completed' "$TMP/log" &&
	! in_comments 8 -qF -- "stuck-runs-uncancellable"; then
	pass "uncancellable run gone: closes without carrying the marker"
else
	fail "uncancellable run gone"
	dump
fi

# A transient failure (HTTP 500) is NOT uncancellable: no force-cancel, nothing
# remembered, and the next tick tries the cancel again.
reset_state
FAKE_CANCEL_500="$Z" run_fake "$FIX/deleted-branch.json"
if grep -qx "WRITE cancel repos/bpalermo/aether/actions/runs/${Z}/cancel" "$TMP/log" &&
	! grep -q '^WRITE cancel .*/force-cancel$' "$TMP/log" && in_body 7 -q 'cancel FAILED' &&
	! in_body 7 -qF 'stuck-runs-uncancellable'; then
	FAKE_CANCEL_500="$Z" run_fake "$FIX/deleted-branch.json"
	if grep -qx "WRITE cancel repos/bpalermo/aether/actions/runs/${Z}/cancel" "$TMP/log"; then
		pass "transient cancel failure: not remembered, retried next tick"
	else
		fail "transient cancel failure: not retried"
		dump
	fi
else
	fail "transient cancel failure"
	dump
fi

# --- 4. whose issue it is, and how it is opened (#1532, #1568) ------------------
# An issue somebody else opened under the title is theirs: a clear tick neither
# comments on it nor closes it.
reset_state
seed 99 open "$MALLORY" "$TITLE" "not the watchdog's"
run_fake "$FIX/empty.json"
if [ "$RC" -eq 0 ] && [ "$(writes)" -eq 0 ] && [ "$(state_of 99)" = open ]; then
	pass "a stranger's issue under the title: a clear tick leaves it alone"
else
	fail "a stranger's issue under the title was written to (rc=$RC)"
	dump
fi
# ... and it does not stand in for the watchdog's own: a stuck run opens one.
# Neither does a pull request of the bot's, nor another bot's issue.
seed 98 open "$BOT" "$TITLE" "a pull request" pr
seed 97 open '{"login":"dependabot[bot]","type":"Bot"}' "$TITLE" "another bot's"
seed 96 open "$BOT" "Re: ${TITLE}" "a longer title"
# The login alone is not the account: its type is part of who it is.
seed 95 open '{"login":"github-actions[bot]","type":"User"}' "$TITLE" "the login, on an account that is not a bot"
run_fake "$FIX/head-run.json"
if [ "$RC" -eq 0 ] && grep -qxF "WRITE issue create 100 [bug+ci]: ${TITLE}" "$TMP/log" &&
	[ "$(grep -c '^WRITE issue' "$TMP/log")" -eq 1 ]; then
	pass "a stranger's issue, a bot's pull request, another bot's issue, a longer title, the login on a user: the watchdog opens its own"
else
	fail "the watchdog did not open its own issue next to the ones that are not its own (rc=$RC)"
	dump
fi

# A marker in a comment anyone can write is not the watchdog's memory: the run
# is still counted, and its cancel still tried.
reset_state
seed 7 closed "$BOT" "$TITLE" "an old report"
say 7 "$MALLORY" "$ZMARK"
run_fake "$FIX/deleted-branch.json"
if grep -qx "WRITE cancel repos/bpalermo/aether/actions/runs/${Z}/cancel" "$TMP/log" &&
	! grep -q "^ignored ${Z} " "$TMP/out" && grep -q 'stuck runs (threshold 60m): 1' "$TMP/out"; then
	pass "an uncancellable marker in a stranger's comment is not read: the run is still stuck"
else
	fail "a stranger's uncancellable marker hid a stuck run"
	dump
fi
reset_state
seed 7 closed "$BOT" "$TITLE" "an old report"
say 7 '{"login":"github-actions[bot]","type":"User"}' "$ZMARK"
run_fake "$FIX/deleted-branch.json"
if ! grep -q "^ignored ${Z} " "$TMP/out" && grep -q 'stuck runs (threshold 60m): 1' "$TMP/out"; then
	pass "... nor in a comment by the login on an account that is not a bot"
else
	fail "a marker by the login on a user account hid a stuck run"
	dump
fi
# The same for the set marker: a stranger's comment naming the new set must not
# pass for the watchdog having reported it.
reset_state
seed 7 open "$BOT" "$TITLE" "$(printf 'old report\n\n<!-- stuck-runs: 1 -->\n')"
say 7 "$MALLORY" '<!-- stuck-runs: 37000000004 -->'
run_fake "$FIX/head-run.json"
if grep -qx 'WRITE issue comment 7' "$TMP/log"; then
	pass "a set marker in a stranger's comment is not read: the changed set is reported"
else
	fail "a stranger's set marker silenced a changed set"
	dump
fi

# A label that is gone: the report is filed all the same, and the run fails.
for mode in reject drop; do
	reset_state
	printf '%s\n' bug >"$TMP/state/labels"
	FAKE_LABELS="$mode" run_fake "$FIX/head-run.json"
	if [ "$RC" -eq 2 ] && [ "$(state_of 7)" = open ] && in_body 7 -q '37000000004' &&
		grep -q 'opened without its labels' "$TMP/out"; then
		pass "a label is gone (GitHub would ${mode} it): the issue is filed, and the run exits 2"
	else
		fail "a label is gone (${mode}): rc=$RC"
		dump
	fi
done

# Two ticks that both found nothing and both opened: the older issue wins, the
# report moves there, and the newer one is closed.
reset_state
seed 6 closed "$BOT" "an unrelated closed issue" "x"
"$JQ" -n --argjson u "$BOT" --arg t "$TITLE" \
	'{number: 3, state: "open", user: $u, title: $t, body: "the other tick", labels: [], comments: []}' >"$TMP/state/race.json"
run_fake "$FIX/head-run.json"
if [ "$RC" -eq 0 ] && grep -qxF "WRITE issue create 7 [bug+ci]: ${TITLE}" "$TMP/log" &&
	grep -qx 'WRITE issue comment 3' "$TMP/log" && grep -qx 'WRITE issue close 7 not_planned' "$TMP/log" &&
	in_comments 3 -q '37000000004' && [ "$(state_of 3)" = open ] && grep -qx 'reported on #3' "$TMP/out"; then
	pass "two issues opened in the same moment: the report moves to the older, the newer is closed"
else
	fail "duplicate fold (rc=$RC)"
	dump
fi
# The ticks after it. The closed duplicate, #7, has the highest number and is
# never written to again: the memory must be read from #3, the issue that won.
# A run turns out uncancellable (recorded on #3); the next tick must remember.
FAKE_CANCEL_409="$Z" run_fake "$FIX/deleted-branch.json"
if grep -qx 'WRITE issue comment 3' "$TMP/log" && in_comments 3 -qF -- "$ZMARK" && ! in_body 7 -qF -- "$ZMARK"; then
	FAKE_CANCEL_409="$Z" run_fake "$FIX/deleted-branch.json"
	if [ "$(grep -c '^WRITE cancel' "$TMP/log")" -eq 0 ] && grep -q "^ignored ${Z} " "$TMP/out"; then
		pass "after a fold: the memory is read from the issue that won, not from the closed duplicate"
	else
		fail "after a fold: the uncancellable run recorded on #3 was forgotten (the closed duplicate #7 was read)"
		dump
	fi
else
	fail "after a fold: the uncancellable run was not recorded on #3"
	dump
fi
# ... and once that issue is closed too, it is still the one read (the newest
# closed that is not a folded duplicate).
FAKE_CANCEL_409="$Z" run_fake "$FIX/deleted-branch.json"
if [ "$(state_of 3)" = closed ] && [ "$(grep -c '^WRITE cancel' "$TMP/log")" -eq 0 ] && grep -q "^ignored ${Z} " "$TMP/out"; then
	pass "after a fold, everything closed: still read from the issue that won"
else
	fail "after a fold, everything closed: the memory was lost"
	dump
fi
# Which issue holds the record, with several of the watchdog's own: the open
# one before any closed one, and among closed ones the newest.
reset_state
seed 5 closed "$BOT" "$TITLE" "an older report, since closed"
seed 7 open "$BOT" "$TITLE" "$(printf 'the open report\n\n<!-- stuck-runs: %s -->\n%s\n' "$Z" "$ZMARK")"
FAKE_CANCEL_409="$Z" run_fake "$FIX/deleted-branch.json"
if [ "$(grep -c '^WRITE cancel' "$TMP/log")" -eq 0 ] && grep -q "^ignored ${Z} " "$TMP/out"; then
	pass "an open issue and an older closed one: the record is the open one's"
else
	fail "the record was not read from the open issue"
	dump
fi
reset_state
seed 5 closed "$BOT" "$TITLE" "$(printf 'an older report\n\n%s\n' "$ZMARK")"
seed 8 closed "$BOT" "$TITLE" "a newer report, which no longer remembers the run"
run_fake "$FIX/deleted-branch.json"
if grep -qx "WRITE cancel repos/bpalermo/aether/actions/runs/${Z}/cancel" "$TMP/log" && ! grep -q "^ignored ${Z} " "$TMP/out"; then
	pass "two closed issues: the record is the newest one's (a run it dropped is counted again)"
else
	fail "the record was read from the older of two closed issues"
	dump
fi

# The issue API failing is a broken check (exit 2), never "nothing to report".
reset_state
FAKE_FAIL=list run_fake "$FIX/head-run.json"
if [ "$RC" -eq 2 ] && [ "$(grep -c '^WRITE issue' "$TMP/log")" -eq 0 ]; then
	pass "the issue listing fails: exit 2, nothing written blind"
else
	fail "the issue listing fails: rc=$RC"
	dump
fi
FAKE_FAIL=create run_fake "$FIX/head-run.json"
if [ "$RC" -eq 2 ]; then pass "the create fails: exit 2"; else
	fail "the create fails: rc=$RC"
	dump
fi

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS failure(s)"
	exit 1
fi
echo "all passed ($CASES fixtures)"
