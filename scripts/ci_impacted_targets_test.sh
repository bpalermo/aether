#!/usr/bin/env bash
# Test of scripts/ci-impacted-targets.sh against a throwaway git repository, a
# fake `bazel` and a fake `java` (bazel-diff): no Bazel, no JVM, no network.
#
# The fake `bazel query` answers from a table of label, kind and tags, and it
# applies the regex the script passes in `attr("tags", "<regex>", ...)` to the
# tag list rendered the way Bazel renders it (`[a, b, c]`). So the cases below
# exercise the script's own expression, not a canned answer per query.
#
# What it pins (#1413, #1437, #1438, #1439):
#   - a test tagged `manual` is in neither impacted_unit.txt nor
#     impacted_integration.txt, on the bazel-diff path and on every full-run
#     fallback: those lists reach `bazel test` as explicit labels, where Bazel
#     no longer honours the tag;
#   - the tag is matched whole: `no-manual-x`, `manually` and `manual-ish` stay,
#     and `manual` is found first, in the middle and last in a list;
#   - has_e2e still follows //test/e2e:e2e_test, which is `manual` itself;
#   - #1438: a rule tagged `manual` is not in impacted_build.txt either, except
#     the e2e target when it is impacted (its leg needs the `test` leg to run);
#   - #1439: `integration` is matched whole too: `no-integration`,
#     `integration-slow` and `integrationx` are unit tests;
#   - #1437: a `bazel query` that exits non-zero, or exits 0 and prints nothing,
#     never becomes an empty list with has_*=false. The rule query falls back
#     to the full run (which does not need it); a test query takes the fallback
#     too, and when it fails there as well the script exits non-zero and sets
#     no has_* output at all. Same for an integration query that takes every
#     test, for a bazel-diff that exits 0 without writing its list, and for a
#     checkout that cannot be put back;
#   - #1459: the lists are for the commit that is checked out. A HEAD_SHA that
#     is another commit fails the step, and impacted_commit.txt names the
#     commit on every path that writes the lists;
#   - anti-vacuity: with a filter taken back out of a copy of the script, the
#     same fixture puts the `manual` targets and the look-alike tags in the lists.
#
# Run: bazel test //scripts:ci_impacted_targets_test, or
#      bash scripts/ci_impacted_targets_test.sh
set -uo pipefail
export LC_ALL=C # the expected lists below are in byte order

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/ci-impacted-targets.sh"
[ -f "$SCRIPT" ] || {
	echo "FAIL: $SCRIPT not found"
	exit 1
}
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

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid
export GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid

# --- the target table: label, kind, tags as Bazel prints them -------------------
TARGETS="$TMP/targets.tsv"
{
	printf '%s\t%s\t%s\n' //pkg:lib go_library '[]'
	printf '%s\t%s\t%s\n' //pkg:unit_test go_test '[]'
	printf '%s\t%s\t%s\n' //pkg:root_test go_test '[requires-root]'
	printf '%s\t%s\t%s\n' //pkg:docker_test go_test '[integration]'
	printf '%s\t%s\t%s\n' //pkg:etcd_test go_test '[integration, no-remote-exec]'
	printf '%s\t%s\t%s\n' //pkg:no_integration_test go_test '[no-integration]'
	printf '%s\t%s\t%s\n' //pkg:integration_slow_test go_test '[integration-slow, requires-root]'
	printf '%s\t%s\t%s\n' //pkg:integrationx_test sh_test '[small, integrationx]'
	printf '%s\t%s\t%s\n' //pkg:no_manual_x_test go_test '[no-manual-x]'
	printf '%s\t%s\t%s\n' //pkg:manually_test go_test '[manually, integration]'
	printf '%s\t%s\t%s\n' //pkg:manual_ish_test sh_test '[small, manual-ish]'
	printf '%s\t%s\t%s\n' //charts/x:fails_by_design_test helm_template_test '[manual]'
	printf '%s\t%s\t%s\n' //pkg:manual_first_test go_test '[manual, integration]'
	printf '%s\t%s\t%s\n' //pkg:manual_middle_test go_test '[integration, manual, requires-root]'
	printf '%s\t%s\t%s\n' //pkg:manual_last_test sh_test '[small, manual]'
	printf '%s\t%s\t%s\n' //test/e2e:e2e_test go_test '[e2e, manual]'
	printf '%s\t%s\t%s\n' //e2e/img:push image_push '[manual]'
} >"$TARGETS"

