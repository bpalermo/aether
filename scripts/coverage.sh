#!/usr/bin/env bash
# Line coverage of the unit suite over ALL first-party Go, as LCOV and as the
# Cobertura XML GitHub's code coverage takes. `make coverage` locally, the
# `report` job of .github/workflows/coverage.yaml in CI: one script, so the two
# cannot report different numbers. docs/runbook.md, "Code coverage".
#
#   scripts/coverage.sh [--out <dir>] [-- <bazel coverage flags>...]
#
# Writes into <dir> (default: ./coverage-report, git-ignored):
#   coverage.lcov     the combined report (what the XML is made from)
#   coverage.xml      Cobertura, for actions/upload-code-coverage
#   tests-only.lcov   the tests' own tracefiles, concatenated: the same run
#                     WITHOUT the zero-hit records, for the summary's comparison
#   summary.md        per-component table + total (scripts/coverage-summary.sh)
#   unrecorded.txt    first-party non-test .go files with no record (see below)
#
# What is measured, and why each choice:
#
# Tests: every go_test not tagged integration, requires-root or manual. That is
# the unit leg of `ci` (scripts/ci-impacted-targets.sh: all tests minus
# `integration` minus //test/e2e, which is `manual`), less the one
# requires-root test, which skips as a normal user. ALWAYS the whole set, never
# bazel-diff's impacted subset: a percentage over a different set of tests per
# pull request cannot be compared with main's. Bazel caches each test's
# coverage result, so locally an unchanged test is a cache hit, not a re-run
# (in CI only the compiles are: docs/runbook.md, "Code coverage").
#
# Components: every top-level directory that has a go_library or go_binary,
# minus EXCLUDED below. Derived by query, so a new top-level component is
# measured the day it gets its first Go target, with no edit here.
#
# Minus EXCLUDED_SUBTREES too: test harnesses that have to live INSIDE a
# component. //agent/test (the `envoy --mode validate` config builders, their
# generator, the pinned-Envoy locator) imports agent/internal/..., and Go's
# internal-package rule only allows that from under agent/ (#1311); until then
# it was //test, excluded above. It is still not product code: nothing in it is
# linked into a shipped binary, and counting it would move the agent's
# percentage with the size of a test fixture. Its go_test targets run like any
# other unit test, and what they exercise in agent/internal/... counts.
#
# The denominator: `bazel coverage` instruments, by default, only the packages
# that have a test in the run, so code nobody tests is not in the report at
# all and the percentage flatters. Two things fix that:
#   1. --instrumentation_filter names every component, so a package linked into
#      another package's test is instrumented too;
#   2. every component go_library is itself a target of the invocation. rules_go
#      (>= 0.64, Bazel >= 9) emits a baseline tracefile per library: every
#      coverable line at zero hits, from `go tool cover` on the sources the
#      compile action would compile, i.e. after build constraints (a
#      _windows.go file is absent, not uncovered). Bazel merges those into the
#      combined report, so a file linked into no test binary counts as zero
#      with the same line set a measured run would give it.
# A go_binary is covered through the go_library it embeds; one that lists .go
# srcs of its own is added as a target for the same reason.
#
# Not instrumented: _test.go files (Bazel's default, --noinstrument_test_targets)
# and everything outside the components, which the filter excludes by
# construction: generated proto Go (//api), test harnesses (//test, //e2e,
# //agent/test) and build tooling (//bazel).
#
# unrecorded.txt lists tracked non-test .go files under a component that the
# report does not name at all. The only expected members are files excluded on
# this platform by build constraints (three on linux/amd64 when this was
# written: two _windows/_unspecified files in common/file and
# agent/internal/udscsi/mounter_other.go). A file with no executable statement
# (constants, type declarations) IS named, with zero lines, so it is not listed.
# Anything else in the list is a file no go_library has in its srcs: run
# `make gazelle`.
set -euo pipefail

