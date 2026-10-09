#!/usr/bin/env bash
# Hermetic test of scripts/third-party-images-report.sh (#1478): no network, no
# registry, no GitHub. A fake `outdated` prints a canned answer with a canned
# exit status, and a fake `gh` keeps the rolling issue in a directory and logs
# every call, so each case asserts what would have been written and what would
# not: a pin behind opens the issue, the same set rewrites it without a
# comment, a changed set comments, a registry error neither closes nor rewrites
# it and never fails the run, every pin current closes it, a dry run calls `gh`
# not at all, and a check that is itself broken exits 2 and reports nothing.
# Since #1570 an incomplete run does write one thing, the hidden count of failed
# lookups: where it lives (the open issue, the newest closed one, one opened
# and closed to hold it), that the third run in a row reports the pin and a
# complete run ends every streak. And the newer tags (#1569): a section that
# opens, closes and comments on nothing.
#
# The fake `gh` answers `issue list` with JSON and runs the script's own `--jq`
# filter over it (with jq: the Bazel-pinned one, or the one on PATH), so which
# issue counts as the rolling one is decided by the real filter: an issue a
# user opened under the same title, another bot's, and a longer title are not.
#
# Run: bazel test //scripts:third_party_images_report_test, or
# bash scripts/third_party_images_report_test.sh with jq on PATH.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/third-party-images-report.sh"

if [ -n "${JQ_RLOCATIONPATH:-}" ]; then
	JQ="${TEST_SRCDIR:-${RUNFILES_DIR:-$PWD/..}}/${JQ_RLOCATIONPATH}"
fi
JQ="${JQ:-$(command -v jq)}"
[ -x "$JQ" ] || {
	echo "FAIL: no jq (JQ=${JQ})"
	exit 1
}
export JQ

TMP="$(cd "$(mktemp -d)" && pwd -P)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin" "$TMP/state"

FAILS=0
CASES=0
ok() {
	CASES=$((CASES + 1))
	echo "ok   $*"
}
bad() {
	CASES=$((CASES + 1))
	FAILS=$((FAILS + 1))
	echo "FAIL $*"
	sed 's/^/     | /' "$TMP/out"
	sed 's/^/     log | /' "$TMP/log"
}

# The fake `outdated`: prints $FAKE_OUTDATED and exits $FAKE_OUTDATED_RC. The
# fake `newer` (#1569) prints $FAKE_NEWER and exits $FAKE_NEWER_RC; with no
# $FAKE_NEWER it fails like a command that is not there, which is what most
# cases below run with: a tag list that cannot be read must change nothing.
cat >"$TMP/bin/images" <<'EOF'
#!/usr/bin/env bash
if [ "$1" = newer ] && [ "$#" -eq 1 ] && [ -n "${FAKE_NEWER:-}" ]; then
	cat "$FAKE_NEWER"
	exit "${FAKE_NEWER_RC:-0}"
fi
[ "$1" = outdated ] && [ "$#" -eq 1 ] || { echo "fake images: unexpected $*" >&2; exit 64; }
cat "$FAKE_OUTDATED"
exit "$FAKE_OUTDATED_RC"
EOF

# The fake `gh`: the rolling issue lives in $FAKE_STATE (num, body, comments),
# every call is logged, every write is logged as WRITE. `issue list` answers
# with the rolling issue (the workflow's own) plus whatever $FAKE_STATE/others
# holds (one JSON issue per line), through the caller's --jq filter. A closed
# issue is kept too (#1570): `issue close` moves the number to
# $FAKE_STATE/closed, `issue list --state closed` answers with it and with
# $FAKE_STATE/others-closed, and `body` is the body of the newest issue, open or
# closed. The first issue is #7, the next #8.
cat >"$TMP/bin/gh" <<'EOF'
#!/usr/bin/env bash
echo "gh $*" >>"$FAKE_LOG"
case " ${FAKE_GH_FAIL:-} " in *" $1-$2 "*)
	echo "fake gh: $1 $2 fails" >&2
	exit 1
	;;
