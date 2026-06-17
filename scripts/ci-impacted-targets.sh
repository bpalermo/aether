#!/usr/bin/env bash
# Computes the Bazel targets impacted by a PR with Tinder/bazel-diff and splits
# the impacted TEST targets into unit / integration / e2e so CI builds and tests
# only what changed. Falls back to "everything impacted" on any error or when the
# base revision is unavailable, so CI never silently under-tests.
#
# Runs bazel-diff at both the base and head revisions (it git-checkouts them), so
# the workflow MUST run this from a copy outside the repo tree (e.g. $RUNNER_TEMP)
# — otherwise the checkout could swap the script file out from under bash.
#
# Env:
#   BASE_SHA        merge-base commit to diff against (empty -> full fallback)
#   HEAD_SHA        PR head commit (default: current HEAD)
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

# bazel query helpers (sorted unique label lists).
all_tests() { "$BAZEL" query 'tests(//...)' 2>/dev/null | sort -u; }
integration_tests() { "$BAZEL" query 'attr(tags, "integration", tests(//...))' 2>/dev/null | sort -u; }
all_rules() { "$BAZEL" query 'kind(rule, //...)' 2>/dev/null | sort -u; }

# full_run: mark everything impacted (build //..., run every test) and exit 0.
full_run() {
	echo ">> bazel-diff fallback (full run): $1" >&2
	echo '//...' >"$OUT_DIR/impacted_build.txt"
	integration_tests >"$OUT_DIR/impacted_integration.txt"
	# unit = all tests - integration - e2e
	comm -23 <(all_tests) <(integration_tests) | grep -vxF "$E2E_TARGET" >"$OUT_DIR/impacted_unit.txt"
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
scripts/ci-impacted-targets.sh
MODULE.bazel
MODULE.bazel.lock
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

orig_ref="$(git rev-parse --abbrev-ref HEAD)"
[ "$orig_ref" = "HEAD" ] && orig_ref="$HEAD_SHA"
restore() { git checkout -q "$orig_ref" 2>/dev/null || git checkout -q --detach "$HEAD_SHA" 2>/dev/null || true; }

if ! { generate "$BASE_SHA" "$OUT_DIR/base.json" &&
	generate "$HEAD_SHA" "$OUT_DIR/head.json" &&
	java -jar "$BAZEL_DIFF_JAR" get-impacted-targets -w "$PWD" -b "$BAZEL" \
		-sh "$OUT_DIR/base.json" -fh "$OUT_DIR/head.json" -o "$OUT_DIR/impacted.txt"; }; then
	restore
	full_run "bazel-diff command failed"
fi
# We are at HEAD_SHA here (last generate); classification queries run against it.

sort -u "$OUT_DIR/impacted.txt" >"$OUT_DIR/impacted.sorted"

# impacted build set = impacted ∩ rule targets (drop source/generated-file labels).
all_rules >"$OUT_DIR/all_rules.txt"
grep -Fxf "$OUT_DIR/impacted.sorted" "$OUT_DIR/all_rules.txt" >"$OUT_DIR/impacted_build.txt" || true
# impacted tests, split by tag.
all_tests >"$OUT_DIR/all_tests.txt"
integration_tests >"$OUT_DIR/integration_tests.txt"
grep -Fxf "$OUT_DIR/impacted.sorted" "$OUT_DIR/all_tests.txt" | sort -u >"$OUT_DIR/impacted_tests.txt" || true
grep -Fxf "$OUT_DIR/integration_tests.txt" "$OUT_DIR/impacted_tests.txt" | sort -u >"$OUT_DIR/impacted_integration.txt" || true
comm -23 "$OUT_DIR/impacted_tests.txt" "$OUT_DIR/impacted_integration.txt" | grep -vxF "$E2E_TARGET" >"$OUT_DIR/impacted_unit.txt" || true

restore

nonempty() { [ -s "$1" ] && echo true || echo false; }
e2e=false
grep -qxF "$E2E_TARGET" "$OUT_DIR/impacted_tests.txt" && e2e=true

emit has_any "$(nonempty "$OUT_DIR/impacted_build.txt")"
emit has_unit "$(nonempty "$OUT_DIR/impacted_unit.txt")"
emit has_integration "$(nonempty "$OUT_DIR/impacted_integration.txt")"
emit has_e2e "$e2e"

echo ">> impacted: build=$(wc -l <"$OUT_DIR/impacted_build.txt") unit=$(wc -l <"$OUT_DIR/impacted_unit.txt") integration=$(wc -l <"$OUT_DIR/impacted_integration.txt") e2e=${e2e}"
