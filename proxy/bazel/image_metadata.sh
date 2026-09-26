#!/usr/bin/env bash
#
# Emits the aether-proxy image's OCI provenance as `name=value` lines, for
# oci_image's `labels` and `annotations` attributes (aether #837).
#
# Before this, nothing about a running proxy said which Envoy it contained:
#
#   * the image carried no labels at all, and its only manifest annotation was
#     `org.opencontainers.image.source` INHERITED from distroless/cc, pointing
#     at GoogleContainerTools/distroless -- a confident wrong answer;
#   * `envoy_server_version` is the first six hex digits of BUILD_SCM_REVISION,
#     which in this workspace is the AETHER commit, not Envoy's (see
#     ../README.md, "What envoy_server_version actually reports").
#
# So the Envoy pin is the one fact recoverable no other way, and it is the one
# this script reads straight out of the two files that define it. Both are
# parsed strictly: a miss is a hard failure, never a silently empty label.
#
# Usage: image_metadata.sh <MODULE.bazel> <.bazelrc>
# Requires the workspace status files (genrule `stamp = 1`).
set -euo pipefail

module_bazel="${1:?usage: image_metadata.sh <MODULE.bazel> <.bazelrc>}"
bazelrc="${2:?usage: image_metadata.sh <MODULE.bazel> <.bazelrc>}"

die() {
	echo "image_metadata.sh: $*" >&2
	exit 1
}

# The Envoy bazel module version, e.g. 1.40.0-dev.20260926.726d7ac.envoy.
envoy_version="$(sed -n -E 's/^bazel_dep\(name = "envoy", version = "([^"]+)"\).*$/\1/p' <"$module_bazel" | head -1)"
[ -n "$envoy_version" ] || die "no \`bazel_dep(name = \"envoy\", version = ...)\` in $module_bazel"

# ...whose `<date>.<sha>` tail is the upstream Envoy commit the snapshot was cut
# from. Absent on a hypothetical tagged release, which is not an error.
envoy_revision="$(printf '%s' "$envoy_version" | sed -n -E 's/^.*-dev\.[0-9]+\.([0-9a-f]+)\.envoy$/\1/p')"

# The envoyproxy/bazel-registry commit. Half of the pin, not a nicety: the
# registry keeps one version directory per module and deletes the previous one
# on every bump, so the module version alone does not resolve without it.
envoy_registry="$(sed -n -E 's#^common --registry=https://raw\.githubusercontent\.com/envoyproxy/bazel-registry/([0-9a-f]{40})$#\1#p' <"$bazelrc" | head -1)"
[ -n "$envoy_registry" ] || die "no envoyproxy/bazel-registry --registry pin in $bazelrc"

# The aether commit. BUILD_SCM_REVISION comes from bazel/get_workspace_status,
# which runs `git rev-parse HEAD` in THIS workspace -- i.e. in the aether
# repository, because proxy/ is a nested workspace inside it. It is the very
# same value Envoy's linkstamp compiles in and reports as
# `envoy_server_version`, so the label and the gauge can never disagree.
revision="$(sed -n -E 's/^BUILD_SCM_REVISION ([0-9a-f]{40})$/\1/p' <bazel-out/volatile-status.txt | head -1)"
[ -n "$revision" ] || die "no 40-hex BUILD_SCM_REVISION in bazel-out/volatile-status.txt"

echo "org.opencontainers.image.source=https://github.com/bpalermo/aether"
echo "org.opencontainers.image.revision=$revision"
echo "dev.aethermesh.envoy.module-version=$envoy_version"
[ -z "$envoy_revision" ] || echo "dev.aethermesh.envoy.revision=$envoy_revision"
echo "dev.aethermesh.envoy.bazel-registry=$envoy_registry"
