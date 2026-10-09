#!/usr/bin/env bash
# Hermetic test of scripts/third-party-images-report.sh (#1478): no network, no
# registry, no GitHub. A fake `outdated` prints a canned answer with a canned
# exit status, and a fake `gh` keeps the rolling issue in a directory and logs
# every call, so each case asserts what would have been written and what would
# not: a pin behind opens the issue, the same set rewrites it without a
# comment, a changed set comments, a registry error neither closes nor rewrites
# it and never fails the run, every pin current closes it, a dry run calls `gh`
# not at all, and a check that is itself broken exits 2 and reports nothing.
#
# Run: bazel test //scripts:third_party_images_report_test, or
# bash scripts/third_party_images_report_test.sh.
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/third-party-images-report.sh"

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

# The fake `outdated`: prints $FAKE_OUTDATED and exits $FAKE_OUTDATED_RC.
cat >"$TMP/bin/images" <<'EOF'
#!/usr/bin/env bash
[ "$1" = outdated ] && [ "$#" -eq 1 ] || { echo "fake images: unexpected $*" >&2; exit 64; }
cat "$FAKE_OUTDATED"
exit "$FAKE_OUTDATED_RC"
EOF

# The fake `gh`: the rolling issue lives in $FAKE_STATE (num, body, comments),
# every call is logged, every write is logged as WRITE.
cat >"$TMP/bin/gh" <<'EOF'
#!/usr/bin/env bash
echo "gh $*" >>"$FAKE_LOG"
case " ${FAKE_GH_FAIL:-} " in *" $1-$2 "*)
	echo "fake gh: $1 $2 fails" >&2
	exit 1
	;;
esac
case "$1 $2" in
"issue list") cat "$FAKE_STATE/num" 2>/dev/null || true ;;
"issue create")
	echo 7 >"$FAKE_STATE/num"
	title=""
	while [ $# -gt 0 ]; do
		[ "$1" = "--title" ] && title="$2"
		[ "$1" = "--body-file" ] && cp "$2" "$FAKE_STATE/body"
		shift
	done
	echo "WRITE issue create: $title" >>"$FAKE_LOG"
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
	rm -f "$FAKE_STATE/num"
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
		FAKE_LOG="$TMP/log" FAKE_STATE="$TMP/state" FAKE_GH_FAIL="${FAKE_GH_FAIL:-}" \
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
expect "a pin behind and no open issue: the issue is opened, the run stays green" 0 "issue create: $TITLE"
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
cp "$TMP/state/body" "$TMP/body.before"
answer <<EOF
current  a/b:1.0  $D1
ERROR    quay.io/c/d:v2  no answer from quay.io
2 checked: 0 behind, 1 could not be checked.
EOF
report 2
expect "a registry error with nothing else behind: the open issue is not closed" 0 ""
has "a registry error is a warning on the run" "$TMP/out" \
	"::warning title=third-party image pin not checked::ERROR    quay.io/c/d:v2  no answer from quay.io" "#7 left as it is"
answer <<EOF
ERROR    a/b:1.0  https://registry-1.docker.io/v2/a/b/manifests/1.0 answered HTTP 429
$NOT_MULTI
2 checked: 1 behind, 1 could not be checked.
EOF
report 2
expect "a registry error and another pin behind: the open issue is not rewritten" 0 ""
if cmp -s "$TMP/state/body" "$TMP/body.before"; then ok "the open issue's body is untouched by an incomplete run"; else bad "the body changed on an incomplete run"; fi
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
expect "only a registry error and no open issue: nothing is written, the run stays green" 0 ""
answer <<EOF
$MOVED_AB
ERROR    quay.io/c/d:v2  no answer from quay.io
2 checked: 1 behind, 1 could not be checked.
EOF
report 2
expect "a pin behind next to a registry error, no open issue: the issue is opened" 0 "issue create: $TITLE"
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
	FAKE_LOG="$TMP/log" FAKE_STATE="$TMP/state" GH_REPO=bpalermo/aether bash "$SCRIPT" --bogus >"$TMP/out" 2>&1
RC=$?
expect "an unknown argument is refused before anything runs" 2 ""

# --- gh failing is never "nothing to report" -----------------------------------
all_current
FAKE_GH_FAIL="issue-list" report 0
expect "the issue list cannot be read: exit 2" 2 ""
one_behind
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

echo
echo "$CASES cases, $FAILS failed"
[ "$FAILS" -eq 0 ]