esac
case "$1 $2" in
"issue list")
	filter=""
	state=open
	while [ $# -gt 0 ]; do
		[ "$1" = "--jq" ] && filter="$2"
		[ "$1" = "--state" ] && state="$2"
		shift
	done
	case " ${FAKE_GH_FAIL:-} " in *" issue-list-$state "*)
		echo "fake gh: issue list --state $state fails" >&2
		exit 1
		;;
	esac
	numfile="$FAKE_STATE/num"
	others="$FAKE_STATE/others"
	if [ "$state" = closed ]; then
		numfile="$FAKE_STATE/closed"
		others="$FAKE_STATE/others-closed"
	fi
	{
		cat "$others" 2>/dev/null
		if [ -f "$numfile" ]; then
			printf '{"number":%s,"title":"%s","author":{"is_bot":true,"login":"app/github-actions"}}\n' \
				"$(cat "$numfile")" "$FAKE_TITLE"
		fi
	} | "$JQ" -s -r "$filter"
	;;
"issue create")
	n="$(cat "$FAKE_STATE/next" 2>/dev/null || echo 7)"
	echo "$n" >"$FAKE_STATE/num"
	echo "$((n + 1))" >"$FAKE_STATE/next"
	title=""
	labels=""
	while [ $# -gt 0 ]; do
		[ "$1" = "--title" ] && title="$2"
		[ "$1" = "--label" ] && labels="${labels:+$labels+}$2"
		[ "$1" = "--body-file" ] && cp "$2" "$FAKE_STATE/body"
		shift
	done
	echo "WRITE issue create [$labels]: $title" >>"$FAKE_LOG"
	echo "https://github.com/bpalermo/aether/issues/$n"
	;;
"issue view") cat "$FAKE_STATE/body" ;;
"issue comment")
	echo "WRITE issue comment $3" >>"$FAKE_LOG"
	while [ $# -gt 0 ]; do
		case "$1" in
		--body-file) cat "$2" >>"$FAKE_STATE/comments" ;;
		--body) printf '%s\n' "$2" >>"$FAKE_STATE/comments" ;;
		esac
		shift
	done
	;;
"issue edit")
	echo "WRITE issue edit $3" >>"$FAKE_LOG"
	while [ $# -gt 0 ]; do
		[ "$1" = "--body-file" ] && cp "$2" "$FAKE_STATE/body"
		shift
	done
	;;
"issue close")
	[ -f "$FAKE_STATE/num" ] && mv "$FAKE_STATE/num" "$FAKE_STATE/closed"
	echo "WRITE issue close $3" >>"$FAKE_LOG"
	;;
*)
	echo "fake gh: unexpected $*" >&2
	exit 1
	;;
esac
EOF
chmod +x "$TMP/bin/images" "$TMP/bin/gh"

D1="sha256:$(printf '1%.0s' {1..64})"
D2="sha256:$(printf '2%.0s' {1..64})"
D3="sha256:$(printf '3%.0s' {1..64})"

# report <rc> [--dry-run]: runs the script on the answer in $TMP/answer. Sets RC;
# the output is in $TMP/out, the gh calls in $TMP/log.
report() {
	local rc="$1"
	shift
	: >"$TMP/log"
	: >"$TMP/summary"
	PATH="$TMP/bin:$PATH" THIRD_PARTY_IMAGES="$TMP/bin/images" FAKE_OUTDATED="$TMP/answer" FAKE_OUTDATED_RC="$rc" \
		FAKE_NEWER="${FAKE_NEWER:-}" FAKE_NEWER_RC="${FAKE_NEWER_RC:-0}" \
		FAKE_LOG="$TMP/log" FAKE_STATE="$TMP/state" FAKE_GH_FAIL="${FAKE_GH_FAIL:-}" FAKE_TITLE="$TITLE" \
		GH_REPO=bpalermo/aether RUN_URL=https://example.invalid/run GITHUB_STEP_SUMMARY="$TMP/summary" \
		bash "$SCRIPT" "$@" >"$TMP/out" 2>&1
	RC=$?
}
answer() { cat >"$TMP/answer"; }
writes() { grep '^WRITE' "$TMP/log" | sed 's/^WRITE //' | paste -sd, -; }
# expect <name> <want rc> <want writes>: the exit status and exactly these writes.
expect() {
	local got
	got="$(writes)"
	if [ "$RC" -eq "$2" ] && [ "$got" = "$3" ]; then
		ok "$1"
	else
		bad "$1: exit $RC (want $2), writes '$got' (want '$3')"
	fi
}
has() { # <name> <file> <needle>...
	local name="$1" file="$2" needle
	shift 2
	for needle in "$@"; do
		grep -qF -- "$needle" "$file" || {
			bad "$name: $(basename "$file") lacks '$needle'"
			return
		}
	done
	ok "$name"
}
lacks() { # <name> <file> <needle>
	if grep -qF -- "$3" "$2"; then bad "$1: $(basename "$2") has '$3'"; else ok "$1"; fi
}

