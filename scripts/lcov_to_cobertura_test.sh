#!/usr/bin/env bash
# Test of scripts/lcov-to-cobertura.sh and scripts/coverage-summary.sh against
# fixtures in scripts/testdata/coverage. No Bazel, no network: bash, awk, sort.
#
# What it pins for the converter:
#   - the whole document, byte for byte, against a golden file (expected.xml):
#     the three things GitHub's code coverage reads (the root line-rate, each
#     <class filename=>, each <line number= hits=>) and the layout around them;
#   - the totals as literals worked out by hand from the fixture, so a change
#     that regenerated the golden file with a wrong count still fails;
#   - two inputs naming one file are added line by line (hits 3 + 2 = 5, and a
#     line only the second input has appears);
#   - path normalisation: --strip-prefix makes an exec-root path repo-relative,
#     a leading ./ is dropped, and a path that is still absolute or has a ..
#     segment fails the conversion instead of being uploaded;
#   - XML escaping of & < > " and the apostrophe in a file name;
#   - a package's files stay in one <package> although a sub-package sorts
#     between them by full path (a/b.go < a/b/c.go < a/d.go);
#   - a file with no DA record is left out, and an input with none at all is
#     refused;
#   - a hit count above 2^31 survives (awk's %d does not, in mawk).
# For the summary: the per-component table and totals as literals, that
# concatenated tracefiles are merged per line, and that its total equals the
# converter's for the same input.
#
# Run: bazel test //scripts:lcov_to_cobertura_test, or
#      bash scripts/lcov_to_cobertura_test.sh
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
CONVERT="$HERE/lcov-to-cobertura.sh"
SUMMARY="$HERE/coverage-summary.sh"
DATA="$HERE/testdata/coverage"
for f in "$CONVERT" "$SUMMARY" "$DATA/input.lcov" "$DATA/second.lcov" "$DATA/expected.xml" "$DATA/report.lcov" "$DATA/tests-only.lcov"; do
	[ -f "$f" ] || {
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
	FAILS=$((FAILS + 1))
}
check() {
	local what="$1"
	shift
	if "$@"; then pass "$what"; else fail "$what"; fi
}
# has <fixed string> <file>: some line of the file contains the string.
has() { grep -qF -- "$1" "$2"; }
# lacks <fixed string> <file>: the file exists and no line contains the string.
lacks() { [ -f "$2" ] && ! grep -qF -- "$1" "$2"; }
# refuse <description> <expected stderr substring> <command...>: the command
# must fail, say why, and write nothing to stdout.
refuse() {
	local what="$1" want="$2" err
	shift 2
	if err="$("$@" 2>&1 >"$TMP/refused.out")"; then
		fail "$what: succeeded, wanted a refusal"
	elif [[ "$err" != *"$want"* ]]; then
		fail "$what: refused without '$want': $err"
	elif [ -s "$TMP/refused.out" ]; then
		fail "$what: refused but still wrote a document"
	else
		pass "$what"
	fi
}

# --- the golden document -------------------------------------------------------
OUT="$TMP/out.xml"
if bash "$CONVERT" --timestamp 1700000000 --strip-prefix /exec/root/_main/ \
	"$DATA/input.lcov" "$DATA/second.lcov" >"$OUT" 2>"$TMP/err"; then
	pass "converts the fixture"
else
	fail "converts the fixture: $(cat "$TMP/err")"
fi
if diff -u "$DATA/expected.xml" "$OUT" >"$TMP/diff"; then
	pass "output equals the golden file"
else
	fail "output differs from the golden file"
	cat "$TMP/diff"
fi

# By hand from the fixture: a.go 4 lines (10 and 12 hit), b.go 1, d.go 2,
# b/c.go 1 (hit), cni/z.go 1 (hit), the quoted file 2 (line 3 hit), main.go 1.
# 12 lines, 5 covered, 5/12 = 0.41666...
check "root: 12 lines, 5 covered, line-rate 0.4167" \
	has '<coverage line-rate="0.4167" branch-rate="0" lines-covered="5" lines-valid="12"' "$OUT"
check "root carries the timestamp it was given" has ' timestamp="1700000000">' "$OUT"
check "hits of one line are added across inputs (3 + 2)" has '<line number="10" hits="5"/>' "$OUT"
check "a line only the second input names is present" has '<line number="13" hits="0"/>' "$OUT"
check "--strip-prefix makes an exec-root path repo-relative" has 'filename="cni/z.go"' "$OUT"
check "a hit count above 2^31 survives" has '<line number="100" hits="12345678901"/>' "$OUT"
check "a leading ./ is dropped and & < > \" ' are escaped" \
	has 'filename="common/x&amp;y/q&lt;&quot;uo&apos;te&gt;.go"' "$OUT"