# Top-level directories whose Go is not product code. Everything else that has
# a Go target is a component.
EXCLUDED='api bazel e2e test'
# Directories below a component whose Go is not product code either. Each must
# exist (a stale entry fails the run) and must sit under a component.
EXCLUDED_SUBTREES='agent/test'

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

bazel="${BAZEL:-bazel}"
out="coverage-report"
bazel_flags=()
while [ "$#" -gt 0 ]; do
	case "$1" in
	--out)
		[ "$#" -ge 2 ] || {
			echo "coverage: --out needs a directory" >&2
			exit 2
		}
		out="$2"
		shift 2
		;;
	--)
		shift
		bazel_flags=("$@")
		break
		;;
	*)
		echo "usage: $0 [--out <dir>] [-- <bazel coverage flags>...]" >&2
		exit 2
		;;
	esac
done
mkdir -p "$out"
out="$(cd "$out" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# A query whose stderr is discarded and whose failure is not checked turns a
# broken BUILD file into an empty list, and an empty list into a green report
# over nothing. Every query goes through here.
query() { # outfile expression
	if ! "$bazel" query --noshow_progress "$2" >"$1" 2>"$work/query.err"; then
		cat "$work/query.err" >&2
		echo "coverage: bazel query failed: $2" >&2
		exit 1
	fi
}

# --- components -------------------------------------------------------------
query "$work/go_targets" 'kind("go_library|go_binary", //...)'
# A target in the root package (//:name) belongs to no directory and is skipped.
sed -n 's|^//\([^/:][^/:]*\)[/:].*|\1|p' "$work/go_targets" | sort -u >"$work/all_dirs"
: >"$work/components"
while IFS= read -r dir; do
	case " $EXCLUDED " in
	*" $dir "*) continue ;;
	esac
	printf '%s\n' "$dir" >>"$work/components"
done <"$work/all_dirs"
if [ ! -s "$work/components" ]; then
	echo "coverage: no first-party Go component found (go_library/go_binary query is empty)" >&2
	exit 1
fi
component_re="$(paste -sd '|' "$work/components")"
component_universe="$(sed 's|.*|//&/...|' "$work/components" | paste -sd '+' | sed 's|+| + |g')"
: >"$work/subtrees"
for sub in $EXCLUDED_SUBTREES; do
	if [ ! -d "$sub" ]; then
		echo "coverage: excluded subtree $sub does not exist: drop it from EXCLUDED_SUBTREES" >&2
		exit 1
	fi
	if ! grep -qxF -- "${sub%%/*}" "$work/components"; then
		echo "coverage: excluded subtree $sub is not under a component (components: $(paste -sd ' ' "$work/components")): drop it from EXCLUDED_SUBTREES" >&2
		exit 1
	fi
	printf '%s\n' "$sub" >>"$work/subtrees"
done
filter="^//(${component_re})[/:]"
# Never matches a path: the neutral value when there is no excluded subtree.
subtree_re='^$'
if [ -s "$work/subtrees" ]; then
	subtree_alt="$(paste -sd '|' "$work/subtrees")"
	subtree_re="^([.]/)?(${subtree_alt})/"
	# A leading '-' is Bazel's syntax for an exclusion in --instrumentation_filter.
	filter="${filter},-^//(${subtree_alt})[/:]"
	# `+` and `-` associate left with equal precedence: (a + b) - c - d.
	component_universe="${component_universe}$(sed 's|.*| - //&/...|' "$work/subtrees" | tr -d '\n')"
fi
echo ">> components: $(paste -sd ' ' "$work/components") (excluded: $EXCLUDED; excluded subtrees: ${EXCLUDED_SUBTREES:-none})"

