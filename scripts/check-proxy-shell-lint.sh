#!/usr/bin/env bash
#
# Asserts that every shell script in the //proxy workspace is reachable by the
# root workspace's shellcheck aspect (#848).
#
# //proxy is listed in //.bazelignore, so root tooling cannot see into it and a
# label under it is never staged into an action sandbox. //bazel/lint/proxy_shell
# bridges the gap with source symlinks, one per script, mirroring the
# proxy-relative path. That bridge is only as good as its completeness: add
# `proxy/bazel/newthing.sh`, forget the symlink, and the script is silently
# unlinted again — exactly the state #848 exists to end.
#
# So this compares the two sets and fails on any difference in either direction:
#   * a proxy shell script with no symlink (uncovered);
#   * a symlink with no proxy script behind it (stale, or dangling);
#   * a symlink replaced by a regular file (a copy drifts; a symlink cannot).
#
# It deliberately does NOT run shellcheck — that is
# `bazel build --config=lint --@aspect_rules_lint//lint:fail_on_violation
# //bazel/lint/proxy_shell:all`, and `.github/workflows/proxy.yml` runs both.
#
# Usage: scripts/check-proxy-shell-lint.sh
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

mirror="bazel/lint/proxy_shell"

# A shell script is a `.sh` file or anything with a shell shebang —
# `proxy/bazel/get_workspace_status` has no extension and is very much shell.
is_shell() {
	case "$1" in
	*.sh) return 0 ;;
	esac
	head -1 "$1" | grep -Eq '^#!.*[/ ](bash|sh|dash|ksh|zsh)([[:space:]]|$)'
}

# The `ln -s` that would fix a missing entry, with the right number of `..`:
# three to climb out of bazel/lint/proxy_shell, plus one per directory the
# script sits under inside //proxy.
fix_command() {
	local rel="$1" dir up="../../.."
	dir="$(dirname "$rel")"
	if [ "$dir" != "." ]; then
		local depth i
		depth="$(printf '%s' "$dir" | tr -cd '/' | wc -c)"
		for ((i = 0; i <= depth; i++)); do up="../$up"; done
	fi
	printf 'mkdir -p %s/%s && ln -s %s/proxy/%s %s/%s' \
		"$mirror" "$dir" "$up" "$rel" "$mirror" "$rel"
}

status=0
expected=""

while IFS= read -r script; do
	is_shell "$script" || continue
	rel="${script#proxy/}"
	expected="${expected}${rel}"$'\n'
	link="$mirror/$rel"

	if [ ! -e "$link" ] && [ ! -L "$link" ]; then
		echo "uncovered: $script has no symlink at $link" >&2
		echo "           fix: $(fix_command "$rel")" >&2
		status=1
		continue
	fi
	if [ ! -L "$link" ]; then
		echo "not a symlink: $link must be a symlink to ../$script, not a copy" >&2
		echo "               a copy drifts from the original without anyone noticing" >&2
		status=1
		continue
	fi
	if [ "$(readlink -f "$link")" != "$(readlink -f "$script")" ]; then
		echo "wrong target: $link resolves to $(readlink -f "$link")" >&2
		echo "              expected $(readlink -f "$script")" >&2
		status=1
	fi
	# `--others --exclude-standard` as well as the index: a script that has been
	# written but not yet `git add`ed is exactly when a developer wants to be
	# told, and .gitignore keeps proxy/bazel-* build symlinks out.
done < <(git ls-files --cached --others --exclude-standard proxy)

# The other direction: nothing in the mirror that no longer exists in //proxy.
while IFS= read -r link; do
	rel="${link#"$mirror"/}"
	if ! printf '%s' "$expected" | grep -qxF "$rel"; then
		echo "stale: $link has no shell script at proxy/$rel — delete it" >&2
		status=1
	fi
done < <(find "$mirror" -type l | sort)

if [ "$status" -eq 0 ]; then
	count="$(printf '%s' "$expected" | grep -c . || true)"
	echo "OK: all $count //proxy shell script(s) are mirrored into //$mirror"
else
	echo >&2
	echo "//proxy shell is linted through //$mirror because proxy is in .bazelignore;" >&2
	echo "see that package's BUILD.bazel for why the ignore stays (#848)." >&2
fi
exit "$status"
