#!/usr/bin/env bash
# What a promtool_rules_test (//bazel/promtool:defs.bzl) runs (#1492):
#
#   - `promtool check rules` on every rule file: it must parse as a rule file
#     and every expression in it must be valid PromQL;
#   - `promtool test rules` on every rule test: the alerts and series it
#     expects at each time must be what the rules produce from its input.
#
# Every file is run, and every failure is reported, before the script exits 1.
# Two things fail it besides promtool:
#
#   - no rule file at all: a glob that stopped matching must not be a passing
#     test;
#   - a .yml next to a file that was given, and not given itself: a rule test
#     that is there and was not run, or a rule file that was not checked. Under
#     Bazel the directory holds the test's data and nothing else, so this is
#     what catches a caller that lists a file as data and does not pass it.
#
# Usage: check_rules.sh --rules <rule file>... --tests <rule test file>...
# Env:   PROMTOOL                the promtool binary, or
#        PROMTOOL_RLOCATIONPATH  its place in the runfiles (set by the macro)

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

PROMTOOL="${PROMTOOL:-}"
if [ -z "$PROMTOOL" ]; then
	PROMTOOL="$(rlocation "${PROMTOOL_RLOCATIONPATH:?set by the BUILD target}")"
fi
if [ -z "$PROMTOOL" ] || [ ! -x "$PROMTOOL" ]; then
	echo "FAIL: promtool binary not found (PROMTOOL=${PROMTOOL}, PROMTOOL_RLOCATIONPATH=${PROMTOOL_RLOCATIONPATH:-})" >&2
	exit 2
fi

rules=()
tests=()
into=""
for arg in "$@"; do
	case "$arg" in
	--rules) into=rules ;;
	--tests) into=tests ;;
	*)
		case "$into" in
		rules) rules+=("$arg") ;;
		tests) tests+=("$arg") ;;
		*)
			echo "FAIL: usage: check_rules.sh --rules <file>... --tests <file>... (got '${arg}' before either)" >&2
			exit 2
			;;
		esac
		;;
	esac
done

if [ "${#rules[@]}" -eq 0 ]; then
	echo "FAIL: no rule file was given: nothing was checked" >&2
	exit 1
fi

FAILS=0
# run <what it is> <file> <promtool arguments...>
run() {
	local what="$1" file="$2" out
	shift 2
	if [ ! -f "$file" ]; then
		echo "FAIL  $what $file: no such file"
		FAILS=$((FAILS + 1))
		return
	fi
	if out="$("$PROMTOOL" "$@" "$file" 2>&1)"; then
		echo "PASS  $what $file"
	else
		echo "FAIL  $what $file"
		printf '%s\n' "$out" | sed 's/^/    /'
		FAILS=$((FAILS + 1))
	fi
}

for file in "${rules[@]}"; do
	run "check rules" "$file" check rules
done
for file in ${tests[@]+"${tests[@]}"}; do
	run "test rules" "$file" test rules
done

# Every .yml beside a given file was given. No associative array: macOS ships
# bash 3.2.
# A file given by its bare name is beside the files of the working directory:
# it is compared as ./<name>, which is how the scan of `.` spells it.
given=""
for file in "${rules[@]}" ${tests[@]+"${tests[@]}"}; do
	case "$file" in
	*/*) ;;
	*) file="./$file" ;;
	esac
	given="$given$file"$'\n'
done
dirs="$(printf '%s' "$given" | sed 's|/[^/]*$||' | sort -u)"
while IFS= read -r dir; do
	for file in "$dir"/*.yml; do
		[ -e "$file" ] || continue
		if ! grep -qxF -- "$file" <<<"$given"; then
			case "$file" in
			*_test.yml) echo "FAIL  $file is a rule test next to the files given, and was not run" ;;
			*) echo "FAIL  $file is next to the files given, and was not checked" ;;
			esac
			FAILS=$((FAILS + 1))
		fi
	done
done <<<"$dirs"

echo
if [ "$FAILS" -ne 0 ]; then
	echo "$FAILS failure(s) over $((${#rules[@]} + ${#tests[@]})) file(s) given"
	exit 1
fi
echo "promtool: ${#rules[@]} rule file(s) checked, ${#tests[@]} rule test file(s) run"
