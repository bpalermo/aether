#!/usr/bin/env bash
# Hermetic test of scripts/publish-verify-superseded.sh: which finished publish
# runs publish-verify skips as superseded, and — the half that matters more —
# which it must keep verifying (and so keep red when artifacts are missing).
# No network, no git: the shas are literals, MAIN_HEAD stands in for
# `git ls-remote`.
#
# Run: bazel test //scripts:publish_verify_superseded_test
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/publish-verify-superseded.sh"

old="b1dc1c04aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"  # the 2026-10-05 superseded publish
head="df3a1136b6e567fd56470a32dc6f04a26ce558cc" # main's head

FAILS=0
expect() { # expect <want> <commit> <conclusion> <head> <name>
	local got
	got="$(bash "$SCRIPT" decide "$2" "$3" "$4")"
	if [ "$got" = "$1" ]; then
		echo "PASS  $5 -> $got"
	else
		echo "FAIL  $5: got '$got', want '$1'"
		FAILS=$((FAILS + 1))
	fi
}

# Skipped: publish did not succeed AND main has moved on.
expect superseded "$old" cancelled "$head" "cancelled, no longer main's head (2026-10-05, b1dc1c04)"
expect superseded "$old" failure "$head" "failed, no longer main's head"
expect superseded "$old" timed_out "$head" "timed out, no longer main's head"

# Verified (a missing artifact stays red):
expect verify "$old" success "$head" "publish reported SUCCESS, superseded since: artifacts must exist"
expect verify "$head" success "$head" "success, still the head"
expect verify "$head" cancelled "$head" "cancelled but still main's head: nothing newer will publish it"
expect verify "$head" failure "$head" "failed and still main's head"
expect verify "$old" cancelled "" "main's head unknown: fail closed"
expect verify "$old" cancelled "not-a-sha" "main's head malformed: fail closed"
expect verify "" cancelled "$head" "no commit: fail closed"
expect verify "B1DC1C04AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" cancelled "$head" "uppercase sha: not a git sha, fail closed"
expect verify "${old:0:12}" cancelled "$head" "abbreviated sha: fail closed"
expect verify "$old" "" "$head" "no conclusion (dispatch/schedule): not a superseded publish"

# The workflow step: GITHUB_OUTPUT and the notice.
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
TARGET="$old" TRIGGERING_CONCLUSION=cancelled MAIN_HEAD="$head" TRIGGERING_RUN=https://example.invalid/run \
	GITHUB_OUTPUT="$tmp/out" GITHUB_STEP_SUMMARY="$tmp/summary" bash "$SCRIPT" >"$tmp/log"
if grep -qx 'skip=true' "$tmp/out" && grep -q '^::notice title=superseded commit, not verified::' "$tmp/log" &&
	grep -qF "$old" "$tmp/summary"; then
	echo "PASS  step: superseded -> skip=true, notice, summary"
else
	echo "FAIL  step: superseded"
	cat "$tmp/out" "$tmp/log"
	FAILS=$((FAILS + 1))
fi
: >"$tmp/out"
TARGET="$old" TRIGGERING_CONCLUSION=success MAIN_HEAD="$head" GITHUB_OUTPUT="$tmp/out" bash "$SCRIPT" >"$tmp/log"
if grep -qx 'skip=false' "$tmp/out" && ! grep -q '::notice' "$tmp/log"; then
	echo "PASS  step: success -> skip=false, no notice"
else
	echo "FAIL  step: success"
	cat "$tmp/out" "$tmp/log"
	FAILS=$((FAILS + 1))
fi

if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS failure(s)"
	exit 1
fi
echo "all passed"