TITLE="CI: third-party image pins are behind their tags"
# What opening the issue looks like in the log: its labels, then its title.
CREATED="issue create [enhancement+ci]: $TITLE"
MOVED_AB="MOVED    a/b:1.0  pinned $D1, the tag now points at $D2"
NOT_MULTI="NOT-MULTI-ARCH quay.io/c/d:v2  $D2: the index does not list linux/arm64"

all_current() {
	answer <<EOF
current  a/b:1.0  $D1
current  quay.io/c/d:v2  $D2
2 checked: 0 behind, 0 could not be checked.
EOF
}
one_behind() {
	answer <<EOF
$MOVED_AB
current  quay.io/c/d:v2  $D2
2 checked: 1 behind, 0 could not be checked.
EOF
}

# --- every pin current, no issue: nothing is written ---------------------------
all_current
report 0
expect "every pin current and no open issue: nothing is written" 0 ""

# --- a dry run calls gh not at all ---------------------------------------------
one_behind
report 1 --dry-run
if [ "$RC" -eq 0 ] && [ ! -s "$TMP/log" ]; then ok "dry run: exit 0 and no gh call, not even a read"; else bad "dry run: exit $RC"; fi
has "dry run: prints the issue it would write" "$TMP/out" "DRY RUN: would open or update the issue \"$TITLE\"" "  | $MOVED_AB"
all_current
report 0 --dry-run
if [ "$RC" -eq 0 ] && [ ! -s "$TMP/log" ]; then ok "dry run, every pin current: no gh call"; else bad "dry run current: exit $RC"; fi
has "dry run, every pin current: says it would close" "$TMP/out" "DRY RUN: every pin is current; would close an open"

# --- a pin behind opens the issue ----------------------------------------------
one_behind
report 1
expect "a pin behind and no open issue: the issue is opened with its labels, the run stays green" 0 "$CREATED"
has "the issue lists the pin, the runbook section and the run" "$TMP/state/body" \
	"$MOVED_AB" "Refreshing third-party image pins" "https://example.invalid/run" "<!-- third-party-images: "
lacks "the issue does not list a pin that is current" "$TMP/state/body" "quay.io/c/d"
lacks "a complete run says nothing about unchecked pins" "$TMP/state/body" "could not be checked"
has "the run is annotated with the pin" "$TMP/out" "::warning title=third-party image pin behind::$MOVED_AB"
has "the step summary carries the whole answer" "$TMP/summary" "$MOVED_AB" "current  quay.io/c/d:v2" "2 checked: 1 behind"

# --- the same set again: rewritten, no comment ---------------------------------
report 1
expect "the same set of pins behind: the body is rewritten, no comment" 0 "issue edit 7"

# The tag moved once more under the same pin: the body follows, still no comment.
answer <<EOF
MOVED    a/b:1.0  pinned $D1, the tag now points at $D3
current  quay.io/c/d:v2  $D2
2 checked: 1 behind, 0 could not be checked.
EOF
report 1
expect "the tag moved again under a pin already listed: no comment" 0 "issue edit 7"
has "the rewritten body says where the tag points now" "$TMP/state/body" "the tag now points at $D3"

# --- the set changed: one comment, then the rewrite ----------------------------
answer <<EOF
$MOVED_AB
$NOT_MULTI
2 checked: 2 behind, 0 could not be checked.
EOF
report 1
expect "another pin fell behind: a comment, then the body is rewritten" 0 "issue comment 7,issue edit 7"
has "the comment lists the new set" "$TMP/state/comments" "$MOVED_AB" "$NOT_MULTI"