check "a root-level file lands in package ." has '<package name="." ' "$OUT"
check "package agent/internal/a: 2 of 7" has '<package name="agent/internal/a" line-rate="0.2857"' "$OUT"
check "a file with no DA record is left out" lacks 'empty.go' "$OUT"
# One <package> per directory: a/b.go and a/d.go sort either side of a/b/c.go.
check "each directory is exactly one <package>" \
	test "$(grep -c '<package ' "$OUT")" = "$(grep -o '<package name="[^"]*"' "$OUT" | sort -u | wc -l)"
check "five packages" test "$(grep -c '<package ' "$OUT")" = 5
check "seven classes" test "$(grep -c '<class ' "$OUT")" = 7

# Deterministic: the default timestamp is SOURCE_DATE_EPOCH, else 0.
bash "$CONVERT" --strip-prefix /exec/root/_main/ "$DATA/input.lcov" >"$TMP/default.xml" 2>/dev/null
check "default timestamp is 0" has ' timestamp="0">' "$TMP/default.xml"
SOURCE_DATE_EPOCH=42 bash "$CONVERT" --strip-prefix /exec/root/_main/ "$DATA/input.lcov" >"$TMP/sde.xml" 2>/dev/null
check "SOURCE_DATE_EPOCH is the default timestamp" has ' timestamp="42">' "$TMP/sde.xml"

# --- refusals ------------------------------------------------------------------
refuse "an absolute path left absolute" "not repo-relative: /exec/root/_main/cni/z.go" \
	bash "$CONVERT" "$DATA/input.lcov"
printf 'SF:agent/../../etc/passwd\nDA:1,1\nend_of_record\n' >"$TMP/dotdot.lcov"
refuse "a .. segment" "has a .. segment" bash "$CONVERT" "$TMP/dotdot.lcov"
printf 'SF:agent/a.go\nLH:0\nLF:0\nend_of_record\n' >"$TMP/empty.lcov"
refuse "an input with no coverable line" "refusing to write an empty report" bash "$CONVERT" "$TMP/empty.lcov"
printf 'SF:agent/a.go\nDA:x,1\nend_of_record\n' >"$TMP/bad.lcov"
refuse "a malformed DA record" "malformed DA record" bash "$CONVERT" "$TMP/bad.lcov"
printf 'DA:1,1\n' >"$TMP/orphan.lcov"
refuse "a DA record outside any SF" "DA record outside an SF record" bash "$CONVERT" "$TMP/orphan.lcov"
refuse "a missing input file" "no such file" bash "$CONVERT" "$TMP/does-not-exist.lcov"
refuse "a non-numeric --timestamp" "--timestamp must be" bash "$CONVERT" --timestamp now "$DATA/second.lcov"

# --- the summary ---------------------------------------------------------------
# report.lcov: agent/x/a.go 3 lines (1 and 3 hit), agent/y/untested.go 4 lines
# at zero (the baseline record of a file no test links), cni/p.go 2 lines (5
# hit). tests-only.lcov: a.go from two tests (line 1 hit by one, line 3 by the
# other), cni/p.go; no untested.go.
S="$TMP/summary.md"
if bash "$SUMMARY" "$DATA/report.lcov" "$DATA/tests-only.lcov" >"$S" 2>"$TMP/err"; then
	pass "summarises the fixture"
else
	fail "summarises the fixture: $(cat "$TMP/err")"
fi
# shellcheck disable=SC2016 # literal Markdown backticks, nothing to expand
check "agent row: the zero-hit file moves 66.67% to 28.57%" \
	has '| `agent` | 2 | 7 | 2 | 28.57% | 3 | 2 | 66.67% |' "$S"
# shellcheck disable=SC2016 # literal Markdown backticks, nothing to expand
check "cni row" has '| `cni` | 1 | 2 | 1 | 50.00% | 2 | 1 | 50.00% |' "$S"
check "total row" has '| **Total** | 3 | 9 | 3 | **33.33%** | 5 | 3 | 60.00% |' "$S"
check "machine-readable total is the last line" \
	test "$(tail -n 1 "$S")" = '<!-- coverage-total lines=9 covered=3 percent=33.33 -->'

bash "$SUMMARY" "$DATA/report.lcov" >"$TMP/summary1.md" 2>/dev/null
check "one input: no tests-only columns" has '| **Total** | 3 | 9 | 3 | **33.33%** |' "$TMP/summary1.md"
check "one input: five-column header" has '| Component | Files | Lines | Covered | Coverage |' "$TMP/summary1.md"

# The summary and the converter must agree on what they were both given.
bash "$CONVERT" "$DATA/report.lcov" >"$TMP/report.xml" 2>/dev/null
check "converter total equals the summary total (9 lines, 3 covered)" \
	has 'lines-covered="3" lines-valid="9"' "$TMP/report.xml"

refuse "summary of an input with no coverable line" "refusing to summarise an empty report" \
	bash "$SUMMARY" "$TMP/empty.lcov"

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS check(s) failed"
	exit 1
fi
echo "all checks passed"
