#!/usr/bin/env bash
# Fails if a promise of the external-harness contract left or changed relative
# to BASE without the contract's version being bumped (#1541).
#
# test/harnesscontract/external-harness.lock.yaml holds one line per promise of
# the contract's current version, and //test/harnesscontract:harnesscontract_test
# holds that file to the contract. That test reads both files from the same
# checkout, so it cannot see an entry removed together with its lock lines.
# This check can: it compares the lock with the one at BASE. Every promise line
# at BASE is still there, unchanged, or the version is higher than at BASE.
# Lines that are only added are new promises and need no bump.
#
# Usage: scripts/check-harness-contract-bump.sh [BASE_REF]
#   BASE_REF defaults to origin/main. CI passes the PR base SHA.
set -euo pipefail

BASE="${1:-origin/main}"
LOCK="test/harnesscontract/external-harness.lock.yaml"

if [ ! -f "$LOCK" ]; then
	echo "ERROR: $LOCK is missing: the contract's version bump is checked against it."
	exit 1
fi

# The lock at BASE. A base that has none (the lock is newer than it) has no
# promise to compare.
if ! base_lock="$(git show "$BASE:$LOCK" 2>/dev/null)"; then
	echo "OK: $LOCK does not exist at $BASE, so no promise is compared."
	exit 0
fi

version() { sed -nE 's/^version:[[:space:]]*([0-9]+)[[:space:]]*$/\1/p'; }
# A promise line: two spaces, a quoted name, a quoted digest.
promises() { grep -E '^  "[^"]+": "[^"]+"[[:space:]]*$' | sed -E 's/[[:space:]]+$//' | LC_ALL=C sort; }

base_version="$(printf '%s\n' "$base_lock" | version)"
head_version="$(version <"$LOCK")"
if [ -z "$base_version" ] || [ -z "$head_version" ]; then
	echo "ERROR: no 'version: N' line in $LOCK (at $BASE: '${base_version}', here: '${head_version}')."
	exit 1
fi

if [ "$head_version" -lt "$base_version" ]; then
	echo "ERROR: the contract's version went back: $base_version at $BASE, $head_version here."
	exit 1
fi

# The lines of BASE that are not here as they were.
broken="$(comm -23 <(printf '%s\n' "$base_lock" | promises) <(promises <"$LOCK") || true)"
if [ -z "$broken" ]; then
	echo "OK: every promise of $LOCK at $BASE is still made (version $base_version -> $head_version)."
	exit 0
fi

count="$(printf '%s\n' "$broken" | wc -l | tr -d ' ')"
if [ "$head_version" -gt "$base_version" ]; then
	echo "OK: $count promise(s) left or changed, and the contract's version was bumped: $base_version -> $head_version."
	exit 0
fi

echo "ERROR: $count promise(s) of the external-harness contract left or changed and its version is still $head_version:"
printf '%s\n' "$broken" | sed -E 's/^  ("[^"]+"): .*$/         \1/'
echo
echo "A harness outside this repository written against version $head_version relies on each of them."
echo "Bump \`version\` in test/harnesscontract/external-harness.yaml and replace $LOCK with the one"
echo "//test/harnesscontract:harnesscontract_test then prints. See test/harnesscontract/README.md."
exit 1