# A pin that is behind for another reason is another set.
answer <<EOF
$MOVED_AB
MOVED    quay.io/c/d:v2  pinned $D2, the tag now points at $D3
2 checked: 2 behind, 0 could not be checked.
EOF
report 1
expect "the same pin behind for another reason: a comment" 0 "issue comment 7,issue edit 7"
one_behind
report 1
expect "a pin caught up while another stays behind: a comment" 0 "issue comment 7,issue edit 7"

# --- a registry error: never red, never closes, never rewrites -----------------
# What an incomplete run writes to an open issue is the hidden count of failed
# lookups (#1570) and nothing a reader sees: the body outside that part is the
# one the last complete run wrote, byte for byte. (Before #1570 such a run
# wrote nothing at all, and these two cases expected no write.)
STATE_LINE="<!-- third-party-images-state: errors@"
visible() { sed -e '/^<!-- third-party-images-state: /,/^<!-- third-party-images-state-end -->$/d' "$1"; }
state_is() { # <name> <want: the state line after "errors@">
	local got
	got="$(grep -F -- "$STATE_LINE" "$TMP/state/body" | sed -e 's/^<!-- third-party-images-state: errors@//' -e 's/ -->$//' | paste -sd'|' -)"
	if [ "$got" = "$2" ]; then ok "$1"; else bad "$1: the hidden state is '$got', want '$2'"; fi
}
visible "$TMP/state/body" >"$TMP/body.before"
answer <<EOF
current  a/b:1.0  $D1
ERROR    quay.io/c/d:v2  no answer from quay.io
2 checked: 0 behind, 1 could not be checked.
EOF
report 2
expect "a registry error with nothing else behind: the open issue is not closed, only its hidden count is written" 0 "issue edit 7"
has "a registry error is a warning on the run" "$TMP/out" \
	"::warning title=third-party image pin not checked::ERROR    quay.io/c/d:v2  no answer from quay.io" "#7 left as it is"
state_is "the open issue counts the first failed lookup" "open quay.io/c/d:v2=1"
answer <<EOF
ERROR    a/b:1.0  https://registry-1.docker.io/v2/a/b/manifests/1.0 answered HTTP 429
$NOT_MULTI
2 checked: 1 behind, 1 could not be checked.
EOF
report 2
expect "a registry error and another pin behind: the open issue is not rewritten, only its hidden count is" 0 "issue edit 7"
if visible "$TMP/state/body" | cmp -s - "$TMP/body.before"; then ok "what the open issue says is untouched by an incomplete run"; else bad "the body changed on an incomplete run"; fi
state_is "a pin that answered again is forgotten, the one that failed now is counted from 1" "open a/b:1.0=1"
lacks "one failed lookup is not reported in the issue" "$TMP/state/body" "or more runs in a row"
report 2 --dry-run
if [ "$RC" -eq 0 ] && [ ! -s "$TMP/log" ]; then ok "dry run with a registry error: exit 0, no gh call"; else bad "dry run error: exit $RC"; fi

# --- every pin current: comment and close --------------------------------------
all_current
report 0
expect "every pin current: the open issue gets a comment and is closed" 0 "issue comment 7,issue close 7"
has "the closing comment names the run" "$TMP/state/comments" "Every pin is current again." "https://example.invalid/run"

# --- a registry error with no issue open ---------------------------------------
rm -rf "$TMP/state"
mkdir -p "$TMP/state"
answer <<EOF
current  a/b:1.0  $D1
ERROR    quay.io/c/d:v2  no answer from quay.io
2 checked: 0 behind, 1 could not be checked.
EOF
report 2
# No issue is left open and the run stays green. There has never been a rolling
# issue here, so one is opened and closed at once to hold the count (#1570;
# before it, nothing was written and the failed lookup was forgotten).
expect "only a registry error and no rolling issue yet: one is opened and closed to hold the count, the run stays green" 0 "$CREATED,issue close 7"
state_is "the closed issue holds the count" "closed quay.io/c/d:v2=1"
if [ ! -f "$TMP/state/num" ]; then ok "a single failed lookup leaves no issue open"; else bad "an issue is open after one failed lookup"; fi
answer <<EOF
$MOVED_AB
ERROR    quay.io/c/d:v2  no answer from quay.io
2 checked: 1 behind, 1 could not be checked.
EOF
report 2
expect "a pin behind next to a registry error, no open issue: the issue is opened with its labels" 0 "$CREATED"
has "that issue says which pin could not be checked" "$TMP/state/body" \
	"$MOVED_AB" "1 pin(s) could not be checked in this run" "ERROR    quay.io/c/d:v2  no answer from quay.io"

