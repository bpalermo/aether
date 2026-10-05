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
mkdir -p "$TMP/bin" "$TMP/state"
cat >"$TMP/bin/gh" <<'EOF'
#!/usr/bin/env bash
# Fake gh: reads the API from $FAKE_SNAPSHOT, logs every call to $FAKE_LOG,
# keeps the rolling issue in $FAKE_STATE.
set -uo pipefail
echo "gh $*" >>"$FAKE_LOG"
snap="$FAKE_SNAPSHOT"
case "$1" in
api)
	shift
	if [ "$1" = "-X" ]; then
		echo "WRITE cancel ${3}" >>"$FAKE_LOG"
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
issue)
	sub="$2"
	case "$sub" in
	list) cat "$FAKE_STATE/num" 2>/dev/null || true ;;
	create)
		echo 7 >"$FAKE_STATE/num"
		while [ $# -gt 0 ]; do [ "$1" = "--body-file" ] && cp "$2" "$FAKE_STATE/body"; shift; done
		echo "WRITE issue create" >>"$FAKE_LOG" ;;
	comment)
		echo "WRITE issue comment $3" >>"$FAKE_LOG"
		while [ $# -gt 0 ]; do
			case "$1" in
			--body-file) cat "$2" >>"$FAKE_STATE/comments"; printf '\0' >>"$FAKE_STATE/comments" ;;
			--body) printf '%s\0' "$2" >>"$FAKE_STATE/comments" ;;
			esac
			shift
		done ;;
	view)
		q=""
		while [ $# -gt 0 ]; do [ "$1" = "--jq" ] && q="$2"; shift; done
		{ cat "$FAKE_STATE/body" 2>/dev/null; printf '\0'; cat "$FAKE_STATE/comments" 2>/dev/null; } |
			"$JQ" -Rs 'split("\u0000") | map(select(. != "")) | {body: .[0], comments: (.[1:] | map({body: .}))}' |
			"$JQ" -r "$q" ;;
	close)
		rm -f "$FAKE_STATE/num"
		echo "WRITE issue close $3" >>"$FAKE_LOG" ;;
	esac
	;;
*) echo "fake gh: unexpected $*" >&2; exit 1 ;;
esac
EOF
chmod +x "$TMP/bin/gh"

run_fake() { # run_fake <snapshot> [args...] -> log in $TMP/log
	local snap="$1"
	shift
	: >"$TMP/log"
	PATH="$TMP/bin:$PATH" FAKE_SNAPSHOT="$snap" FAKE_LOG="$TMP/log" FAKE_STATE="$TMP/state" \
		GH_REPO=bpalermo/aether STUCK_NOW=2026-10-05T19:20:00Z RUN_URL=https://example.invalid/run \
		GITHUB_STEP_SUMMARY="$TMP/summary" \
		bash "$SCRIPT" run "$@" >"$TMP/out" 2>&1
}
writes() { grep -c '^WRITE' "$TMP/log"; }

run_fake "$FIX/publish-superseded.json" --dry-run
rc=$?
if [ "$rc" -eq 0 ] && [ "$(writes)" -eq 0 ] && grep -q 'DRY RUN: would cancel 37322742027' "$TMP/out"; then
	pass "run --dry-run: names the cancel, writes nothing"
else
	fail "run --dry-run (rc=$rc, writes=$(writes))"
	sed 's/^/    /' "$TMP/out" "$TMP/log"
fi

run_fake "$FIX/publish-superseded.json"
if grep -qx 'WRITE cancel repos/bpalermo/aether/actions/runs/37322742027/cancel' "$TMP/log" &&
	[ "$(grep -c '^WRITE cancel' "$TMP/log")" -eq 1 ] &&
	grep -qx 'WRITE issue create' "$TMP/log" &&
	grep -q '37344778036' "$TMP/state/body" && grep -q 'CANCELLED' "$TMP/state/body"; then
	pass "run: cancels only the superseded run, opens the issue naming both"
else
	fail "run: cancel/issue"
	sed 's/^/    /' "$TMP/out" "$TMP/log"
fi

run_fake "$FIX/publish-superseded.json"
if [ "$(grep -c '^WRITE issue' "$TMP/log")" -eq 0 ]; then
	pass "run again, same stuck set: no new issue comment"
else
	fail "run again, same stuck set: wrote to the issue"
	sed 's/^/    /' "$TMP/log"
fi

run_fake "$FIX/head-run.json"
if grep -qx 'WRITE issue comment 7' "$TMP/log" && [ "$(grep -c '^WRITE cancel' "$TMP/log")" -eq 0 ]; then
	pass "run, a different stuck set: one comment, no cancel of the head's run"
else
	fail "run, different set"
	sed 's/^/    /' "$TMP/log"
fi

run_fake "$FIX/empty.json"
if grep -qx 'WRITE issue close 7' "$TMP/log"; then
	pass "run, nothing stuck: closes the issue"
else
	fail "run, nothing stuck: issue not closed"
	sed 's/^/    /' "$TMP/log"
fi

run_fake "$FIX/empty.json"
if [ "$(writes)" -eq 0 ]; then
	pass "run, nothing stuck and no issue: writes nothing"
else
	fail "run, nothing stuck and no issue: wrote something"
fi

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS failure(s)"
	exit 1
fi
echo "all passed ($CASES fixtures)"
