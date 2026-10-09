#!/usr/bin/env bash
# check_rules.sh, held to rule files written to fail (#1492).
#
# The checker is one more script between a rule file and a green test. This
# runs it, with the pinned promtool, on fixtures whose verdict is known:
#
#   - good.yml + good_test.yml pass, and the output says both ran;
#   - broken.yml (an expression that is not PromQL) fails `check rules`;
#   - not_rules.yml (YAML, but not a rule file) fails `check rules`;
#   - failing_test.yml (expects an alert the rule does not fire) fails
#     `test rules`, and the good rule file next to it still passes;
#   - a failure does not stop the run: the files after it are still reported;
#   - a .yml that is beside the files given and was not given fails;
#   - no rule file at all fails.

# --- begin runfiles.bash initialization v3 ---
# shellcheck disable=SC1090
set -uo pipefail
set +e
f=bazel_tools/tools/bash/runfiles/runfiles.bash
source "${RUNFILES_DIR:-/dev/null}/$f" 2>/dev/null ||
	source "$(grep -sm1 "^$f " "${RUNFILES_MANIFEST_FILE:-/dev/null}" | cut -f2- -d' ')" 2>/dev/null ||
	source "$0.runfiles/$f" 2>/dev/null ||
	source "$(grep -sm1 "^$f " "$0.runfiles_manifest" | cut -f2- -d' ')" 2>/dev/null ||
	source "$(grep -sm1 "^$f " "$0.exe.runfiles_manifest" | cut -f2- -d' ')" 2>/dev/null ||
	{
		echo >&2 "ERROR: cannot find $f"
		exit 1
	}
f=
# --- end runfiles.bash initialization v3 ---

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
CHECK="$HERE/check_rules.sh"
DATA="$HERE/testdata"
PROMTOOL="${PROMTOOL:-$(rlocation "${PROMTOOL_RLOCATIONPATH:?set by the BUILD target}")}"
export PROMTOOL
for f in "$CHECK" "$DATA/good.yml" "$PROMTOOL"; do
	[ -e "$f" ] || {
		echo "FAIL: $f not found"
		exit 1
	}
done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FAILS=0
pass() { echo "PASS  $*"; }
fail() {
	echo "FAIL  $*"
	sed 's/^/    | /' "$TMP/log"
	FAILS=$((FAILS + 1))
}
RC=0
check() {
	bash "$CHECK" "$@" >"$TMP/log" 2>&1
	RC=$?
}
# said <a line the output must have, as a fixed string>
said() { grep -qF -- "$1" "$TMP/log"; }
# A directory of its own for each case, holding the fixtures it names and no
# other: the checker fails on a .yml that is beside the files it was given and
# was not given.
N=0
D=""
with() {
	N=$((N + 1))
	D="$TMP/case$N"
	mkdir "$D"
	local f
	for f in "$@"; do cp "$DATA/$f" "$D/$f"; done
}

with good.yml good_test.yml
check --rules "$D/good.yml" --tests "$D/good_test.yml"
if [ "$RC" -eq 0 ] && said "PASS  check rules $D/good.yml" && said "PASS  test rules $D/good_test.yml" &&
	said "promtool: 1 rule file(s) checked, 1 rule test file(s) run"; then
	pass "a good rule file and its test pass, and both are reported"
else
	fail "a good rule file and its test: expected exit 0 and both reported, got exit $RC"
fi

with good.yml
check --rules "$D/good.yml" --tests
if [ "$RC" -eq 0 ] && said "promtool: 1 rule file(s) checked, 0 rule test file(s) run"; then
	pass "rule files with no rule test pass, and the output says no test ran"
else
	fail "rule files with no rule test: expected exit 0, got exit $RC"
fi

with broken.yml good.yml good_test.yml
check --rules "$D/broken.yml" "$D/good.yml" --tests "$D/good_test.yml"
if [ "$RC" -eq 1 ] && said "FAIL  check rules $D/broken.yml" && said "PASS  check rules $D/good.yml" &&
	said "PASS  test rules $D/good_test.yml" && said "1 failure(s) over 3 file(s) given"; then
	pass "a rule file with an invalid expression fails, and the files after it are still run"
