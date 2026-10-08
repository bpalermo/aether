#!/usr/bin/env bash
# Computes the Bazel targets impacted by a revision range with Tinder/bazel-diff
# and splits the impacted TEST targets into unit / integration / e2e so CI builds
# and tests only what changed. Falls back to "everything impacted" on any error or
# when the base revision is unavailable, so CI never silently under-tests. When
# even the full run cannot list the tests (a `bazel query` that fails or returns
# nothing), it exits non-zero and sets no has_* output: the caller's step fails,
# and an empty list never stands in for "nothing to test" (#1437).
#
# The range is whatever the caller passes; nothing here assumes a PR. Two callers:
#   .github/workflows/ci.yaml   BASE_SHA = the PR's merge-base, HEAD_SHA = PR head
#   .github/workflows/main.yaml BASE_SHA = HEAD^ (a merge commit's first parent),
#                               HEAD_SHA = the merge commit — i.e. post-merge
#                               validation of what a merge added to main (#679)
#
# Runs bazel-diff at both the base and head revisions (it git-checkouts them), so
# the workflow MUST run this from a copy outside the repo tree (e.g. $RUNNER_TEMP)
# — otherwise the checkout could swap the script file out from under bash.
#
# Env:
#   BASE_SHA        base commit to diff against (empty -> full fallback)
#   HEAD_SHA        head commit to diff (default: current HEAD)
#   BAZEL_DIFF_JAR  path to bazel-diff_deploy.jar (missing -> full fallback)
#   OUT_DIR         output dir for the target lists (default: $PWD/.bazel-diff-out)
#   GITHUB_OUTPUT   if set, has_any/has_unit/has_integration/has_e2e are appended
#
# Outputs in OUT_DIR: impacted_build.txt, impacted_unit.txt, impacted_integration.txt
set -uo pipefail

BAZEL="$(command -v bazelisk || command -v bazel)"
HEAD_SHA="${HEAD_SHA:-$(git rev-parse HEAD)}"
OUT_DIR="${OUT_DIR:-$PWD/.bazel-diff-out}"
mkdir -p "$OUT_DIR"

E2E_TARGET="//test/e2e:e2e_test"

emit() { # name value
	[ -n "${GITHUB_OUTPUT:-}" ] && echo "$1=$2" >>"$GITHUB_OUTPUT"
	echo "  output: $1=$2"
}

die() { # message: what to test could not be determined, and nothing wider is left
	echo "::error::ci-impacted-targets: $1" >&2
	exit 1
}

# What CI may build and test: nothing tagged `manual` (#1413 tests, #1438 build).
# The lists below reach `bazel build` / `bazel test` as explicit labels
# (--target_pattern_file), and Bazel honours `manual` only when expanding a
# wildcard, so a `manual` target has to leave the candidate set here or it is
# built and run. `attr` matches a regex against the tag list printed as
# `[a, b, c]`: the brackets pin a whole tag, where a bare `manual` or
# `\bmanual\b` would also take `no-manual-x`. Same form as scripts/coverage.sh.
# shellcheck disable=SC2016 # a Bazel query expression, not a shell expansion
MANUAL='attr("tags", "[\[ ]manual[,\]]", //...)'
RUNNABLE_TESTS="tests(//...) except ${MANUAL}"
BUILDABLE_RULES="kind(rule, //...) except ${MANUAL}"
# `integration` whole, in the same bracket form (#1439): `no-integration` or
# `integration-slow` is not this tag.
INTEGRATION_TESTS="attr(\"tags\", \"[\\[ ]integration[,\\]]\", ${RUNNABLE_TESTS})"

