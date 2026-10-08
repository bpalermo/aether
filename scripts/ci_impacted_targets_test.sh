#!/usr/bin/env bash
# Test of scripts/ci-impacted-targets.sh against a throwaway git repository, a
# fake `bazel` and a fake `java` (bazel-diff): no Bazel, no JVM, no network.
#
# The fake `bazel query` answers from a table of label, kind and tags, and it
# applies the regex the script passes in `attr("tags", "<regex>", ...)` to the
# tag list rendered the way Bazel renders it (`[a, b, c]`). So the cases below
# exercise the script's own expression, not a canned answer per query.
#
# What it pins (#1413):
#   - a test tagged `manual` is in neither impacted_unit.txt nor
#     impacted_integration.txt, on the bazel-diff path and on every full-run
#     fallback: those lists reach `bazel test` as explicit labels, where Bazel
#     no longer honours the tag;
#   - the tag is matched whole: `no-manual-x`, `manually` and `manual-ish` stay,
#     and `manual` is found first, in the middle and last in a list;
#   - has_e2e still follows //test/e2e:e2e_test, which is `manual` itself;
#   - anti-vacuity: with the filter taken back out of a copy of the script, the
#     same fixture puts the `manual` tests in the lists.
#
# Run: bazel test //scripts:ci_impacted_targets_test, or
#      bash scripts/ci_impacted_targets_test.sh
set -uo pipefail

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
# `attr(tags, "integration", <expr>)`: keep the rows whose tag list matches.
if [[ "$q" == 'attr(tags, "integration", '* ]]; then
	rows="$(awk -F'\t' '$3 ~ /integration/' <<<"$rows")"
fi
[ -n "$rows" ] && cut -f1 <<<"$rows"
exit 0
EOF
cp "$BIN/bazel" "$BIN/bazelisk" # the script prefers bazelisk when it is on PATH
cat >"$BIN/java" <<'EOF'
#!/usr/bin/env bash
# Fake bazel-diff: `-jar <jar> generate-hashes ... <out>` writes <out>;
# `-jar <jar> get-impacted-targets ... -o <out>` copies $FAKE_IMPACTED there.
set -uo pipefail
[ "${FAKE_JAVA_FAIL:-}" = 1 ] && exit 1
case "$3" in
generate-hashes) echo '{}' >"${*: -1}" ;;
get-impacted-targets) cp "$FAKE_IMPACTED" "${*: -1}" ;;
*) exit 3 ;;
esac
EOF
chmod +x "$BIN/bazel" "$BIN/bazelisk" "$BIN/java"
export PATH="$BIN:$PATH"
export FAKE_TARGETS="$TARGETS" FAKE_LOG="$TMP/queries.log" FAKE_IMPACTED="$TMP/impacted.txt"

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
run() {
	local script="$1" out="$2"
	shift 2
	rm -rf "$out"
	: >"$TMP/gh_output"
	(cd "$REPO" && env OUT_DIR="$out" GITHUB_OUTPUT="$TMP/gh_output" HEAD_SHA= "$@" bash "$script") >"$TMP/log" 2>&1
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
# list; the look-alike tags stay.
WANT_UNIT='//pkg:manual_ish_test //pkg:no_manual_x_test //pkg:root_test //pkg:unit_test'
WANT_INTEGRATION='//pkg:docker_test //pkg:manually_test'
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

# --- 5. anti-vacuity: without the filter, this fixture lets them through ----------
# Take the `except attr(...)` clause back out of a copy. If the manual tests do
# not then appear, the fake or the table stopped exercising the filter and the
# cases above prove nothing.
OLD="$TMP/unfiltered.sh"
sed -e 's|^RUNNABLE_TESTS=.*$|RUNNABLE_TESTS='"'"'tests(//...)'"'"'|' "$SCRIPT" >"$OLD"
if cmp -s "$OLD" "$SCRIPT"; then
	fail "anti-vacuity: could not take the filter out (did RUNNABLE_TESTS change?)"
else
	OUT="$TMP/out5"
	run "$OLD" "$OUT" BASE_SHA="$BASE" BAZEL_DIFF_JAR="$JAR"
	got="$(cat "$OUT/impacted_unit.txt" "$OUT/impacted_integration.txt" | sort -u | grep -Fxf <(tr ' ' '\n' <<<"$MANUAL_TESTS") | tr '\n' ' ' | sed 's/ $//')"
	expect "anti-vacuity: unfiltered, the four manual tests reach the lists" "$got" "$MANUAL_TESTS"
fi

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS check(s) failed"
	exit 1
fi
echo "all checks passed"