# --- a broken check is exit 2 and reports nothing ------------------------------
# (the issue from the case above is open: none of these may close or rewrite it)
broken() { # <name> <outdated rc> <needle>
	report "$2"
	expect "$1" 2 ""
	has "$1: says why" "$TMP/out" "::error::third-party-images-report: " "$3"
}
answer <<EOF
third-party-images: jq not found (JQ=jq)
EOF
broken "outdated died before its summary (exit 2, like a registry error)" 2 "without its summary line"
answer </dev/null
broken "outdated printed nothing and exited 0" 0 "without its summary line"
answer <<EOF
$MOVED_AB
2 checked: 0 behind, 0 could not be checked.
EOF
broken "a MOVED line the summary does not count" 0 "but printed 1 line(s) for a pin behind"
answer <<EOF
current  a/b:1.0  $D1
2 checked: 0 behind, 1 could not be checked.
EOF
broken "an error the summary counts and no line shows" 2 "and 0 ERROR line(s)"
one_behind
broken "a pin behind under exit 0" 0 "means exit 1"
all_current
broken "every pin current under exit 1" 1 "means exit 0"
all_current
broken "every pin current under an exit status outdated never uses" 127 "exited 127"
all_current
: >"$TMP/log"
PATH="$TMP/bin:$PATH" THIRD_PARTY_IMAGES="$TMP/bin/images" FAKE_OUTDATED="$TMP/answer" FAKE_OUTDATED_RC=0 \
	FAKE_LOG="$TMP/log" FAKE_STATE="$TMP/state" FAKE_TITLE="$TITLE" GH_REPO=bpalermo/aether bash "$SCRIPT" --bogus >"$TMP/out" 2>&1
RC=$?
expect "an unknown argument is refused before anything runs" 2 ""

# --- only the workflow's own issue is the rolling issue -------------------------
# The repository is public: anyone can open an issue under the title. Such an
# issue is never rewritten, commented on or closed; the report opens its own.
rm -rf "$TMP/state"
mkdir -p "$TMP/state"
cat >"$TMP/state/others" <<EOF
{"number":91,"title":"$TITLE","author":{"is_bot":false,"login":"mallory"}}
{"number":92,"title":"$TITLE","author":{"is_bot":true,"login":"app/some-other-app"}}
{"number":93,"title":"$TITLE","author":{"is_bot":false,"login":"app/github-actions"}}
{"number":94,"title":"$TITLE (again)","author":{"is_bot":true,"login":"app/github-actions"}}
EOF
all_current
report 0
expect "every pin current: an issue someone else opened under the title is not closed" 0 ""
one_behind
report 1
expect "a pin behind: someone else's issue under the title is not reused, the report opens its own" 0 "$CREATED"
report 1
expect "the next run rewrites the workflow's own issue and no other" 0 "issue edit 7"
all_current
report 0
expect "every pin current: the workflow's own issue is closed and no other" 0 "issue comment 7,issue close 7"
rm -f "$TMP/state/others"

# --- gh failing is never "nothing to report" -----------------------------------
all_current
FAKE_GH_FAIL="issue-list" report 0
expect "the issue list cannot be read: exit 2" 2 ""
one_behind
report 1 # opens the issue the next cases fail to read, rewrite and close
FAKE_GH_FAIL="issue-view" report 1
expect "the open issue cannot be read: exit 2, nothing written" 2 ""
FAKE_GH_FAIL="issue-edit" report 1
if [ "$RC" -eq 2 ]; then ok "the rewrite fails: exit 2"; else bad "edit fails: exit $RC"; fi
all_current
FAKE_GH_FAIL="issue-close" report 0
if [ "$RC" -eq 2 ]; then ok "the close fails: exit 2"; else bad "close fails: exit $RC"; fi
rm -rf "$TMP/state"
mkdir -p "$TMP/state"
one_behind
FAKE_GH_FAIL="issue-create" report 1
if [ "$RC" -eq 2 ]; then ok "the issue cannot be opened: exit 2"; else bad "create fails: exit $RC"; fi

