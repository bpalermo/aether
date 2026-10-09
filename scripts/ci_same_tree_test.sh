#!/usr/bin/env bash
# Hermetic test of the two scripts that keep ci.yaml's `diff` job and its test
# legs on ONE tree (#1459), against throwaway git repositories. No Bazel, no
# network; needs a real `git` on PATH.
#
#   scripts/ci-merge-range.sh    the range `diff` hands to bazel-diff: the
#                                checkout must be the run's commit and the merge
#                                of the pull request's head; the range is its
#                                first parent to the merge itself. So a target
#                                that exists only on the base side of the merge
#                                is in the head hashes, and one the base removed
#                                is not.
#   scripts/ci-impacted-lists.sh what a job runs right after it downloads the
#                                lists: all of them are there, they were
#                                computed for the commit this job checked out,
#                                and they agree with the has_* outputs. A
#                                missing list is a failure, never an empty one
#                                (#1460).
#
# Run: bazel test //scripts:ci_same_tree_test, or bash scripts/ci_same_tree_test.sh
set -uo pipefail
export LC_ALL=C

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
RANGE="$HERE/ci-merge-range.sh"
LISTS="$HERE/ci-impacted-lists.sh"
for f in "$RANGE" "$LISTS"; do
	[ -f "$f" ] || {
		echo "FAIL: $f not found"
		exit 1
	}
