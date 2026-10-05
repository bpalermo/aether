#!/usr/bin/env bash
#
# Lint the //proxy workspace's shell from the repository root: ShellCheck over
# every shell script under proxy/ (`*.sh`, or a shell shebang:
# `proxy/bazel/get_workspace_status` has no extension).
#
# proxy/ Starlark is NOT linted here any more: buildifier's linter runs inside
# the root formatter's check (`make format-check`, bazel/format/BUILD.bazel,
# #1245), which already reaches proxy/ through `git ls-files` and uses the one
# warning set the rest of the repository is held to.
#
# Why not the lint aspects (`make lint`, //bazel/lint:linters.bzl): proxy/ is
# in //.bazelignore, so no root target can name a file under it and no aspect
# ever visits one. Until this script, ShellCheck reached proxy/ through a
# symlink bridge (//bazel/lint/proxy_shell, #848) plus a guard that the bridge
# was complete, and nothing linted proxy Starlark at all. Running the SAME
# binary the aspect uses (rules_lint's ShellCheck) on the files themselves
# needs neither: the file set is `git ls-files`, so a new proxy script is
# covered the moment it is tracked.
#
# Formatting is not this script's job: `make format-check` (//:format) covers
# proxy/ directly (see .gitattributes).
#
# Exit 0 clean, 1 on a finding, 2 when the check itself cannot run — including
# finding no shell files: a gate that looks at nothing proves nothing (#853).
#
# Usage: scripts/lint-proxy.sh   (from anywhere; Bazel runs from the repo root)
set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" || exit 2
cd "$repo_root" || exit 2

is_shell() {
	case "$1" in
	*.sh) return 0 ;;
	esac
	head -1 "$1" | grep -Eq '^#!.*[/ ](bash|sh|dash|ksh|zsh)([[:space:]]|$)'
}

shell=()
while IFS= read -r f; do
	[ -f "$f" ] || continue
	if is_shell "$f"; then shell+=("$f"); fi
done < <(git ls-files proxy)

if [ "${#shell[@]}" -eq 0 ]; then
	echo "::error::lint-proxy: found no shell file under proxy/ — refusing to pass a lint that looked at nothing" >&2
	exit 2
fi

run_quiet() {
	bazel run --ui_event_filters=-info,-stdout,-stderr --noshow_progress "$@"
}

fail=0

# ShellCheck runs from the runfiles tree: hand it absolute paths, and the same
# rcfile the aspect passes.
echo "shellcheck: ${#shell[@]} shell file(s) under proxy/"
shell_abs=()
for f in "${shell[@]}"; do shell_abs+=("${repo_root}/${f}"); done
run_quiet @aspect_rules_lint//lint:shellcheck_bin -- --rcfile "${repo_root}/.shellcheckrc" "${shell_abs[@]}"
case $? in
0) ;;
*) fail=1 ;;
esac

if [ "$fail" -ne 0 ]; then
	echo "::error::lint-proxy: ShellCheck findings above" >&2
	exit 1
fi
echo "lint-proxy: clean"
