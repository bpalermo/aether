#!/usr/bin/env bash
#
# Asserts that every shell script in the ROOT workspace is reachable by the
# ShellCheck lint aspect (#853). (Capitalised deliberately: a comment whose
# first word is `shellcheck` is parsed as a shellcheck directive, and this file
# lints itself — SC1072/SC1073, caught by the very gate this script guards.)
#
# `lint_shellcheck_aspect` visits sh_binary / sh_library / sh_test and nothing
# else (rule_kinds in @aspect_rules_lint//lint:shellcheck.bzl), and then
# `filter_srcs` keeps only srcs that are SOURCE files in the main repo
# (lint/private/lint_aspect.bzl: `s.is_source and s.owner.workspace_name == ""`).
#
# Both halves matter, and the second is why #853 was easy to miss. This repo has
# always had one sh_binary — //:gazelle, from the gazelle macro — so a bare
# `kind(sh_..., //...)` query was never empty. But its only src is the GENERATED
# //:gazelle-runner, which filter_srcs discards, so the aspect received zero
# files. Until #848 declared the //proxy bridge, `--config=lint` ran shellcheck
# over nothing at all for this repository's entire life: `make lint` was green
# because it had no input, which is indistinguishable from green because the
# shell is clean. That is #853, and it is why this check counts SOURCE files the
# aspect would actually read rather than trusting that targets exist.
#
# The srcs are globs (//scripts, //bazel, //e2e, //e2e/pressure, //e2e/soak), so
# a script added to one of those directories is picked up with no edit. What a
# glob cannot do is cross a package boundary: a script added to a directory with
# no sh_* target — a new //e2e/foo, or one of //bazel's existing subpackages —
# is silently outside the gate, and nothing about `make lint` passing would say
# so. That is exactly the gap scripts/check-proxy-shell-lint.sh closes for the
# //proxy bridge, and this is its root-workspace sibling.
#
# So: enumerate the repository's shell from git, ask Bazel which files the sh_*
# targets actually claim, and fail on any file in the first set and not the
# second. It deliberately does NOT run shellcheck — that is
# `bazel build --config=lint --@aspect_rules_lint//lint:fail_on_violation //...`,
# and .github/workflows/ci.yaml's `shell` job runs both.
#
# //proxy and //bazel/lint/proxy_shell are excluded here: that tree is a
# separate bzlmod workspace reached through source symlinks, with its own target
# and its own guard (#848). This check owns the root workspace's own shell.
#
# Usage: scripts/check-shell-lint.sh        (or: make check-shell-lint)
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

bazel="${BAZEL:-bazel}"

# A shell script is a `.sh` file or anything with a shell shebang. Same test as
# check-proxy-shell-lint.sh: the aspect lints every src regardless of extension,
# so coverage has to be judged the same way.
is_shell() {
	case "$1" in
	*.sh) return 0 ;;
	esac
	head -1 "$1" 2>/dev/null | grep -Eq '^#!.*[/ ](bash|sh|dash|ksh|zsh)([[:space:]]|$)'
}

# `--others --exclude-standard` as well as the index: a script written but not
# yet `git add`ed is exactly when a developer wants to be told.
declared=""
while IFS= read -r f; do
	case "$f" in
	proxy/* | bazel/lint/proxy_shell/*) continue ;;
	esac
	# Symlinks are bridges into another workspace, never root shell of our own.
	[ -f "$f" ] && [ ! -L "$f" ] || continue
	is_shell "$f" || continue
	declared="${declared}${f}"$'\n'
done < <(git ls-files --cached --others --exclude-standard)

# What the build graph actually hands the aspect. Asking Bazel rather than
# re-deriving the globs is the point: this must fail when the TARGETS stop
# covering a file, not when a second copy of the glob list goes stale.
#
# `kind("source file", ...)` reproduces filter_srcs exactly. Without it the set
# would include //:gazelle-runner, a generated file the aspect discards — the
# difference between "a target lists it" and "shellcheck reads it", which is the
# distinction #853 turned on.
covered="$(
	"$bazel" query --noshow_progress --ui_event_filters=-info,-debug \
		'kind("source file", labels(srcs, kind("sh_(binary|library|test)", //...)))' 2>/dev/null |
		sed -e 's|^//:|/|' -e 's|^//||' -e 's|:|/|' |
		grep -v '^bazel/lint/proxy_shell/' | sort -u
)"

if [ -z "$covered" ]; then
	echo "no sh_* target hands the aspect a single source file — nothing is being linted" >&2
	echo "(the #853 state itself; also check the bazel query above actually ran)" >&2
	exit 1
fi

status=0
while IFS= read -r f; do
	[ -n "$f" ] || continue
	if ! printf '%s\n' "$covered" | grep -qxF "$f"; then
		echo "uncovered: $f is in no sh_* target, so the shellcheck aspect never sees it" >&2
		echo "           fix: add it to the sh_library in $(dirname "$f")/BUILD.bazel," >&2
		echo "                or create that package's sh_library if it has none" >&2
		status=1
	fi
done < <(printf '%s' "$declared")

if [ "$status" -eq 0 ]; then
	echo "OK: all $(printf '%s' "$declared" | grep -c .) root-workspace shell script(s) are covered by an sh_* target"
else
	echo >&2
	echo "srcs are per-package globs and a glob cannot cross a package boundary, so a" >&2
	echo "script in a directory with no sh_* target is silently unlinted (#853)." >&2
fi
exit "$status"