# --- fakes ----------------------------------------------------------------------
BIN="$TMP/bin"
mkdir -p "$BIN"
cat >"$BIN/bazel" <<'EOF'
#!/usr/bin/env bash
# Fake bazel: `query <expr>` only, over $FAKE_TARGETS.
set -uo pipefail
[ "${1:-}" = query ] || {
	echo "fake bazel: unexpected command: $*" >&2
	exit 3
}
q="$2"
echo "$q" >>"$FAKE_LOG"
# Failure injection (#1437). Each is a glob over the query expression:
#   FAKE_QUERY_FAIL       exit 7 with an error on stderr, every time
#   FAKE_QUERY_FAIL_ONCE  the same, the first time only (a transient failure)
#   FAKE_QUERY_EMPTY      exit 0 and print nothing
#   FAKE_QUERY_PARTIAL    print the answer, then exit 3 (what `bazel query` does
#                         when part of the graph did not load)
# shellcheck disable=SC2053 # the right-hand sides are globs on purpose
if [ -n "${FAKE_QUERY_FAIL:-}" ] && [[ "$q" == $FAKE_QUERY_FAIL ]]; then
	echo "fake bazel: ERROR: query failed: $q" >&2
	exit 7
fi
# shellcheck disable=SC2053
if [ -n "${FAKE_QUERY_FAIL_ONCE:-}" ] && [[ "$q" == $FAKE_QUERY_FAIL_ONCE ]] && [ ! -e "$FAKE_ONCE" ]; then
	: >"$FAKE_ONCE"
	echo "fake bazel: ERROR: query failed once: $q" >&2
	exit 7
fi
# shellcheck disable=SC2053
if [ -n "${FAKE_QUERY_EMPTY:-}" ] && [[ "$q" == $FAKE_QUERY_EMPTY ]]; then
	exit 0
fi
rows="$(cat "$FAKE_TARGETS")"
case "$q" in
*'kind(rule, //...)'*) ;;
*'tests(//...)'*) rows="$(awk -F'\t' '$2 ~ /_test$/' <<<"$rows")" ;;
*)
	echo "fake bazel: unknown query: $q" >&2
	exit 3
	;;
esac
# `except attr("tags", "<regex>", //...)`: drop the rows whose tag list matches.
if [[ "$q" == *'except attr("tags", "'* ]]; then
	re="${q#*except attr(\"tags\", \"}"
	re="${re%%\", //...)*}"
	rows="$(RE="$re" awk -F'\t' '$3 !~ ENVIRON["RE"]' <<<"$rows")"
fi
# A leading `attr(tags, "<regex>", <expr>)`, `tags` quoted or not: keep the rows
# whose tag list matches the regex.
if [[ "$q" == 'attr(tags, "'* || "$q" == 'attr("tags", "'* ]]; then
	re="${q#attr(tags, \"}"
	re="${re#attr(\"tags\", \"}"
	re="${re%%\", *}"
	rows="$(RE="$re" awk -F'\t' '$3 ~ ENVIRON["RE"]' <<<"$rows")"
fi
[ -n "$rows" ] && cut -f1 <<<"$rows"
# shellcheck disable=SC2053
if [ -n "${FAKE_QUERY_PARTIAL:-}" ] && [[ "$q" == $FAKE_QUERY_PARTIAL ]]; then
	echo "fake bazel: ERROR: query finished with errors: $q" >&2
	exit 3
