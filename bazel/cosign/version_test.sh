#!/usr/bin/env bash
# The cosign //bazel/cosign runs is the version this repository documents.
#
# Network-free: the binary is a repository artifact, checksummed by the
# rules_img_signer_cosign lock. A bazel_dep bump that moves cosign fails here
# until EXPECTED below moves with it, so a new cosign major (whose signature
# layout changed once already, v2 -> v3) never arrives unnoticed.

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

EXPECTED="v3.1.2"

cosign="$(rlocation "${COSIGN_RLOCATIONPATH:?set by the BUILD target}")"
out="$("$cosign" version 2>&1)"
got="$(printf '%s\n' "$out" | sed -nE 's/^GitVersion:[[:space:]]+(v[0-9][^[:space:]]*)$/\1/p')"
if [ "$got" != "$EXPECTED" ]; then
	printf '%s\n' "$out" >&2
	echo "FAIL: cosign GitVersion is '${got}', expected '${EXPECTED}'" >&2
	echo "      (a rules_img_signer_cosign bump moved cosign: update EXPECTED and the" >&2
	echo "      runbook's version, and re-verify a published image by hand)" >&2
	exit 1
fi
echo "PASS: cosign ${got}"
