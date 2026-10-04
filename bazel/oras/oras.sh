#!/usr/bin/env bash
# `bazel run //bazel/oras -- <oras args>`: THE oras for this repository.
#
# Execs the official oras release binary MODULE.bazel pins (http_archive per
# platform, sha256 from the release's checksums file). chart_push
# (//bazel/helm:defs.bzl) pushes every chart with this same binary, so a
# workstation and CI run one oras version. Bumping oras is bumping those
# archives; //bazel/oras:version_test fails until its EXPECTED moves with them.
#
# The process runs in the caller's directory (BUILD_WORKING_DIRECTORY), not the
# runfiles tree, so relative paths mean what they look like, and it inherits
# the caller's environment. oras reads registry credentials from the Docker
# config (~/.docker/config.json), which `docker login` / docker/login-action
# writes.

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

oras="$(rlocation "${ORAS_RLOCATIONPATH:?set by the BUILD target}")"
if [ -z "$oras" ] || [ ! -x "$oras" ]; then
	echo "ERROR: oras binary not found in runfiles (${ORAS_RLOCATIONPATH})" >&2
	exit 2
fi

cd "${BUILD_WORKING_DIRECTORY:-.}"
exec "$oras" "$@"