fi
exit 0
EOF
cp "$BIN/bazel" "$BIN/bazelisk" # the script prefers bazelisk when it is on PATH
cat >"$BIN/java" <<'EOF'
#!/usr/bin/env bash
# Fake bazel-diff: `-jar <jar> generate-hashes ... <out>` writes <out>;
# `-jar <jar> get-impacted-targets ... -o <out>` copies $FAKE_IMPACTED there.
set -uo pipefail
# FAKE_JAVA_FAIL=1       every call fails
# FAKE_JAVA_NO_OUTPUT=1  get-impacted-targets exits 0 without writing its list
# FAKE_JAVA_DIRTY=1      generate-hashes rewrites the tracked file `f` and fails,
#                        so the checkout cannot be moved off the base revision
[ "${FAKE_JAVA_FAIL:-}" = 1 ] && exit 1
case "$3" in
generate-hashes)
	if [ "${FAKE_JAVA_DIRTY:-}" = 1 ]; then
		echo dirty >f
		exit 1
	fi
	echo '{}' >"${*: -1}"
	;;
get-impacted-targets)
	[ "${FAKE_JAVA_NO_OUTPUT:-}" = 1 ] && exit 0
	cp "$FAKE_IMPACTED" "${*: -1}"
	;;
*) exit 3 ;;
esac
EOF
chmod +x "$BIN/bazel" "$BIN/bazelisk" "$BIN/java"
export PATH="$BIN:$PATH"
export FAKE_TARGETS="$TARGETS" FAKE_LOG="$TMP/queries.log" FAKE_IMPACTED="$TMP/impacted.txt" FAKE_ONCE="$TMP/failed-once"

REPO="$TMP/repo"
mkdir -p "$REPO"
(
	cd "$REPO" || exit 1
	git init -q -b main . && echo a >f && git add f && git commit -q -m base &&
		echo b >f && git commit -q -am head
) || {
	echo "FAIL: could not build the throwaway repository"
	exit 1
}
BASE="$(git -C "$REPO" rev-parse HEAD^)"
JAR="$TMP/bazel-diff_deploy.jar"
: >"$JAR"

# run <script> <out-dir> [VAR=value...]: one run in the throwaway repository.
# Leaves the script's exit status in RC.
RC=0
run() {
	local script="$1" out="$2"
	shift 2
	rm -f "$FAKE_ONCE"
	[ -n "${KEEP_OUT:-}" ] || rm -rf "$out" # KEEP_OUT=1: run into a directory a previous run left
	: >"$TMP/gh_output"
	(cd "$REPO" && env OUT_DIR="$out" GITHUB_OUTPUT="$TMP/gh_output" HEAD_SHA= "$@" bash "$script") >"$TMP/log" 2>&1
	RC=$?
}
# lists <out-dir> <file>: the list on one line, for a literal comparison.
lists() { tr '\n' ' ' <"$1/$2" | sed 's/ $//'; }
expect() { # what, got, want
	if [ "$2" = "$3" ]; then
		pass "$1"
	else
		fail "$1"
		echo "    got:  $2"
		echo "    want: $3"
	fi
}
output() { grep -cxF "$1" "$TMP/gh_output"; }

# What every path must produce for the whole table. No `manual` test in either
# list; the look-alike tags stay; `integration` first, last and alone in its
# list is integration, and its look-alikes are unit tests.
WANT_UNIT='//pkg:integration_slow_test //pkg:integrationx_test //pkg:manual_ish_test //pkg:no_integration_test //pkg:no_manual_x_test //pkg:root_test //pkg:unit_test'
WANT_INTEGRATION='//pkg:docker_test //pkg:etcd_test //pkg:manually_test'
INTEGRATION_LOOKALIKES='//pkg:integration_slow_test //pkg:integrationx_test //pkg:no_integration_test'
# The bazel-diff path's build list: every rule not tagged `manual`, and the e2e
# target (which is `manual`) because it is impacted.
WANT_BUILD='//pkg:docker_test //pkg:etcd_test //pkg:integration_slow_test //pkg:integrationx_test //pkg:lib //pkg:manual_ish_test //pkg:manually_test //pkg:no_integration_test //pkg:no_manual_x_test //pkg:root_test //pkg:unit_test //test/e2e:e2e_test'
MANUAL_RULES='//charts/x:fails_by_design_test //e2e/img:push //pkg:manual_first_test //pkg:manual_last_test //pkg:manual_middle_test'
MANUAL_TESTS='//charts/x:fails_by_design_test //pkg:manual_first_test //pkg:manual_last_test //pkg:manual_middle_test'

