#!/usr/bin/env bash
# Hermetic test of scripts/e2e-report-failure.sh, the step of the
# `report-failure` job of .github/workflows/e2e.yaml: no network, no GitHub.
#
# The fake `gh` is scripts/fake-gh-issues.sh: it keeps the issues as JSON and
# applies the script's own `--jq` filters with jq (the Bazel-pinned one, or the
# one on PATH), so which issue is "the open one" is decided by the real filter.
# What is held:
#   - a failed night opens ONE issue, labelled, naming the failed jobs and the
#     run; the next failed night comments on it;
#   - an issue somebody else opened under the title, another bot's, a pull
#     request and a longer title are not the workflow's: none is written to
#     (#1532);
#   - a label that is gone still files the issue, and fails the step (#1568);
#   - an issue API that does not answer fails the step;
#   - the workflow runs this script, in a job that checks out the script and
#     the library it sources and nothing else.
#
# Run: bazel test //scripts:e2e_report_failure_test, or
# bash scripts/e2e_report_failure_test.sh with jq on PATH.
# shellcheck disable=SC2016 # single-quoted $names here are jq variables (and
# Markdown backticks), never shell expansions.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/e2e-report-failure.sh"
WORKFLOW="${TEST_SRCDIR:-}/${TEST_WORKSPACE:-_main}/.github/workflows/e2e.yaml"
[ -f "$WORKFLOW" ] || WORKFLOW="$HERE/../.github/workflows/e2e.yaml"

if [ -n "${JQ_RLOCATIONPATH:-}" ]; then
	JQ="${TEST_SRCDIR:-${RUNFILES_DIR:-$PWD/..}}/${JQ_RLOCATIONPATH}"
fi
JQ="${JQ:-$(command -v jq)}"
[ -x "$JQ" ] || {
	echo "FAIL: no jq (JQ=${JQ})"
	exit 1
}
export JQ
for f in "$SCRIPT" "$WORKFLOW" "$HERE/fake-gh-issues.sh" "$HERE/rolling-issue-lib.sh"; do
	[ -f "$f" ] || {
		echo "FAIL: $f not found"
		exit 1
	}
done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin" "$TMP/state"
cat >"$TMP/bin/gh" <<EOF
#!/usr/bin/env bash
exec bash "$HERE/fake-gh-issues.sh" "\$@"
EOF
chmod +x "$TMP/bin/gh"

FAILS=0
pass() { echo "PASS  $*"; }
fail() {
	echo "FAIL  $*"
	FAILS=$((FAILS + 1))
	sed 's/^/    /' "$TMP/out" "$TMP/log" 2>/dev/null | cut -c1-240
}

TITLE="Nightly e2e failing"
BOT='{"login":"github-actions[bot]","type":"Bot"}'
reset_state() {
	echo '[]' >"$TMP/state/issues.json"
	printf '%s\n' bug ci enhancement >"$TMP/state/labels"
}
# seed <number> <user json> <title> [pull_request]
seed() {
	"$JQ" --argjson n "$1" --argjson u "$2" --arg t "$3" --arg pr "${4:-}" \
		'. + [{number: $n, state: "open", user: $u, title: $t, body: "", labels: [], comments: []}
		      | if $pr != "" then .pull_request = {} else . end]' \
		"$TMP/state/issues.json" >"$TMP/state/issues.next" && mv "$TMP/state/issues.next" "$TMP/state/issues.json"
}
field() { "$JQ" -r --argjson n "$1" ".[] | select(.number == \$n) | $2" "$TMP/state/issues.json"; }
# Never `producer | grep -q` under pipefail; read the whole text first.
has() { # has <text> <grep arguments...>
	local text="$1"
	shift
	grep -q "$@" <<<"$text"
}
RESULTS_RED="$(printf '%s\n' build=success gateway-http=failure mesh-http=success uds=failure waypoint=cancelled first-install=skipped)"
# report [VAR=value...]: run the step. RC is its exit status.
report() {
	: >"$TMP/log"
	env -u FAKE_FAIL -u FAKE_LABELS PATH="$TMP/bin:$PATH" GH_TOKEN=x GH_REPO=o/r \
		FAKE_LOG="$TMP/log" FAKE_STATE="$TMP/state" \
		RUN_URL=https://example.invalid/runs/42 RESULTS="$RESULTS_RED" "$@" \
		bash "$SCRIPT" >"$TMP/out" 2>&1
	RC=$?
}
writes() { grep -c '^WRITE' "$TMP/log"; }

# --- a failed night, nothing open ---------------------------------------------------
reset_state
report
if [ "$RC" -eq 0 ] && grep -qxF "WRITE issue create 7 [bug+ci]: ${TITLE}" "$TMP/log" && [ "$(writes)" -eq 1 ]; then
	pass "a failed night, nothing open: one issue, labelled bug and ci"
else
	fail "a failed night, nothing open (rc=$RC)"
fi
body="$(field 7 .body)"
if has "$body" -xF 'Failed jobs: `gateway-http` `uds` ' && has "$body" -xF 'Run: https://example.invalid/runs/42' &&
	! has "$body" -F 'waypoint' && ! has "$body" -F 'first-install'; then
	pass "the body names the jobs that failed (not the cancelled, not the skipped) and the run"
else
	fail "the body: $body"
fi