# query <outfile> <expression>: the sorted unique labels, or status 1.
#
# A query that fails, and one that succeeds with nothing, both return 1 (#1437).
# Each of the three lists asked for here is certain to be non-empty in this
# repository (it has rules, tests and integration tests), so an empty answer is
# a broken query (a regex that matches nothing still exits 0) and never
# "nothing to test". Left unchecked, either one emptied the candidate list:
# every has_* came out false, the test legs skipped and `ci` was green over
# nothing. Bazel's stderr is kept and printed when the query is refused.
query() {
	if ! "$BAZEL" query "$2" --noshow_progress >"$1.raw" 2>"$1.err"; then
		cat "$1.err" >&2
		echo ">> bazel query failed: $2" >&2
		return 1
	fi
	# No list from an earlier run into the same OUT_DIR, and no partial one: a
	# `sort` or a write that fails is a failed query too.
	rm -f "$1"
	if ! sort -u "$1.raw" >"$1.sorted"; then
		echo ">> could not sort the answer of: $2" >&2
		return 1
	fi
	grep -v '^[[:space:]]*$' "$1.sorted" >"$1"
	if [ "$?" -gt 1 ] || [ ! -s "$1" ]; then
		cat "$1.err" >&2
		echo ">> bazel query returned nothing: $2" >&2
		return 1
	fi
}
# test_lists: all_tests.txt, integration_tests.txt and what is left for the unit
# leg, all_unit.txt. That one is certain to be non-empty too: every test coming
# back as integration is a query that matched everything.
test_lists() {
	query "$OUT_DIR/all_tests.txt" "$RUNNABLE_TESTS" || return 1
	query "$OUT_DIR/integration_tests.txt" "$INTEGRATION_TESTS" || return 1
	unit "$OUT_DIR/all_tests.txt" "$OUT_DIR/integration_tests.txt" >"$OUT_DIR/all_unit.txt" || return 1
	if [ ! -s "$OUT_DIR/all_unit.txt" ]; then
		echo ">> every test came back tagged integration: ${INTEGRATION_TESTS}" >&2
		return 1
	fi
}
# keep <labels> <candidates>: the candidates that are in <labels>. grep exits 1
# when there are none, which is an answer; above 1 it could not read a file,
# which is not.
keep() {
	grep -Fxf "$1" "$2"
	[ "$?" -le 1 ]
}
# unit <tests> <integration>: tests - integration - e2e, on stdout.
unit() {
	comm -23 "$1" "$2" | {
		grep -vxF "$E2E_TARGET"
		[ "$?" -le 1 ]
	}
}

# full_run: mark everything impacted (build //..., run every test) and exit 0.
# The answer to every "the impacted set could not be computed" below. It needs
# the two test lists itself, to split unit from integration. When it cannot get
# them there is nothing wider to fall back to, so it fails the step.
full_run() {
	echo ">> bazel-diff fallback (full run): $1" >&2
	test_lists || die "the full run could not list the tests (see the query above); refusing to report an empty test set"
	echo '//...' >"$OUT_DIR/impacted_build.txt"
	cp "$OUT_DIR/integration_tests.txt" "$OUT_DIR/impacted_integration.txt" || die "could not write impacted_integration.txt"
	cp "$OUT_DIR/all_unit.txt" "$OUT_DIR/impacted_unit.txt" || die "could not write impacted_unit.txt"
	emit has_any true
	emit has_unit true
	emit has_integration true
	emit has_e2e true
	exit 0
}

[ -n "${BAZEL_DIFF_JAR:-}" ] && [ -f "${BAZEL_DIFF_JAR:-}" ] || full_run "no bazel-diff jar"
[ -n "${BASE_SHA:-}" ] || full_run "no BASE_SHA"
git cat-file -e "${BASE_SHA}^{commit}" 2>/dev/null || full_run "BASE_SHA ${BASE_SHA} unreachable"

# Seed files: changing any of these marks all targets impacted (full run). These
# affect the build/CI but are not part of the Bazel build graph that bazel-diff
# hashes, so they must be declared explicitly.
SEED="$OUT_DIR/seed.txt"
cat >"$SEED" <<'EOF'
.bazelrc
.github/workflows/ci.yaml
.github/workflows/main.yaml
scripts/ci-impacted-targets.sh
MODULE.bazel
MODULE.bazel.lock
bazel/registry/MODULE.bazel
bazel/registry/BUILD.bazel
bazel/registry/registry.bzl
EOF

generate() { # ref outfile
	git checkout -q --detach "$1" || return 1
	# bazel-diff errors on a seed path that doesn't exist at the checked-out rev,
	# so pass only the seed files present here. A seed file present in one rev but
	# not the other still flips every target's hash (-> full run), which is the
	# intended behavior for a newly added/removed infra file.
	local rev_seed="$SEED.rev"
	: >"$rev_seed"
	while IFS= read -r p; do [ -e "$p" ] && printf '%s\n' "$p" >>"$rev_seed"; done <"$SEED"
	java -jar "$BAZEL_DIFF_JAR" generate-hashes -w "$PWD" -b "$BAZEL" -s "$rev_seed" "$2"
}