# --- 1. bazel-diff path, everything impacted ------------------------------------
{
	cut -f1 "$TARGETS"
	echo //pkg:lib.go # a source-file label: in no rule list
} >"$FAKE_IMPACTED"
OUT="$TMP/out1"
run "$SCRIPT" "$OUT" BASE_SHA="$BASE" BAZEL_DIFF_JAR="$JAR"
expect "impacted: unit list has no manual test" "$(lists "$OUT" impacted_unit.txt)" "$WANT_UNIT"
expect "impacted: integration list has no manual test" "$(lists "$OUT" impacted_integration.txt)" "$WANT_INTEGRATION"
expect "impacted: build list has no manual rule but the impacted e2e target (#1438)" "$(lists "$OUT" impacted_build.txt)" "$WANT_BUILD"
expect "impacted: has_e2e follows the manual e2e target" "$(output has_e2e=true)" 1
expect "impacted: has_unit, has_integration" "$(output has_unit=true)$(output has_integration=true)" 11
if grep -q fallback "$TMP/log"; then
	fail "impacted: took the full-run fallback, so case 1 did not test the bazel-diff path"
	sed 's/^/    /' "$TMP/log"
else
	pass "impacted: bazel-diff path, no fallback"
fi
expect "impacted: the repository is back on its branch" "$(git -C "$REPO" rev-parse --abbrev-ref HEAD)" main

# --- 2. only manual tests impacted: nothing to run -------------------------------
tr ' ' '\n' <<<"$MANUAL_TESTS" >"$FAKE_IMPACTED"
OUT="$TMP/out2"
run "$SCRIPT" "$OUT" BASE_SHA="$BASE" BAZEL_DIFF_JAR="$JAR"
expect "only manual impacted: empty unit list" "$(lists "$OUT" impacted_unit.txt)" ""
expect "only manual impacted: empty integration list" "$(lists "$OUT" impacted_integration.txt)" ""
expect "only manual impacted: has_unit=false, has_integration=false, has_e2e=false" \
	"$(output has_unit=false)$(output has_integration=false)$(output has_e2e=false)" 111
expect "only manual impacted: empty build list, has_any=false (#1438)" "$(lists "$OUT" impacted_build.txt) $(output has_any=false)" " 1"

# --- 2b. the e2e target and another manual rule impacted --------------------------
# The e2e target is the one manual rule the build list keeps: with has_any=false
# the `test` leg is skipped, and the `e2e` leg needs it.
printf '%s\n' //e2e/img:push //test/e2e:e2e_test >"$FAKE_IMPACTED"
OUT="$TMP/out2b"
run "$SCRIPT" "$OUT" BASE_SHA="$BASE" BAZEL_DIFF_JAR="$JAR"
expect "e2e + a manual rule impacted: only the e2e target is built" "$(lists "$OUT" impacted_build.txt)" "//test/e2e:e2e_test"
expect "e2e + a manual rule impacted: has_any=true, has_e2e=true, has_unit=false" \
	"$(output has_any=true)$(output has_e2e=true)$(output has_unit=false)" 111

# --- 3. e2e not impacted ----------------------------------------------------------
echo //pkg:unit_test >"$FAKE_IMPACTED"
OUT="$TMP/out3"
run "$SCRIPT" "$OUT" BASE_SHA="$BASE" BAZEL_DIFF_JAR="$JAR"
expect "one unit test impacted: unit list" "$(lists "$OUT" impacted_unit.txt)" "//pkg:unit_test"
expect "one unit test impacted: has_e2e=false" "$(output has_e2e=false)" 1

