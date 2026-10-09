#!/usr/bin/env bash
# The promtool //bazel/promtool runs is the version MODULE.bazel pins.
#
# Network-free: the binary is a repository artifact, checksummed by its
# http_archive. A bump of those archives fails here until EXPECTED below moves
# with them, so a new promtool (whose PromQL parser and rule-test semantics
# decide what the shipped rule files mean in a test) never arrives unnoticed.

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

EXPECTED="3.15.0"

promtool="$(rlocation "${PROMTOOL_RLOCATIONPATH:?set by the BUILD target}")"
out="$("$promtool" --version 2>&1)"
got="$(printf '%s\n' "$out" | sed -nE 's/^promtool, version ([0-9][^[:space:]]*) .*$/\1/p')"
if [ "$got" != "$EXPECTED" ]; then
	printf '%s\n' "$out" >&2
	echo "FAIL: promtool version is '${got}', expected '${EXPECTED}'" >&2
	echo "      (the Prometheus archives in MODULE.bazel moved: update EXPECTED, and" >&2
	echo "      read the release notes for PromQL and rule-test changes)" >&2
	exit 1
fi
echo "PASS: promtool ${got}"