# --- a lookup that fails on every run is reported after three (#1570) -----------
# One failed lookup is a warning; a pin whose registry never answers would stay
# a warning for good. The rolling issue counts the runs in a row, in a hidden
# line, and the third opens it.
fresh() {
	rm -rf "$TMP/state"
	mkdir -p "$TMP/state"
}
ERR_CD="ERROR    quay.io/c/d:v2  no answer from quay.io"
ERR_AB="ERROR    a/b:1.0  https://registry-1.docker.io/v2/a/b/manifests/1.0 answered HTTP 429"
cd_fails() {
	answer <<EOF
current  a/b:1.0  $D1
$ERR_CD
2 checked: 0 behind, 1 could not be checked.
EOF
}
both_fail() {
	answer <<EOF
$ERR_AB
$ERR_CD
2 checked: 0 behind, 2 could not be checked.
EOF
}
fresh
cd_fails
report 2
expect "streak, run 1: the count goes into an issue that is closed at once" 0 "$CREATED,issue close 7"
has "the issue that holds the count says what it is" "$TMP/state/body" "to hold" "3 in a row open a new issue"
report 2
expect "streak, run 2: the closed issue's count is edited, nothing is opened and nobody is told" 0 "issue edit 7"
state_is "streak, run 2: the count is 2" "closed quay.io/c/d:v2=2"
lacks "streak, run 2: still nothing a reader sees" "$TMP/state/body" "or more runs in a row"
report 2
expect "streak, run 3: the issue is opened for the pin that cannot be checked" 0 "$CREATED"
has "the issue names the pin, the streak and the registry's last answer" "$TMP/state/body" \
	"1 pin(s) could not be checked in 3 or more runs in a row" "quay.io/c/d:v2  3 runs in a row; last answer: no answer from quay.io" \
	"found no pinned third-party image behind its tag among the pins it could check" "https://example.invalid/run"
state_is "streak, run 3: the open issue holds the count" "open quay.io/c/d:v2=3"
if [ "$(cat "$TMP/state/num")" = 8 ]; then ok "the report is a new issue, the closed one is not reopened"; else bad "the open issue is #$(cat "$TMP/state/num" 2>/dev/null), want 8"; fi
report 2
expect "streak, run 4: the count moves on, no comment for a pin already reported" 0 "issue edit 8"
has "the section follows the count" "$TMP/state/body" "quay.io/c/d:v2  4 runs in a row"
# A second pin starts failing: counted, and reported with a comment at its third run.
both_fail
report 2
expect "a second pin fails once: counted, not reported, no comment" 0 "issue edit 8"
state_is "both pins are counted, each from its own first failure" "open a/b:1.0=1 quay.io/c/d:v2=5"
lacks "the second pin is not in the section before its third run" "$TMP/state/body" "a/b:1.0  "
report 2
: >"$TMP/state/comments"
report 2
expect "the second pin's third run: a comment, then the edit" 0 "issue comment 8,issue edit 8"
has "the comment names both pins that cannot be checked" "$TMP/state/comments" \
	"2 pin(s) could not be checked in 3 or more runs in a row" "a/b:1.0  3 runs in a row; last answer: https://registry-1.docker.io/v2/a/b/manifests/1.0 answered HTTP 429" "quay.io/c/d:v2  7 runs in a row"