# --- 4. the full-run fallbacks ----------------------------------------------------
cut -f1 "$TARGETS" >"$FAKE_IMPACTED"
fallback() { # what, env...
	local what="$1"
	shift
	OUT="$TMP/out4"
	run "$SCRIPT" "$OUT" "$@"
	if ! grep -q 'fallback (full run)' "$TMP/log"; then
		fail "$what: no fallback"
		sed 's/^/    /' "$TMP/log"
		return
	fi
	expect "$what: unit list has no manual test" "$(lists "$OUT" impacted_unit.txt)" "$WANT_UNIT"
	expect "$what: integration list has no manual test" "$(lists "$OUT" impacted_integration.txt)" "$WANT_INTEGRATION"
	expect "$what: builds //... and sets every has_*" \
		"$(lists "$OUT" impacted_build.txt) $(output has_any=true)$(output has_unit=true)$(output has_integration=true)$(output has_e2e=true)" \
		"//... 1111"
}
fallback "fallback (no jar)" BASE_SHA="$BASE"
fallback "fallback (no BASE_SHA)" BAZEL_DIFF_JAR="$JAR"
fallback "fallback (unreachable BASE_SHA)" BASE_SHA=0000000000000000000000000000000000000000 BAZEL_DIFF_JAR="$JAR"
fallback "fallback (bazel-diff fails)" BASE_SHA="$BASE" BAZEL_DIFF_JAR="$JAR" FAKE_JAVA_FAIL=1

# --- 5. anti-vacuity: without a filter, this fixture lets them through ------------
# Take one filter back out of a copy. If the targets it guards do not then
# appear, the fake or the table stopped exercising it and the cases above prove
# nothing.
{
	cut -f1 "$TARGETS"
	echo //pkg:lib.go
} >"$FAKE_IMPACTED"
unfiltered() { # what, sed expression, guarded labels, list file...
	local what="$1" expr="$2" want="$3" got
	shift 3
	local old="$TMP/unfiltered.sh"
	sed -e "$expr" "$SCRIPT" >"$old"
	if cmp -s "$old" "$SCRIPT"; then
		fail "anti-vacuity ($what): could not take the filter out (did the variable change?)"
		return
	fi
	OUT="$TMP/out5"
	run "$old" "$OUT" BASE_SHA="$BASE" BAZEL_DIFF_JAR="$JAR"
	got="$(cd "$OUT" && cat "$@" | sort -u | grep -Fxf <(tr ' ' '\n' <<<"$want") | tr '\n' ' ' | sed 's/ $//')"
	expect "anti-vacuity ($what)" "$got" "$want"
}
unfiltered "unfiltered, the four manual tests reach the test lists" \
	's|^RUNNABLE_TESTS=.*$|RUNNABLE_TESTS="tests(//...)"|' \
	"$MANUAL_TESTS" impacted_unit.txt impacted_integration.txt
unfiltered "unfiltered, the manual rules reach the build list" \
	's|^BUILDABLE_RULES=.*$|BUILDABLE_RULES="kind(rule, //...)"|' \
	"$MANUAL_RULES" impacted_build.txt
# shellcheck disable=SC2016 # ${RUNNABLE_TESTS} is for the copy to expand
unfiltered "matched as a substring, the look-alike tags reach the integration list" \
	's|^INTEGRATION_TESTS=.*$|INTEGRATION_TESTS="attr(tags, \\"integration\\", ${RUNNABLE_TESTS})"|' \
	"$INTEGRATION_LOOKALIKES" impacted_integration.txt

# --- 6. a query that fails or returns nothing (#1437) -----------------------------
# The three queries, as globs over the expression the script passes.
Q_RULES='kind(rule,*'
Q_TESTS='tests(*'
Q_INTEGRATION='attr(*integration*'