# Where the checkout is now. The callers check out HEAD_SHA before they run this,
# so the full run (which queries here) and the bazel-diff path (which queries at
# HEAD_SHA) list the same commit's targets.
start_commit="$(git rev-parse HEAD)" || die "git rev-parse HEAD failed"
orig_ref="$(git rev-parse --abbrev-ref HEAD)"
[ "$orig_ref" = "HEAD" ] && orig_ref="$start_commit"
# restore: back to where the run started, or fail. From a checkout left on the
# base revision the full run would list the BASE's tests: a test the change adds
# would be in no list, and nothing would say so.
restore() {
	git checkout -q "$orig_ref" 2>/dev/null || git checkout -q --detach "$start_commit" 2>/dev/null
	[ "$(git rev-parse HEAD 2>/dev/null)" = "$start_commit" ] ||
		die "could not check out ${orig_ref} again after bazel-diff; the checkout is not at the commit under test"
}

# The `sort` is part of the condition: a get-impacted-targets that exits 0
# without writing its list is a failure too, not an empty impact set. And a list
# an earlier run left in OUT_DIR is not this run's answer.
rm -f "$OUT_DIR/impacted.txt" "$OUT_DIR/impacted.sorted"
if ! { generate "$BASE_SHA" "$OUT_DIR/base.json" &&
	generate "$HEAD_SHA" "$OUT_DIR/head.json" &&
	java -jar "$BAZEL_DIFF_JAR" get-impacted-targets -w "$PWD" -b "$BAZEL" \
		-sh "$OUT_DIR/base.json" -fh "$OUT_DIR/head.json" -o "$OUT_DIR/impacted.txt" &&
	sort -u "$OUT_DIR/impacted.txt" >"$OUT_DIR/impacted.sorted"; }; then
	restore
	full_run "bazel-diff command failed"
fi
# We are at HEAD_SHA here (last generate); classification queries run against it.
# A query that fails takes the same fallback as a failed bazel-diff. The rule
# list is not needed there (the full run builds //...). The test lists are, and
# the full run asks for them again: a transient failure costs a full run, a
# persistent one fails the step.
if ! { query "$OUT_DIR/all_rules.txt" "$BUILDABLE_RULES" && test_lists; }; then
	restore
	full_run "a classification query failed or returned nothing"
fi
restore

# The e2e target is `manual`, so it is in none of the lists above: its leg names
# it itself, and has_e2e is read from what bazel-diff reported.
e2e=false
grep -qxF "$E2E_TARGET" "$OUT_DIR/impacted.sorted" && e2e=true

# impacted build set = impacted ∩ buildable rule targets (drops source and
# generated-file labels, and `manual` rules), plus the e2e target when it is
# impacted. That is the one `manual` target CI means to build: the `e2e` leg
# needs the `test` leg, which is skipped on an empty build list (has_any), and
# main.yaml has no e2e leg, so this is where a change to that test is compiled
# and linted on main.
keep "$OUT_DIR/impacted.sorted" "$OUT_DIR/all_rules.txt" >"$OUT_DIR/impacted_build.txt" || die "could not compute impacted_build.txt"
if [ "$e2e" = true ]; then
	echo "$E2E_TARGET" >>"$OUT_DIR/impacted_build.txt" || die "could not add the e2e target to impacted_build.txt"
	sort -u -o "$OUT_DIR/impacted_build.txt" "$OUT_DIR/impacted_build.txt" || die "could not sort impacted_build.txt"
fi
# impacted tests, split by tag.
keep "$OUT_DIR/impacted.sorted" "$OUT_DIR/all_tests.txt" >"$OUT_DIR/impacted_tests.txt" || die "could not compute impacted_tests.txt"
keep "$OUT_DIR/integration_tests.txt" "$OUT_DIR/impacted_tests.txt" >"$OUT_DIR/impacted_integration.txt" || die "could not compute impacted_integration.txt"
unit "$OUT_DIR/impacted_tests.txt" "$OUT_DIR/impacted_integration.txt" >"$OUT_DIR/impacted_unit.txt" || die "could not compute impacted_unit.txt"

nonempty() { [ -s "$1" ] && echo true || echo false; }

emit has_any "$(nonempty "$OUT_DIR/impacted_build.txt")"
emit has_unit "$(nonempty "$OUT_DIR/impacted_unit.txt")"
emit has_integration "$(nonempty "$OUT_DIR/impacted_integration.txt")"
emit has_e2e "$e2e"

echo ">> impacted: build=$(wc -l <"$OUT_DIR/impacted_build.txt") unit=$(wc -l <"$OUT_DIR/impacted_unit.txt") integration=$(wc -l <"$OUT_DIR/impacted_integration.txt") e2e=${e2e}"