else
	fail "a rule file with an invalid expression: expected exit 1 naming broken.yml only, got exit $RC"
fi

with not_rules.yml
check --rules "$D/not_rules.yml" --tests
if [ "$RC" -eq 1 ] && said "FAIL  check rules $D/not_rules.yml"; then
	pass "a YAML file that is not a rule file fails"
else
	fail "a YAML file that is not a rule file: expected exit 1, got exit $RC"
fi

with good.yml failing_test.yml good_test.yml
check --rules "$D/good.yml" --tests "$D/failing_test.yml" "$D/good_test.yml"
if [ "$RC" -eq 1 ] && said "PASS  check rules $D/good.yml" && said "FAIL  test rules $D/failing_test.yml" &&
	said "PASS  test rules $D/good_test.yml" && said "1 failure(s) over 3 file(s) given"; then
	pass "a rule test whose expectation does not hold fails, and the files around it are still run"
else
	fail "a rule test whose expectation does not hold: expected exit 1 naming failing_test.yml only, got exit $RC"
fi

with good.yml
check --rules "$D/good.yml" "$D/absent.yml" --tests
if [ "$RC" -eq 1 ] && said "FAIL  check rules $D/absent.yml: no such file"; then
	pass "a rule file that is not there fails"
else
	fail "a rule file that is not there: expected exit 1, got exit $RC"
fi

# What a caller that lists a file as data and does not pass it looks like.
with good.yml good_test.yml
check --rules "$D/good.yml" --tests
if [ "$RC" -eq 1 ] && said "PASS  check rules $D/good.yml" &&
	said "FAIL  $D/good_test.yml is a rule test next to the files given, and was not run"; then
	pass "a rule test beside the rule files that was not given fails"
else
	fail "a rule test beside the rule files that was not given: expected exit 1, got exit $RC"
fi

with good.yml broken.yml
check --rules "$D/good.yml" --tests
if [ "$RC" -eq 1 ] && said "FAIL  $D/broken.yml is next to the files given, and was not checked"; then
	pass "a rule file beside the rule files that was not given fails"
else
	fail "a rule file beside the rule files that was not given: expected exit 1, got exit $RC"
fi

# The same for files given by their bare name, as a target in the package of
# the rule files hands them over.
with good.yml good_test.yml
(cd "$D" && bash "$CHECK" --rules good.yml --tests good_test.yml) >"$TMP/log" 2>&1
RC=$?
if [ "$RC" -eq 0 ] && said "PASS  check rules good.yml" && said "PASS  test rules good_test.yml"; then
	pass "a rule file and its test given by bare name pass"
else
	fail "a rule file and its test given by bare name: expected exit 0, got exit $RC"
fi
(cd "$D" && bash "$CHECK" --rules good.yml --tests) >"$TMP/log" 2>&1
RC=$?
if [ "$RC" -eq 1 ] && said "FAIL  ./good_test.yml is a rule test next to the files given, and was not run"; then
	pass "a rule test beside a rule file given by bare name, and not given, fails"
else
	fail "a rule test beside a rule file given by bare name, and not given: expected exit 1, got exit $RC"
fi

with good_test.yml
check --rules --tests "$D/good_test.yml"
if [ "$RC" -eq 1 ] && said "no rule file was given"; then
	pass "no rule file at all fails"
else
	fail "no rule file at all: expected exit 1, got exit $RC"
fi

check
if [ "$RC" -eq 1 ] && said "no rule file was given"; then
	pass "no argument at all fails"
else
	fail "no argument at all: expected exit 1, got exit $RC"
fi

with good.yml
check "$D/good.yml"
if [ "$RC" -eq 2 ] && said "usage:"; then
	pass "a file before --rules or --tests is a usage error"
else
	fail "a file before --rules or --tests: expected exit 2, got exit $RC"
fi

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS check(s) failed"
	exit 1
fi
echo "all checks passed"