# --- targets ----------------------------------------------------------------
query "$work/tests" 'kind("go_test", //...) except attr("tags", "[\[ ](integration|requires-root|manual)[,\]]", //...)'
query "$work/libraries" "kind(\"go_library\", $component_universe)"
query "$work/binaries" "attr(\"srcs\", \"\\.go\", kind(\"go_binary\", $component_universe))"
for list in tests libraries; do
	if [ ! -s "$work/$list" ]; then
		echo "coverage: the $list query returned nothing: refusing to report coverage of an empty set" >&2
		exit 1
	fi
done
sort -u "$work/tests" "$work/libraries" "$work/binaries" >"$work/targets"
echo ">> targets: $(wc -l <"$work/tests") unit go_test, $(wc -l <"$work/libraries") go_library, $(wc -l <"$work/binaries") go_binary with srcs of its own"

# --- run --------------------------------------------------------------------
# A failing test fails the report: coverage of a suite that did not pass is not
# the number main's is compared with.
"$bazel" coverage --config=coverage \
	"--instrumentation_filter=${filter}" \
	${bazel_flags[@]+"${bazel_flags[@]}"} \
	--target_pattern_file="$work/targets"

output_path="$("$bazel" info output_path 2>/dev/null)"
testlogs="$("$bazel" info bazel-testlogs 2>/dev/null)"
combined="$output_path/_coverage/_coverage_report.dat"
if [ ! -s "$combined" ]; then
	echo "coverage: bazel coverage succeeded but wrote no combined report at $combined" >&2
	exit 1
fi

# --- collect ----------------------------------------------------------------
# Keep only component records, less the excluded subtrees. The instrumentation
# filter already limits the report to them; this makes that a property of the
# output, not of a flag.
keep_components() { # lcov -> stdout
	awk -v re="^([.]/)?(${component_re})/" -v drop="$subtree_re" '
		/^SF:/ { keep = (substr($0, 4) ~ re && substr($0, 4) !~ drop) }
		keep { print }
		/^end_of_record$/ { keep = 0 }
	' "$1"
}
keep_components "$combined" >"$out/coverage.lcov"

# The tests' own tracefiles, by target, not by `find`: bazel-testlogs keeps the
# coverage.dat of every test ever run in this output base.
: >"$work/tests_only"
missing=0
while IFS= read -r label; do
	path="${label#//}"
	dat="$testlogs/${path/://}/coverage.dat"
	if [ -f "$dat" ]; then
		cat "$dat" >>"$work/tests_only"
	else
		echo "coverage: no coverage.dat for $label ($dat)" >&2
		missing=$((missing + 1))
	fi
done <"$work/tests"
if [ "$missing" -ne 0 ]; then
	echo "coverage: $missing test(s) left no coverage.dat: the tests-only comparison would be short" >&2
	exit 1
fi
keep_components "$work/tests_only" >"$out/tests-only.lcov"

# --- convert + summarise ----------------------------------------------------
# The commit time, so the XML is a function of the commit and the measurements.
timestamp="$(git log -1 --format=%ct 2>/dev/null || echo 0)"
scripts/lcov-to-cobertura.sh --timestamp "$timestamp" "$out/coverage.lcov" >"$out/coverage.xml"
scripts/coverage-summary.sh "$out/coverage.lcov" "$out/tests-only.lcov" >"$out/summary.md"

sed -n 's/^SF://p' "$out/coverage.lcov" | sed 's|^\./||' | sort -u >"$work/recorded"
git ls-files -- '*.go' | grep -E "^(${component_re})/" | grep -Ev -- "$subtree_re" | grep -v '_test\.go$' | sort -u >"$work/tracked" || true
comm -23 "$work/tracked" "$work/recorded" >"$out/unrecorded.txt"

cat "$out/summary.md"
echo ">> $(wc -l <"$out/unrecorded.txt") tracked non-test .go file(s) are not in the report (expected: only files excluded by build constraints): $out/unrecorded.txt"
echo ">> wrote $out/coverage.xml ($(wc -c <"$out/coverage.xml") bytes), $out/coverage.lcov, $out/summary.md"