# One of them answers again while the other still fails: an incomplete run
# never closes, the section drops the pin that answered.
answer <<EOF
$ERR_AB
current  quay.io/c/d:v2  $D2
2 checked: 0 behind, 1 could not be checked.
EOF
report 2
expect "a reported pin answers again, another still fails: the issue stays open" 0 "issue edit 8"
state_is "the pin that answered is forgotten" "open a/b:1.0=4"
lacks "the section no longer lists the pin that answered" "$TMP/state/body" "quay.io/c/d:v2  "
# Every pin answers: the report is over.
all_current
report 0
expect "every pin answers again: the issue is closed" 0 "issue comment 8,issue close 8"
# A count written while the issue was open is not continued once it is closed:
# the run that closed it checked every pin.
answer <<EOF
$ERR_AB
current  quay.io/c/d:v2  $D2
2 checked: 0 behind, 1 could not be checked.
EOF
report 2
expect "a failed lookup after the close: counted in the closed issue" 0 "issue edit 8"
state_is "the streak of the same pin starts again at 1, not at 5" "closed a/b:1.0=1"
report 2
state_is "and goes on in the closed issue" "closed a/b:1.0=2"
# A complete run in between ends the streak: err, err, ok, err is 1, not 3.
all_current
report 0
expect "a complete run with a count in the closed issue: the count is cleared, nothing else" 0 "issue edit 8"
state_is "the cleared count" "closed"
report 0
expect "a complete run with nothing to clear writes nothing" 0 ""
cd_fails
report 2
state_is "the streak after a complete run starts at 1" "closed quay.io/c/d:v2=1"
if [ ! -f "$TMP/state/num" ]; then ok "an interrupted streak opens no issue"; else bad "an issue is open after an interrupted streak"; fi

# The streak of a pin under an issue that is open because another pin is behind:
# the third run adds the section and a comment, and what the issue says is
# behind stays as the last complete run wrote it.
fresh
one_behind
report 1
visible "$TMP/state/body" >"$TMP/body.before"
answer <<EOF
$MOVED_AB
$ERR_CD
2 checked: 1 behind, 1 could not be checked.
EOF
report 2
report 2
expect "open for a pin behind, a lookup fails twice: only the hidden count is written" 0 "issue edit 7"
report 2
expect "open for a pin behind, the third failed lookup: a comment and the section" 0 "issue comment 7,issue edit 7"
has "the section is in the issue" "$TMP/state/body" "quay.io/c/d:v2  3 runs in a row" "$MOVED_AB"
if visible "$TMP/state/body" | cmp -s - "$TMP/body.before"; then ok "what the issue says is behind is untouched by the three incomplete runs"; else bad "the body outside the count changed"; fi
one_behind
report 1
expect "a complete run rewrites the issue" 0 "issue edit 7"
lacks "a complete run ends every streak: the section is gone" "$TMP/state/body" "or more runs in a row"
state_is "and so is the count" "open"

# A pin behind found by an incomplete run, with a count in a closed issue: the
# new issue carries the count on.
fresh
cd_fails
report 2
answer <<EOF
$MOVED_AB
$ERR_CD
2 checked: 1 behind, 1 could not be checked.
EOF
report 2
expect "a pin behind next to a failed lookup, count in a closed issue: a new issue is opened" 0 "$CREATED"
state_is "the new issue carries the count on" "open quay.io/c/d:v2=2"

# The count is read from the workflow's own issue only, and what is not a pin
# and a count is not believed (the body is text a maintainer can edit).
fresh
cat >"$TMP/state/others-closed" <<EOF
{"number":95,"title":"$TITLE","author":{"is_bot":false,"login":"mallory"}}
EOF
cd_fails
report 2
expect "a closed issue someone else opened under the title does not hold the count" 0 "$CREATED,issue close 7"
rm -f "$TMP/state/others-closed"
printf '%s\n' "text" "<!-- third-party-images-state: errors@closed quay.io/c/d:v2=2 \$(touch $TMP/pwned) x=y a/b:1.0=9999999 ;=1 -->" "<!-- third-party-images-state-end -->" "tail" >"$TMP/state/body"
both_fail
report 2
expect "a count line with junk in it: the run goes on" 0 "$CREATED"
state_is "only a pin and a count of at most six digits are read from it" "open a/b:1.0=1 quay.io/c/d:v2=3"
if [ ! -e "$TMP/pwned" ]; then ok "nothing from the body is run"; else bad "text from the issue body was executed"; fi

# gh failing while the count is read or written is exit 2.
fresh
cd_fails
report 2
FAKE_GH_FAIL="issue-list-closed" report 2
expect "the closed issues cannot be listed: exit 2, nothing written" 2 ""
FAKE_GH_FAIL="issue-view" report 2
expect "the closed issue cannot be read: exit 2, nothing written" 2 ""
FAKE_GH_FAIL="issue-edit" report 2
if [ "$RC" -eq 2 ]; then ok "the count cannot be written: exit 2"; else bad "count edit fails: exit $RC"; fi
fresh
FAKE_GH_FAIL="issue-close" report 2
if [ "$RC" -eq 2 ]; then ok "the issue that holds the count cannot be closed: exit 2"; else bad "holder close fails: exit $RC"; fi