# --- the next failed night comments on it --------------------------------------------
report
if [ "$RC" -eq 0 ] && grep -qx 'WRITE issue comment 7' "$TMP/log" && [ "$(writes)" -eq 1 ] && grep -qx 'commented on #7' "$TMP/out"; then
	pass "the next failed night: a comment on the open issue, no second issue"
else
	fail "the next failed night (rc=$RC)"
fi

# --- no job says `failure` ---------------------------------------------------------
report RESULTS="build=cancelled"
if [ "$RC" -eq 0 ] && has "$(field 7 '.comments[-1].body')" -F 'Failed jobs: (no job reported failure — check the run)'; then
	pass "no job reported failure: says so instead of an empty list"
else
	fail "no job reported failure (rc=$RC)"
fi
# A job name is quoted in a code span: nothing that could close it gets through.
if [ "$(RESULTS='a`b c=failure' bash "$SCRIPT" body | sed -n 1p)" = 'Failed jobs: `abc` ' ]; then
	pass "a job name is reduced to plain characters before it is quoted"
else
	fail "a job name with a backtick: $(RESULTS='a`b c=failure' bash "$SCRIPT" body | sed -n 1p)"
fi

# --- whose issue (#1532) --------------------------------------------------------------
reset_state
seed 40 '{"login":"mallory","type":"User"}' "$TITLE"
seed 41 '{"login":"dependabot[bot]","type":"Bot"}' "$TITLE"
seed 42 "$BOT" "$TITLE" pr
seed 43 "$BOT" "Re: ${TITLE} (again)"
report
if [ "$RC" -eq 0 ] && grep -qxF "WRITE issue create 44 [bug+ci]: ${TITLE}" "$TMP/log" && [ "$(writes)" -eq 1 ]; then
	pass "a stranger's issue, another bot's, a pull request, a longer title: none is written to, the workflow opens its own"
else
	fail "an issue that is not the workflow's was used (rc=$RC)"
fi

# --- a label that is gone (#1568) -------------------------------------------------------
for mode in reject drop; do
	reset_state
	printf '%s\n' bug >"$TMP/state/labels"
	report FAKE_LABELS="$mode"
	if [ "$RC" -eq 1 ] && [ "$(field 7 .state)" = open ] && has "$(field 7 .body)" -F 'gateway-http' &&
		grep -q 'filed without its labels' "$TMP/out"; then
		pass "a label is gone (GitHub would ${mode} it): the issue is filed, and the step fails"
	else
		fail "a label is gone (${mode}): rc=$RC"
	fi
done

# --- the issue API does not answer ----------------------------------------------------
reset_state
report FAKE_FAIL=list
if [ "$RC" -eq 1 ] && [ "$(writes)" -eq 0 ]; then
	pass "the listing fails: the step fails, nothing is filed blind"
else
	fail "the listing fails: rc=$RC"
fi
report FAKE_FAIL=create
if [ "$RC" -eq 1 ]; then pass "the create fails: the step fails"; else fail "the create fails: rc=$RC"; fi
seed 7 "$BOT" "$TITLE"
report FAKE_FAIL=comment
if [ "$RC" -eq 1 ]; then pass "the comment fails: the step fails"; else fail "the comment fails: rc=$RC"; fi
report GH_REPO=
if [ "$RC" -eq 1 ] && [ ! -s "$TMP/log" ]; then
	pass "no GH_REPO: refused before any call"
else
	fail "no GH_REPO: rc=$RC"
fi

# --- the workflow ------------------------------------------------------------------------
job="$(awk '/^  report-failure:$/ { on = 1; next } on && /^  [A-Za-z0-9_-]+:$/ { on = 0 } on' "$WORKFLOW")"
if has "$job" -qE '^[[:space:]]+run: \./scripts/e2e-report-failure\.sh$'; then
	pass "the report-failure job runs the script"
else
	fail "the report-failure job does not run ./scripts/e2e-report-failure.sh"
fi
if has "$job" -E 'gh (issue (list|create|comment|close)|api) '; then
	fail "the report-failure job still calls gh itself"
else
	pass "the job has no issue logic of its own"
fi
# The job checks out the script and what it sources: a file missing from the
# sparse checkout is a step that cannot start.
missing=""
for f in scripts/e2e-report-failure.sh scripts/rolling-issue-lib.sh; do
	has "$job" -qE "^[[:space:]]+${f//./\\.}\$" || missing+=" $f"
done
if [ -z "$missing" ] && has "$job" -F 'persist-credentials: false'; then
	pass "the job's sparse checkout holds the script and its library, and leaves no token"
else
	fail "the job's checkout lacks:${missing:- persist-credentials: false}"
fi
sourced="$(sed -n 's|^\. "\$(cd -- "\$(dirname -- "\${BASH_SOURCE\[0\]}")" && pwd)/\(.*\)"$|\1|p' "$SCRIPT")"
if [ "$sourced" = rolling-issue-lib.sh ]; then
	pass "the script sources exactly the library the job checks out"
else
	fail "the script sources '${sourced}', and the job checks out rolling-issue-lib.sh"
fi
if has "$job" -qE '^[[:space:]]+issues: write$' && has "$job" -qF "if: failure() && github.event_name == 'schedule'"; then
	pass "the job may write issues, and runs only for a failed scheduled run"
else
	fail "the job's permission or condition changed"
fi

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS check(s) failed"
	exit 1
fi
echo "all checks passed"
