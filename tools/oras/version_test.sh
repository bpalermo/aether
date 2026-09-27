#!/usr/bin/env bash
# The oras //tools/oras runs is the version MODULE.bazel pins.
#
# Network-free: the binary is a repository artifact, checksummed by its
# http_archive. A bump of those archives fails here until EXPECTED below moves
# with them, so a new oras (whose push defaults -- manifest spec, annotations --
# decide what a chart publish writes) never arrives unnoticed.

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

EXPECTED="1.3.4"

oras="$(rlocation "${ORAS_RLOCATIONPATH:?set by the BUILD target}")"
out="$("$oras" version 2>&1)"
got="$(printf '%s\n' "$out" | sed -nE 's/^Version:[[:space:]]+([0-9][^[:space:]]*)$/\1/p')"
if [ "$got" != "$EXPECTED" ]; then
	printf '%s\n' "$out" >&2
	echo "FAIL: oras Version is '${got}', expected '${EXPECTED}'" >&2
	echo "      (the oras archives in MODULE.bazel moved: update EXPECTED, and" >&2
	echo "      re-check that a chart pushed with it still pulls with helm)" >&2
	exit 1
fi
echo "PASS: oras ${got}"
