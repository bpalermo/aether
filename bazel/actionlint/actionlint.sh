#!/usr/bin/env bash
# actionlint over every GitHub Actions workflow in .github/workflows, with the
# repository's own ShellCheck (the binary the lint aspects run, so a `run:`
# block and a scripts/*.sh file are held to the same ShellCheck version).
#
#   bazel run //bazel/actionlint            # lint the working tree (make actionlint)
#   bazel run //bazel/actionlint -- <file>  # lint just these workflow files
#   bazel test //bazel/actionlint:actionlint_test   # the CI gate (ci.yaml `actionlint`)
#
# actionlint is the official release binary MODULE.bazel pins (http_archive per
# platform, sha256 from the release's checksums file). EXPECTED below must move
# with a bump, so a new actionlint (new checks, new findings) never arrives
# unnoticed. Configuration: .github/actionlint.yaml.
#
# What actionlint does NOT cover: the `run:` blocks of the composite actions in
# .github/actions/*/action.yml (it lints workflow files only). Those are short
# and pass ShellCheck by hand; a composite's `run:` reaches a workflow only
# through its inputs, which are passed through `env:` (never `${{ }}` in the
# script), and that convention is what to review there.
#
# Exit status is actionlint's: 0 clean, 1 findings, >1 the check could not run.

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
set -e
# --- end runfiles.bash initialization v3 ---

EXPECTED="1.7.12"

actionlint="$(rlocation "${ACTIONLINT_RLOCATIONPATH:?set by the BUILD target}")"
shellcheck="$(rlocation "${SHELLCHECK_RLOCATIONPATH:?set by the BUILD target}")"

got="$("$actionlint" -version | head -1)"
if [ "$got" != "$EXPECTED" ]; then
	echo "FAIL: actionlint is '${got}', expected '${EXPECTED}' (the archives in MODULE.bazel moved: update EXPECTED, and fix or explain any new findings)" >&2
	exit 2
fi

# `bazel run` lints the caller's tree; `bazel test` lints the runfiles copy of
# //:ci_definitions (the workflows, the composite actions) and the config.
if [ -n "${BUILD_WORKSPACE_DIRECTORY:-}" ]; then
	cd "$BUILD_WORKSPACE_DIRECTORY"
else
	# actionlint resolves `uses: ./.github/actions/<x>` (and checks the inputs a
	# workflow passes against the action's metadata) only inside a project,
	# which it recognises by a .git directory. The runfiles tree has none, so
	# lint a copy that does — otherwise those checks pass by not running.
	root="${TEST_TMPDIR:?}/repo"
	mkdir -p "$root/.git"
	cp -RL "${TEST_SRCDIR:?}/_main/.github" "$root/"
	cd "$root"
fi

files=("$@")
if [ "${#files[@]}" -eq 0 ]; then
	shopt -s nullglob
	files=(.github/workflows/*.yaml .github/workflows/*.yml)
	shopt -u nullglob
fi
if [ "${#files[@]}" -eq 0 ]; then
	echo "FAIL: no workflow files under $(pwd)/.github/workflows — refusing to pass a lint that looked at nothing (#853)" >&2
	exit 2
fi

echo "actionlint ${got} (shellcheck $("$shellcheck" --version | sed -n 's/^version: //p')): ${#files[@]} workflow file(s)"
# -pyflakes= : no workflow runs Python inline, and the host's pyflakes (or its
# absence) must not change the result.
"$actionlint" \
	-config-file .github/actionlint.yaml \
	-shellcheck "$shellcheck" \
	-pyflakes= \
	-color=false \
	"${files[@]}"
rc=$?
if [ "$rc" -eq 0 ]; then echo "actionlint: clean"; fi
exit "$rc"
