#!/usr/bin/env bash
# Hermetic test of the sweep's superseded-head selection (#1282):
# select_unsuperseded() and publish_conclusion() in scripts/push-heads-lib.sh,
# which apply scripts/publish-verify-superseded.sh's rule (#1277) to every push
# head the `--recent` sweep would check. Pins which heads it skips and — the half
# that matters more — which it must keep checking (and so keep red when their
# artifacts are missing). No network, no git: the shas and the publish-run list
# are canned (the shape of `gh api .../publish.yaml/runs`, newest first).
#
# Run: bazel test //scripts:push_heads_superseded_test
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091 # a runfile next to this test, not a path shellcheck can follow
. "$HERE/push-heads-lib.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# main on 2026-10-05 after the sweep in run 37384419419 went red: eight
# consecutive publishes ended `cancelled` (each replaced while pending by the
# next merge) between two successful ones, and main had moved on to 9b849d68.
main="9b849d687051ca21301b704ee68a086690001a87"
df3="df3a1136b6e567fd56470a32dc6f04a26ce558cc"  # success
s28b="28b2680aad88c83bc36858165f3f51b642303b29" # cancelled
cf4="cf47d9ab9ec7cbf006770bff2ec44323423a2377"  # cancelled
b1d="b1dc1c04ae610c71f085da20def6a291ba36e9dc"  # cancelled
s65e="65e473a428a8f5c5b6b5811c802d81012108d704" # success
# Stand-ins for the remaining shapes.
failed="f00dfa11aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"  # publish failed
rerun="5e5e5e5eaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"   # cancelled, then re-run green
running="7a7a7a7aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" # no conclusion yet
norun="0000aaaabbbbccccddddeeeeffff000011112222"   # no publish run on record

cat >"$tmp/runs" <<EOF
$main success
$df3 success
$s28b cancelled
$cf4 cancelled
$b1d cancelled
$s65e success
$failed failure
$rerun success
$rerun cancelled
$running
EOF

FAILS=0
N=0
expect() { # expect <name> <want> <got>
	N=$((N + 1))
	if [ "$3" = "$2" ]; then
		echo "PASS  $1"
	else
		printf 'FAIL  %s\n  want: %s\n  got:  %s\n' "$1" "$(printf '%s' "$2" | tr '\n' '|')" "$(printf '%s' "$3" | tr '\n' '|')"
		FAILS=$((FAILS + 1))
	fi
}

# publish_conclusion: what each head's publish ended as.
expect "conclusion: success" success "$(publish_conclusion "$tmp/runs" "$df3")"
expect "conclusion: cancelled" cancelled "$(publish_conclusion "$tmp/runs" "$b1d")"
expect "conclusion: a green re-run wins over the cancelled attempt" success "$(publish_conclusion "$tmp/runs" "$rerun")"
expect "conclusion: still running is empty" "" "$(publish_conclusion "$tmp/runs" "$running")"
expect "conclusion: no run on record is empty" "" "$(publish_conclusion "$tmp/runs" "$norun")"
expect "conclusion: a prefix is not a match" "" "$(publish_conclusion "$tmp/runs" "${b1d:0:12}")"

# 1. The 2026-10-05 window, newest first as `git log` gives it.
got="$(printf '%s\n' "$df3" "$s28b" "$cf4" "$b1d" "$s65e" | select_unsuperseded "$tmp/runs" "$main")"
want="check $df3
superseded $s28b cancelled
superseded $cf4 cancelled
superseded $b1d cancelled
check $s65e"
expect "2026-10-05: the cancelled heads are superseded, the published ones checked" "$want" "$got"

# 2. Kept (a missing artifact stays red):
got="$(printf '%s\n' "$failed" | select_unsuperseded "$tmp/runs" "$main")"
expect "failed publish, main moved on: superseded" "superseded $failed failure" "$got"
got="$(printf '%s\n' "$b1d" | select_unsuperseded "$tmp/runs" "$b1d")"
expect "cancelled but still main's head: checked (nothing newer will publish it)" "check $b1d" "$got"
got="$(printf '%s\n' "$rerun" | select_unsuperseded "$tmp/runs" "$main")"
expect "re-run to success: checked" "check $rerun" "$got"
got="$(printf '%s\n' "$running" | select_unsuperseded "$tmp/runs" "$main")"
expect "no conclusion yet: checked" "check $running" "$got"
got="$(printf '%s\n' "$norun" | select_unsuperseded "$tmp/runs" "$main")"
expect "no publish run on record (#880: a run that never existed): checked" "check $norun" "$got"

# 3. Fail closed.
got="$(printf '%s\n' "$s28b" "$cf4" | select_unsuperseded "$tmp/runs" "")"
expect "main's head unknown: every head checked" "check $s28b
check $cf4" "$got"
got="$(printf '%s\n' "$s28b" | select_unsuperseded "$tmp/runs" "not-a-sha")"
expect "main's head malformed: checked" "check $s28b" "$got"
: >"$tmp/empty"
got="$(printf '%s\n' "$s28b" "$cf4" | select_unsuperseded "$tmp/empty" "$main")"
expect "unreadable (empty) run list: every head checked" "check $s28b
check $cf4" "$got"

# 4. Shape: one line per candidate, input order, blank lines ignored.
got="$(printf '%s\n' "$s65e" "" "$s28b" | select_unsuperseded "$tmp/runs" "$main")"
expect "one line per candidate, in order" "check $s65e
superseded $s28b cancelled" "$got"

# PUBLISH_RUNS_FILE stands in for the API, verbatim.
got="$(PUBLISH_RUNS_FILE="$tmp/runs" github_publish_conclusions | sed -n 3p)"
expect "PUBLISH_RUNS_FILE is read instead of the API" "$s28b cancelled" "$got"

if [ "$N" -ne 17 ]; then
	echo "ran ${N} cases, expected 17 -- a test that checks nothing passes"
	exit 2
fi
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS failure(s)"
	exit 1
fi
echo "all ${N} passed"
