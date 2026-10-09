#!/usr/bin/env bash
# `bazel run //bazel/promtool -- <promtool args>`: THE promtool for this
# repository.
#
# Execs the promtool of the official Prometheus release MODULE.bazel pins
# (http_archive per platform, sha256 from the release's sha256sums.txt), the
# same binary //:observability_rules_test checks the shipped rule files with,
# so a workstation and CI run one version. Bumping it is bumping those
# archives; //bazel/promtool:version_test fails until its EXPECTED moves with
# them.
#
# The process runs in the caller's directory (BUILD_WORKING_DIRECTORY), not the
# runfiles tree, so relative paths mean what they look like:
#
#   bazel run //bazel/promtool -- test rules docs/observability/<name>_test.yml

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

promtool="$(rlocation "${PROMTOOL_RLOCATIONPATH:?set by the BUILD target}")"
if [ -z "$promtool" ] || [ ! -x "$promtool" ]; then
	echo "ERROR: promtool binary not found in runfiles (${PROMTOOL_RLOCATIONPATH})" >&2
	exit 2
fi

cd "${BUILD_WORKING_DIRECTORY:-.}"
exec "$promtool" "$@"
