#!/usr/bin/env bash
# Test of scripts/coverage.sh against a throwaway git repository and a fake
# `bazel`: no Bazel, no network, no Go. The fake answers the script's queries
# from files the cases write, records what `bazel coverage` was asked to do,
# and serves a canned combined report and per-test tracefiles.
#
# What it pins:
#   - the components are derived from the Go targets, minus the excluded
#     top-level directories (api, bazel, e2e, test): the instrumentation filter
#     and the library universe name exactly the rest;
#   - an excluded subtree of a component (agent/test: harnesses that Go's
#     internal-package rule keeps under agent/, #1311) is subtracted from the
#     filter and from the library universe, its records are dropped from the
#     report and its files from unrecorded.txt, while its tests still run; an
#     entry that no longer exists is refused;
#   - the invocation: --config=coverage, the derived filter, the caller's flags
#     passed through, and a target list that is the unit tests plus every
#     component go_library plus any go_binary with sources of its own;
#   - the report keeps component records only, whatever the combined report
#     held, and the Cobertura totals and summary are the fixture's, as literals;
#   - unrecorded.txt is the tracked non-test component .go files with no record;
#   - every way of reporting on nothing is refused: a failed query, an empty
#     test or library list, a failed `bazel coverage`, a missing combined
#     report, a test that left no coverage.dat.
#
# The real thing is `make coverage` and the `report` job of
# .github/workflows/coverage.yaml.
#
# Run: bazel test //scripts:coverage_test, or bash scripts/coverage_test.sh
set -uo pipefail

HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
for f in coverage.sh lcov-to-cobertura.sh coverage-summary.sh; do
	[ -f "$HERE/$f" ] || {
		echo "FAIL: $HERE/$f not found"
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
check() {
	local what="$1"
	shift
	if "$@"; then pass "$what"; else fail "$what"; fi
}
has() { grep -qF -- "$1" "$2"; }
lacks() { [ -f "$2" ] && ! grep -qF -- "$1" "$2"; }
# refuse <description> <expected output substring> <command...>: the command
# must fail and say why.
refuse() {
	local what="$1" want="$2" out
	shift 2
	if out="$("$@" 2>&1)"; then
		fail "$what: succeeded, wanted a refusal"
	elif [[ "$out" != *"$want"* ]]; then
		fail "$what: refused without '$want': $out"
	else
		pass "$what"
	fi
}

# No user or system git configuration, and a fixed commit time: the XML's
# timestamp is the commit's.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid
export GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid
export GIT_AUTHOR_DATE='1700000000 +0000' GIT_COMMITTER_DATE='1700000000 +0000'

REPO="$TMP/repo"
FAKE="$TMP/fake"
mkdir -p "$REPO/scripts" "$FAKE/bin"
cp "$HERE/coverage.sh" "$HERE/lcov-to-cobertura.sh" "$HERE/coverage-summary.sh" "$REPO/scripts/"
chmod +x "$REPO"/scripts/*.sh

# --- the fake bazel -----------------------------------------------------------
cat >"$FAKE/bin/bazel" <<'EOF'
#!/usr/bin/env bash
# Serves canned answers from $FAKE_DIR and logs every invocation, one argument
# per line after a "--- <command>" header.
{
	echo "--- $1"
	printf '%s\n' "$@"
} >>"$FAKE_DIR/calls"
case "$1" in
query)
	[ ! -f "$FAKE_DIR/query_fails" ] || {
		echo "ERROR: fake query failure" >&2
		exit 7
	}
	expr="${*: -1}"
	case "$expr" in
	'kind("go_library|go_binary", //...)') cat "$FAKE_DIR/go_targets" ;;
	'kind("go_test", //...) except '*) cat "$FAKE_DIR/tests" ;;
	'kind("go_library", '*) cat "$FAKE_DIR/libraries" ;;
	'attr("srcs", '*) cat "$FAKE_DIR/binaries" ;;
	*)
		echo "fake bazel: unexpected query: $expr" >&2
		exit 64
		;;
	esac
	;;
coverage)
	for arg in "$@"; do
		case "$arg" in
		--target_pattern_file=*) cp "${arg#*=}" "$FAKE_DIR/targets_seen" ;;
		esac
	done
	[ ! -f "$FAKE_DIR/coverage_fails" ] || exit 3
	;;
info)
	case "$2" in
	output_path) echo "$FAKE_DIR/out" ;;
	bazel-testlogs) echo "$FAKE_DIR/testlogs" ;;
	*) exit 64 ;;
	esac
	;;
*)
	echo "fake bazel: unexpected command $1" >&2
	exit 64
	;;
esac
EOF
chmod +x "$FAKE/bin/bazel"
export FAKE_DIR="$FAKE" BAZEL="$FAKE/bin/bazel"

# --- the fixture --------------------------------------------------------------
# Go targets in two components (agent, cni) and in all four excluded
# directories, plus a root-package target, which belongs to no directory.
reset_fake() {
	rm -rf "$FAKE/out" "$FAKE/testlogs" "$FAKE"/calls "$FAKE"/targets_seen \
		"$FAKE"/query_fails "$FAKE"/coverage_fails
	cat >"$FAKE/go_targets" <<'EOF'
//:root_tool
//agent/a:a
//agent/cmd/x:x
//agent/cmd/x:x_lib
//agent/test/h:h
//api/v1:v1
//bazel/tool:tool
//cni/p:p
//e2e/echo:echo
//test/h:h
EOF
	printf '%s\n' '//agent/a:a_test' '//agent/test/h:h_test' '//bazel/tool:tool_test' '//cni/p:p_test' >"$FAKE/tests"
	printf '%s\n' '//agent/a:a' '//agent/cmd/x:x_lib' '//cni/p:p' >"$FAKE/libraries"
	printf '%s\n' '//agent/cmd/x:x' >"$FAKE/binaries"
	mkdir -p "$FAKE/out/_coverage" "$FAKE/testlogs/agent/a/a_test" "$FAKE/testlogs/agent/test/h/h_test" \
		"$FAKE/testlogs/bazel/tool/tool_test" "$FAKE/testlogs/cni/p/p_test"
	# The combined report: two component files measured, one at zero (the
	# baseline of a library no test links), and three records that are not
	# component code and must not survive: generated Go, a bazel-out source,
	# and a harness file in the excluded subtree (which the filter keeps out of
	# a real report; the script must not depend on that).
	cat >"$FAKE/out/_coverage/_coverage_report.dat" <<'EOF'
SF:agent/a/a.go
DA:1,2
DA:2,0
LH:1
LF:2
end_of_record
SF:agent/cmd/x/main.go
DA:10,0
DA:11,0
DA:12,0
LH:0
LF:3
end_of_record
SF:agent/test/h/h.go
DA:1,7
DA:2,7
DA:3,7
DA:4,7
LH:4
LF:4
end_of_record
SF:api/v1/x.pb.go
DA:1,9
LH:1
LF:1
end_of_record
SF:bazel-out/k8-fastbuild/bin/agent/a/gen.go
DA:1,9
LH:1
LF:1
end_of_record
SF:cni/p/p.go
DA:5,1
LH:1
LF:1
end_of_record
EOF
	printf 'SF:agent/a/a.go\nDA:1,2\nDA:2,0\nLH:1\nLF:2\nend_of_record\n' >"$FAKE/testlogs/agent/a/a_test/coverage.dat"
	: >"$FAKE/testlogs/bazel/tool/tool_test/coverage.dat"
	# The harness's own test: it exercises component code (a.go) and, were it
	# instrumented, its own package.
	printf 'SF:agent/a/a.go\nDA:1,1\nDA:2,0\nLH:1\nLF:2\nend_of_record\nSF:agent/test/h/h.go\nDA:1,7\nLH:1\nLF:1\nend_of_record\n' >"$FAKE/testlogs/agent/test/h/h_test/coverage.dat"
	printf 'SF:cni/p/p.go\nDA:5,1\nLH:1\nLF:1\nend_of_record\n' >"$FAKE/testlogs/cni/p/p_test/coverage.dat"
}

(
	cd "$REPO" || exit 1
	git init -q .
	mkdir -p agent/a agent/cmd/x agent/test/h cni/p bazel/tool
	for f in agent/a/a.go agent/a/a_test.go agent/a/a_windows.go agent/cmd/x/main.go agent/test/h/h.go agent/test/h/gen/main.go agent/test/h/h_test.go cni/p/p.go bazel/tool/main.go; do
		mkdir -p "$(dirname "$f")"
		echo 'package p' >"$f"
	done
	echo '/coverage-report/' >.gitignore
	git add -A .
	git commit -q -m fixture
) || {
	echo "FAIL: could not build the fixture repository"
	exit 1
}

run() { (cd "$TMP" && bash "$REPO/scripts/coverage.sh" "$@"); }

# --- the happy path -----------------------------------------------------------
reset_fake
OUT="$TMP/report"
if run --out "$OUT" -- --jobs=6 --some_flag >"$TMP/log" 2>&1; then
	pass "produces a report"
else
	fail "produces a report: $(cat "$TMP/log")"
fi

check "instrumentation filter names exactly the components" \
	grep -qxF -- '--instrumentation_filter=^//(agent|cni)[/:],-^//(agent/test)[/:]' "$FAKE/calls"
check "library universe is the components less the excluded subtree" \
	grep -qxF -- 'kind("go_library", //agent/... + //cni/... - //agent/test/...)' "$FAKE/calls"
check "binary query is scoped to the components less the excluded subtree" \
	grep -qxF -- 'attr("srcs", "\.go", kind("go_binary", //agent/... + //cni/... - //agent/test/...))' "$FAKE/calls"
check "runs bazel coverage --config=coverage" \
	test "$(grep -A2 -xF -- '--- coverage' "$FAKE/calls" | tr '\n' ' ')" = '--- coverage coverage --config=coverage '
check "the caller's flags are passed through" grep -qxF -- '--some_flag' "$FAKE/calls"
printf '%s\n' '//agent/a:a' '//agent/a:a_test' '//agent/cmd/x:x' '//agent/cmd/x:x_lib' \
	'//agent/test/h:h_test' '//bazel/tool:tool_test' '//cni/p:p' '//cni/p:p_test' >"$TMP/want_targets"
check "targets = unit tests (the excluded subtree's too) + component libraries + binaries with srcs" \
	diff -u "$TMP/want_targets" "$FAKE/targets_seen"

for f in coverage.lcov coverage.xml tests-only.lcov summary.md unrecorded.txt; do
	check "writes $f" test -f "$OUT/$f"
done
check "generated proto Go is not in the report" lacks 'api/v1/x.pb.go' "$OUT/coverage.lcov"
check "a bazel-out source is not in the report" lacks 'bazel-out/' "$OUT/coverage.lcov"
check "an excluded-subtree file is not in the report" lacks 'agent/test/' "$OUT/coverage.lcov"
check "an excluded-subtree file is not in the tests-only tracefile" lacks 'agent/test/' "$OUT/tests-only.lcov"
check "what the excluded subtree's test covers in a component is kept" \
	test "$(grep -c '^SF:agent/a/a.go$' "$OUT/tests-only.lcov")" = 2
check "the log names the excluded subtree" has 'excluded subtrees: agent/test' "$TMP/log"
check "the zero-hit file is in the report" has 'SF:agent/cmd/x/main.go' "$OUT/coverage.lcov"
check "the zero-hit file is not in the tests-only tracefile" lacks 'agent/cmd/x/main.go' "$OUT/tests-only.lcov"
# a.go 2 lines (1 hit), main.go 3 lines (0), p.go 1 line (1): 6 lines, 2 covered.
check "Cobertura: 6 lines, 2 covered, stamped with the commit time" \
	has '<coverage line-rate="0.3333" branch-rate="0" lines-covered="2" lines-valid="6" branches-covered="0" branches-valid="0" complexity="0" version="aether-lcov-to-cobertura-1" timestamp="1700000000">' "$OUT/coverage.xml"
check "Cobertura names files repo-relative" has 'filename="agent/cmd/x/main.go"' "$OUT/coverage.xml"
# shellcheck disable=SC2016 # literal Markdown backticks, nothing to expand
check "summary: agent 20.00% with the zero-hit file, 50.00% without" \
	has '| `agent` | 2 | 5 | 1 | 20.00% | 2 | 1 | 50.00% |' "$OUT/summary.md"
check "summary: total" has '| **Total** | 3 | 6 | 2 | **33.33%** | 3 | 2 | 66.67% |' "$OUT/summary.md"
# agent/test/h/h.go and agent/test/h/gen/main.go are tracked, non-test and
# unrecorded, and must not be listed: they are not component code.
check "unrecorded.txt is the one tracked component file with no record" \
	test "$(cat "$OUT/unrecorded.txt")" = 'agent/a/a_windows.go'

# --- refusals -----------------------------------------------------------------
reset_fake
touch "$FAKE/query_fails"
refuse "a failed query" "bazel query failed" run --out "$TMP/r1"
check "a failed query runs no coverage" lacks '--- coverage' "$FAKE/calls"

reset_fake
: >"$FAKE/tests"
refuse "an empty test list" "the tests query returned nothing" run --out "$TMP/r2"
check "an empty test list runs no coverage" lacks '--- coverage' "$FAKE/calls"

reset_fake
: >"$FAKE/libraries"
refuse "an empty library list" "the libraries query returned nothing" run --out "$TMP/r3"

reset_fake
printf '%s\n' '//api/v1:v1' '//test/h:h' >"$FAKE/go_targets"
refuse "no component at all" "no first-party Go component found" run --out "$TMP/r4"

# A stale excluded subtree is refused, not silently skipped: the subtraction in
# the query would otherwise be an error nobody reads, or worse, nothing.
reset_fake
mv "$REPO/agent/test" "$REPO/agent/test.moved"
refuse "an excluded subtree that does not exist" "excluded subtree agent/test does not exist" run --out "$TMP/r4a"
check "a stale excluded subtree runs no coverage" lacks '--- coverage' "$FAKE/calls"
mv "$REPO/agent/test.moved" "$REPO/agent/test"
reset_fake
grep -v '^//agent/' "$FAKE/go_targets" >"$FAKE/go_targets.new"
mv "$FAKE/go_targets.new" "$FAKE/go_targets"
refuse "an excluded subtree outside every component" "excluded subtree agent/test is not under a component" run --out "$TMP/r4b"

reset_fake
touch "$FAKE/coverage_fails"
if run --out "$TMP/r5" >"$TMP/log" 2>&1; then
	fail "a failed bazel coverage: succeeded, wanted a failure"
else
	pass "a failed bazel coverage fails the report"
fi
check "a failed bazel coverage writes no XML" test ! -e "$TMP/r5/coverage.xml"

reset_fake
rm "$FAKE/out/_coverage/_coverage_report.dat"
refuse "a missing combined report" "wrote no combined report" run --out "$TMP/r6"

reset_fake
rm "$FAKE/testlogs/cni/p/p_test/coverage.dat"
refuse "a test that left no coverage.dat" "1 test(s) left no coverage.dat" run --out "$TMP/r7"
check "a short tests-only set writes no XML" test ! -e "$TMP/r7/coverage.xml"

refuse "an unknown argument" "usage:" run --bogus

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS check(s) failed"
	exit 1
fi
echo "all checks passed"