# --- newer tags are a section, and decide nothing (#1569) -----------------------
NEWER_CD="NEWER    quay.io/c/d:v2  2 newer: v3 v4"
newer() { cat >"$TMP/newer"; }
newer <<EOF
$NEWER_CD
2 checked: 1 with newer tags, 0 could not be listed.
EOF
fresh
all_current
FAKE_NEWER="$TMP/newer" report 0
expect "a newer tag and every pin current: no issue is opened" 0 ""
has "the newer tag is in the step summary" "$TMP/summary" "$NEWER_CD"
FAKE_NEWER="$TMP/newer" report 0 --dry-run
has "dry run: the newer tags are printed as the section they would be" "$TMP/out" "  | $NEWER_CD" "DRY RUN: every pin is current"
one_behind
FAKE_NEWER="$TMP/newer" report 1
expect "a pin behind: the issue is opened" 0 "$CREATED"
has "the issue has the newer tags as a section of their own" "$TMP/state/body" \
	"1 pin(s) have newer tags of the same shape" "$NEWER_CD" "a newer tag never opens, changes or closes this issue"
newer <<EOF
$NEWER_CD
NEWER    a/b:1.0  1 newer: 1.1
2 checked: 2 with newer tags, 0 could not be listed.
EOF
FAKE_NEWER="$TMP/newer" report 1
expect "another newer tag under the same set of pins behind: rewritten, no comment" 0 "issue edit 7"
has "the section follows" "$TMP/state/body" "NEWER    a/b:1.0  1 newer: 1.1"
all_current
FAKE_NEWER="$TMP/newer" report 0
expect "every pin current while newer tags exist: the issue is closed all the same" 0 "issue comment 7,issue close 7"
# A tag list that cannot be read: said in the section, and nothing else changes.
newer <<EOF
$NEWER_CD
ERROR    a/b:1.0  tags: no answer from registry-1.docker.io
2 checked: 1 with newer tags, 1 could not be listed.
EOF
fresh
one_behind
FAKE_NEWER="$TMP/newer" FAKE_NEWER_RC=2 report 1
expect "a tag list that cannot be read: the issue is opened as ever, the run stays green" 0 "$CREATED"
has "the section says which list was not read" "$TMP/state/body" \
	"$NEWER_CD" "The tag list of 1 pin(s) was not read in this run" "ERROR    a/b:1.0  tags: no answer from registry-1.docker.io"
all_current
FAKE_NEWER="$TMP/newer" FAKE_NEWER_RC=2 report 0
expect "a tag list that cannot be read never keeps the issue open" 0 "issue comment 7,issue close 7"
# An answer that does not agree with itself is not believed, and not fatal.
unbelieved() { # <name> <newer rc>
	fresh
	one_behind
	FAKE_NEWER="$TMP/newer" FAKE_NEWER_RC="$2" report 1
	expect "$1: the issue is opened as ever" 0 "$CREATED"
	has "$1: the section says the lists were not read" "$TMP/state/body" "Newer tags were not listed in this run."
	lacks "$1: no line of it is repeated" "$TMP/state/body" "NEWER "
	has "$1: a warning on the run" "$TMP/out" "::warning title=third-party image newer tags not listed::"
}
newer <<EOF
$NEWER_CD
EOF
unbelieved "newer without its summary" 0
newer <<EOF
$NEWER_CD
2 checked: 0 with newer tags, 0 could not be listed.
EOF
unbelieved "a NEWER line the summary does not count" 0
newer <<EOF
$NEWER_CD
2 checked: 1 with newer tags, 0 could not be listed.
EOF
unbelieved "newer exits 2 with no list unread" 2
fresh
one_behind
report 1
has "newer not there at all: the section says so" "$TMP/state/body" "Newer tags were not listed in this run."

echo
echo "$CASES cases, $FAILS failed"
[ "$FAILS" -eq 0 ]
