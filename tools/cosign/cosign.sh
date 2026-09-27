#!/usr/bin/env bash
# `bazel run //tools/cosign -- <cosign args>`: THE cosign for this repository.
#
# Execs the official prebuilt cosign release binary that the
# rules_img_signer_cosign module downloads for the current platform
# (@rules_img_signer_cosign//cosign), pinned by sha256 in that module's
# cli/cosign_cli.lock.json. CI signs and verifies with it and so does a
# workstation, so there is exactly one cosign version in play and it is whatever
# the bazel_dep in MODULE.bazel says. Bumping cosign is bumping that bazel_dep;
# //tools/cosign:version_test fails until the expected version moves with it.
#
# The process runs in the caller's directory (BUILD_WORKING_DIRECTORY), not the
# runfiles tree, so relative paths in the arguments mean what they look like, and
# it inherits the caller's environment -- including the GitHub Actions OIDC
# variables keyless signing reads (ACTIONS_ID_TOKEN_REQUEST_URL/_TOKEN).

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

cosign="$(rlocation "${COSIGN_RLOCATIONPATH:?set by the BUILD target}")"
if [ -z "$cosign" ] || [ ! -x "$cosign" ]; then
	echo "ERROR: cosign binary not found in runfiles (${COSIGN_RLOCATIONPATH})" >&2
	exit 2
fi

cd "${BUILD_WORKING_DIRECTORY:-.}"
exec "$cosign" "$@"