done
command -v git >/dev/null || {
	echo "FAIL: git is not on PATH"
	exit 1
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FAILS=0
pass() { echo "PASS  $*"; }
fail() {
	echo "FAIL  $*"
	FAILS=$((FAILS + 1))
}
expect() { # what, got, want
	if [ "$2" = "$3" ]; then
		pass "$1"
	else
		fail "$1"
		echo "    got:  $2"
		echo "    want: $3"
	fi
}

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid
export GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid

# --- the repository: a pull request that is behind its base -----------------------
#
#   root --- main1 --- main2 (base tip)
#      \                    \
#       pr1 --- pr2 (head) --- merge   <- what a pull_request run checks out
REPO="$TMP/repo"
mkdir -p "$REPO"
(
	cd "$REPO" || exit 1
	git init -q -b main . && echo root >f && git add f && git commit -q -m root &&
		git checkout -q -b pr && echo pr1 >p && git add p && git commit -q -m pr1 &&
		echo pr2 >p && git commit -q -am pr2 &&
		git checkout -q main && echo main1 >m && git add m && git commit -q -m main1 &&
		echo main2 >m && git commit -q -am main2 &&
		git checkout -q --detach main && git merge -q --no-ff -m merge pr
) || {
	echo "FAIL: could not build the throwaway repository"
	exit 1
}
MERGE="$(git -C "$REPO" rev-parse HEAD)"
BASE_TIP="$(git -C "$REPO" rev-parse main)"
PR_HEAD="$(git -C "$REPO" rev-parse pr)"

RC=0
# range <env...>: runs ci-merge-range.sh in the repository as checked out.
range() {
	: >"$TMP/gh_output"
	(cd "$REPO" && env GITHUB_OUTPUT="$TMP/gh_output" "$@" bash "$RANGE") >"$TMP/log" 2>&1
	RC=$?
}
outputs() { tr '\n' ' ' <"$TMP/gh_output" | sed 's/ $//'; }
refused() { # what, piece of the error
	if [ "$RC" -ne 0 ] && grep -F -- '::error::' "$TMP/log" | grep -qF -- "$2" && [ ! -s "$TMP/gh_output" ]; then
		pass "$1"
	else
		fail "$1: expected a failure saying '$2' and no output, got exit $RC and '$(outputs)'"
		sed 's/^/    /' "$TMP/log"
	fi
}

# --- 1. ci-merge-range.sh ----------------------------------------------------------
range EXPECT_SHA="$MERGE" PR_HEAD_SHA="$PR_HEAD"
expect "merge of a head that is behind its base: exit 0" "$RC" 0
expect "the range is the merge's first parent (the base tip) to the merge itself, not to the head" \
	"$(outputs)" "base=$BASE_TIP head=$MERGE"

# The checkout is the pull request's head, as `diff` used to ask for: refused.
git -C "$REPO" checkout -q --detach "$PR_HEAD"
range EXPECT_SHA="$MERGE" PR_HEAD_SHA="$PR_HEAD"
refused "the checkout is the head, not the run's commit" "the checkout is at $PR_HEAD and this run is for $MERGE"
git -C "$REPO" checkout -q --detach "$MERGE"

range PR_HEAD_SHA="$PR_HEAD"
refused "EXPECT_SHA is not set" 'EXPECT_SHA is not a full commit id'
range EXPECT_SHA="${MERGE:0:12}" PR_HEAD_SHA="$PR_HEAD"
refused "EXPECT_SHA is abbreviated" 'EXPECT_SHA is not a full commit id'
range EXPECT_SHA="$MERGE"
refused "PR_HEAD_SHA is not set" 'PR_HEAD_SHA is not a full commit id'
# The merge is of another head than the event's: the tree is not this pull request.
range EXPECT_SHA="$MERGE" PR_HEAD_SHA="$BASE_TIP"
refused "the merge's second parent is not the pull request's head" "is the merge of $PR_HEAD, and the pull request's head is $BASE_TIP"

# Not a merge at all (one parent): no first-parent range to trust, so the base
# is left empty and ci-impacted-targets.sh takes its full run.
git -C "$REPO" checkout -q --detach "$PR_HEAD"
range EXPECT_SHA="$PR_HEAD" PR_HEAD_SHA="$PR_HEAD"
expect "a commit that is not a merge: exit 0, an empty base (full run), head is the checkout" \
	"$RC $(outputs)" "0 base= head=$PR_HEAD"
if grep -q '^::warning::' "$TMP/log"; then
	pass "a commit that is not a merge: says so with ::warning::"
else
	fail "a commit that is not a merge: no ::warning:: line"
fi
git -C "$REPO" checkout -q --detach "$MERGE"

# Outside a repository nothing is resolved and nothing is written.
: >"$TMP/gh_output"
(cd "$TMP" && env GIT_CEILING_DIRECTORIES="$TMP" GITHUB_OUTPUT="$TMP/gh_output" EXPECT_SHA="$MERGE" PR_HEAD_SHA="$PR_HEAD" bash "$RANGE") >"$TMP/log" 2>&1
RC=$?
refused "not a git checkout" 'git rev-parse HEAD failed'

# --- 2. ci-impacted-lists.sh ---------------------------------------------------------
DIR="$TMP/impacted"
# fill: the artifact of a run that impacts one unit test and no integration test.
fill() {
	rm -rf "$DIR"
	mkdir -p "$DIR"
	printf '%s\n' //pkg:lib //pkg:unit_test >"$DIR/impacted_build.txt"
	echo //pkg:unit_test >"$DIR/impacted_unit.txt"
	: >"$DIR/impacted_integration.txt"
	echo "$MERGE" >"$DIR/impacted_commit.txt"
}
# lists <env...>: runs ci-impacted-lists.sh on $DIR from the repository.
lists() {
	(cd "$REPO" && env "$@" bash "$LISTS" "$DIR") >"$TMP/log" 2>&1
	RC=$?
}
OK=(HAS_ANY=true HAS_UNIT=true HAS_INTEGRATION=false)
bad() { # what, piece of the error
	if [ "$RC" -ne 0 ] && grep -F -- '::error::' "$TMP/log" | grep -qF -- "$2"; then
		pass "$1"
	else
		fail "$1: expected a failure saying '$2', got exit $RC"
		sed 's/^/    /' "$TMP/log"
	fi
}

fill
lists "${OK[@]}"
expect "lists computed for this checkout, agreeing with the outputs: exit 0" "$RC" 0

# Nothing impacted is a state of its own: three empty lists and three `false`.
fill
: >"$DIR/impacted_build.txt"
: >"$DIR/impacted_unit.txt"
lists HAS_ANY=false HAS_UNIT=false HAS_INTEGRATION=false
expect "three empty lists and three false: exit 0" "$RC" 0

for f in impacted_build.txt impacted_unit.txt impacted_integration.txt impacted_commit.txt; do
	fill
	rm "$DIR/$f"
	lists "${OK[@]}"
	bad "$f is missing" "$f is missing"
done
# A missing list with has_*=false is still missing: it is not the empty list.
fill
rm "$DIR/impacted_integration.txt"
lists "${OK[@]}"
bad "a missing list is not an empty one, whatever the output says" 'impacted_integration.txt is missing'
fill
rm -rf "$DIR"
lists "${OK[@]}"
bad "the directory is missing" 'impacted_build.txt is missing'

# The lists are for another commit than the one checked out: the #1459 state.
fill
echo "$PR_HEAD" >"$DIR/impacted_commit.txt"
lists "${OK[@]}"
bad "lists computed at the head, checkout at the merge" "computed for $PR_HEAD and this job checked out $MERGE"
fill
echo "${MERGE:0:12}" >"$DIR/impacted_commit.txt"
lists "${OK[@]}"
bad "the commit in the artifact is abbreviated" 'impacted_commit.txt does not hold one full commit id'
fill
: >"$DIR/impacted_commit.txt"
lists "${OK[@]}"
bad "the commit in the artifact is empty" 'impacted_commit.txt does not hold one full commit id'

# The outputs and the lists disagree, either way round.
fill
lists HAS_ANY=true HAS_UNIT=false HAS_INTEGRATION=false
bad "has_unit=false and a unit list with a target" 'has_unit is false and impacted_unit.txt has 1 target'
fill
lists HAS_ANY=true HAS_UNIT=true HAS_INTEGRATION=true
bad "has_integration=true and an empty integration list" 'has_integration is true and impacted_integration.txt has 0 target'
fill
lists HAS_ANY=false HAS_UNIT=true HAS_INTEGRATION=false
bad "has_any=false and a build list with targets" 'has_any is false and impacted_build.txt has 2 target'
# A list of blank lines is an empty list.
fill
printf '\n  \n' >"$DIR/impacted_unit.txt"
lists "${OK[@]}"
bad "has_unit=true and a unit list of blank lines" 'has_unit is true and impacted_unit.txt has 0 target'
# An output that is not a decision.
fill
lists HAS_ANY=true HAS_UNIT= HAS_INTEGRATION=false
bad "has_unit is the empty string" 'has_unit is "", not "true" or "false"'
fill
lists HAS_ANY=true HAS_INTEGRATION=false
bad "has_unit is not set" 'has_unit is "", not "true" or "false"'
fill
lists HAS_ANY=TRUE HAS_UNIT=true HAS_INTEGRATION=false
bad "has_any is TRUE" 'has_any is "TRUE", not "true" or "false"'

fill
(cd "$REPO" && env "${OK[@]}" bash "$LISTS") >"$TMP/log" 2>&1
RC=$?
bad "no directory argument" 'usage: ci-impacted-lists.sh <directory>'

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS check(s) failed"
	exit 1
fi
echo "all checks passed"