# full: the run must have taken the full-run fallback and produced the full lists.
full() { # what
	if [ "$RC" -ne 0 ] || ! grep -q 'fallback (full run)' "$TMP/log"; then
		fail "$1: expected the full-run fallback, got exit $RC"
		sed 's/^/    /' "$TMP/log"
		return
	fi
	expect "$1: full run (build //..., every test, every has_*)" \
		"$(lists "$OUT" impacted_build.txt) | $(lists "$OUT" impacted_unit.txt) | $(lists "$OUT" impacted_integration.txt) | $(output has_any=true)$(output has_unit=true)$(output has_integration=true)$(output has_e2e=true)" \
		"//... | $WANT_UNIT | $WANT_INTEGRATION | 1111"
}
# loud: the run must have failed, said why, and set no output at all: an unset
# has_* skips a leg just as `false` does, so the exit status is what the caller
# has to see.
loud() { # what
	expect "$1: exits non-zero" "$([ "$RC" -ne 0 ] && echo failed || echo "exit 0")" failed
	expect "$1: sets no has_* output" "$(grep -c '^has_' "$TMP/gh_output")" 0
	if grep -q '^::error::' "$TMP/log"; then
		pass "$1: says so with ::error::"
	else
		fail "$1: no ::error:: line"
		sed 's/^/    /' "$TMP/log"
	fi
}

for mode in FAKE_QUERY_FAIL FAKE_QUERY_EMPTY FAKE_QUERY_PARTIAL; do
	OUT="$TMP/out6"
	# The rule query is only needed to intersect with bazel-diff's answer. The
	# full run builds //... and does not ask it, so that is where this goes.
	run "$SCRIPT" "$OUT" BASE_SHA="$BASE" BAZEL_DIFF_JAR="$JAR" "$mode=$Q_RULES"
	full "bazel-diff path, rule query $mode"
	# The test queries are needed by the full run too: nothing wider to fall
	# back to, so the step fails.
	run "$SCRIPT" "$OUT" BASE_SHA="$BASE" BAZEL_DIFF_JAR="$JAR" "$mode=$Q_TESTS"
	loud "bazel-diff path, test query $mode"
	run "$SCRIPT" "$OUT" BASE_SHA="$BASE" BAZEL_DIFF_JAR="$JAR" "$mode=$Q_INTEGRATION"
	loud "bazel-diff path, integration query $mode"
	run "$SCRIPT" "$OUT" BASE_SHA="$BASE" "$mode=$Q_TESTS"
	loud "full run (no jar), test query $mode"
	run "$SCRIPT" "$OUT" BASE_SHA="$BASE" "$mode=$Q_INTEGRATION"
	loud "full run (no jar), integration query $mode"
done
# Bazel's own error reaches the log: it is not sent to /dev/null any more.
run "$SCRIPT" "$OUT" BASE_SHA="$BASE" BAZEL_DIFF_JAR="$JAR" FAKE_QUERY_FAIL="$Q_TESTS"
if grep -q 'fake bazel: ERROR: query failed' "$TMP/log"; then
	pass "a failed query's stderr is in the log"
else
	fail "a failed query's stderr is not in the log"
fi
# A transient failure costs a full run, not the step: the fallback asks again.
for q in "$Q_TESTS" "$Q_INTEGRATION"; do
	run "$SCRIPT" "$OUT" BASE_SHA="$BASE" BAZEL_DIFF_JAR="$JAR" FAKE_QUERY_FAIL_ONCE="$q"
	full "bazel-diff path, query '$q' fails once"
done

# An integration query that matches everything leaves the unit leg nothing, and
# exits 0 with a full list: refused as well.
ONLY_INTEGRATION="$TMP/only-integration.tsv"
awk -F'\t' '$3 ~ /[[ ]integration[],]/' "$TARGETS" >"$ONLY_INTEGRATION"
run "$SCRIPT" "$OUT" BASE_SHA="$BASE" BAZEL_DIFF_JAR="$JAR" FAKE_TARGETS="$ONLY_INTEGRATION"
loud "bazel-diff path, every test is integration"

# --- 7. bazel-diff exits 0 without writing its list -------------------------------
OUT="$TMP/out7"
run "$SCRIPT" "$OUT" BASE_SHA="$BASE" BAZEL_DIFF_JAR="$JAR" FAKE_JAVA_NO_OUTPUT=1
full "bazel-diff wrote no impacted list"
# The same into an output directory an earlier run left its list in (the default
# OUT_DIR is in the tree): the stale list is not this run's answer.
mkdir -p "$OUT"
echo //pkg:unit_test >"$OUT/impacted.txt"
KEEP_OUT=1 run "$SCRIPT" "$OUT" BASE_SHA="$BASE" BAZEL_DIFF_JAR="$JAR" FAKE_JAVA_NO_OUTPUT=1
full "bazel-diff wrote no impacted list, a stale one is there"

# --- 8. the checkout cannot be put back -------------------------------------------
# bazel-diff fails on the base revision and leaves the tree dirty, so git will
# not move back to the head. A fallback from there would list the BASE's tests.
OUT="$TMP/out8"
run "$SCRIPT" "$OUT" BASE_SHA="$BASE" BAZEL_DIFF_JAR="$JAR" FAKE_JAVA_DIRTY=1
expect "stuck checkout: the fixture left the repository on the base revision" "$(git -C "$REPO" rev-parse HEAD)" "$BASE"
loud "checkout stuck on the base revision"
git -C "$REPO" checkout -q -f main

# --- 9. the lists are for the commit that is checked out (#1459) -------------------
# The jobs that read the lists build the commit they check out. A HEAD_SHA that
# is another commit would list targets on one tree for a build of another: the
# bazel-diff path lists at HEAD_SHA, the full run where the checkout is.
HEAD_COMMIT="$(git -C "$REPO" rev-parse HEAD)"
cut -f1 "$TARGETS" >"$FAKE_IMPACTED"
OUT="$TMP/out9"
run "$SCRIPT" "$OUT" BASE_SHA="$BASE" BAZEL_DIFF_JAR="$JAR" HEAD_SHA="$BASE"
loud "HEAD_SHA is not the checkout (bazel-diff path)"
run "$SCRIPT" "$OUT" BASE_SHA="$BASE" HEAD_SHA="$BASE"
loud "HEAD_SHA is not the checkout (full run, no jar)"
run "$SCRIPT" "$OUT" BASE_SHA="$BASE" BAZEL_DIFF_JAR="$JAR" HEAD_SHA=0000000000000000000000000000000000000000
loud "HEAD_SHA is not a commit"
# The same commit under another name is the checkout.
run "$SCRIPT" "$OUT" BASE_SHA="$BASE" BAZEL_DIFF_JAR="$JAR" HEAD_SHA=main
expect "HEAD_SHA names the checkout by branch: accepted" "$RC $(output has_any=true)" "0 1"
# Every path that writes the lists says which commit they are for: the jobs
# that download them compare it with their own checkout (ci-impacted-lists.sh).
expect "bazel-diff path: impacted_commit.txt is the checkout" "$(lists "$OUT" impacted_commit.txt)" "$HEAD_COMMIT"
run "$SCRIPT" "$OUT" BASE_SHA="$BASE"
expect "full run: impacted_commit.txt is the checkout" "$(lists "$OUT" impacted_commit.txt)" "$HEAD_COMMIT"
: >"$FAKE_IMPACTED"
run "$SCRIPT" "$OUT" BASE_SHA="$BASE" BAZEL_DIFF_JAR="$JAR"
expect "nothing impacted: impacted_commit.txt is the checkout, and every has_* is written as false" \
	"$(lists "$OUT" impacted_commit.txt) $(output has_any=false)$(output has_unit=false)$(output has_integration=false)$(output has_e2e=false)" \
	"$HEAD_COMMIT 1111"
# A commit file an earlier run left in OUT_DIR is not this run's: a failed run
# leaves none behind.
mkdir -p "$OUT"
echo stale >"$OUT/impacted_commit.txt"
KEEP_OUT=1 run "$SCRIPT" "$OUT" BASE_SHA="$BASE" BAZEL_DIFF_JAR="$JAR" FAKE_QUERY_FAIL="$Q_TESTS"
loud "a failed run"
expect "a failed run leaves no impacted_commit.txt" "$([ -e "$OUT/impacted_commit.txt" ] && echo present || echo absent)" absent

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS check(s) failed"
	exit 1
fi
echo "all checks passed"
