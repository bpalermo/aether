load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")

# Pinned Envoy source — the single source of truth for the custom proxy build.
#
# Pinned to the v1.38.0 release commit. The aether_stats extension is a
# compiled-in C++ filter (//source/extensions/filters/http/aether_stats), so it
# is built against this exact Envoy tree — there is no separate SDK to keep in
# sync. On an Envoy bump:
#   1. ENVOY_SHA = <commit of the new tag>
#      (`gh api repos/envoyproxy/envoy/git/refs/tags/vX.Y.Z`)
#   2. ENVOY_SHA256 = sha256 of the archive
#      (`curl -sSL .../archive/$ENVOY_SHA.tar.gz | sha256sum`)
#   3. Re-check .bazelversion / envoy.bazelrc against the new Envoy release.
#
# Envoy tag: v1.38.0
ENVOY_SHA = "f1dd21b16c244bda00edfb5ffce577e12d0d2ec2"

ENVOY_SHA256 = "e58e6b779aeb0d0efcf67e305c09b4e4f66d935dc1a00dc1cf199437fa93a115"

ENVOY_ORG = "envoyproxy"

ENVOY_REPO = "envoy"

def envoy_repository():
    # To override with a local Envoy checkout, pass
    # `--override_repository=envoy=/PATH/TO/ENVOY` or persist it in user.bazelrc.
    http_archive(
        name = "envoy",
        sha256 = ENVOY_SHA256,
        strip_prefix = ENVOY_REPO + "-" + ENVOY_SHA,
        url = "https://github.com/" + ENVOY_ORG + "/" + ENVOY_REPO + "/archive/" + ENVOY_SHA + ".tar.gz",
        # Source patches to @envoy go here (patch_args = ["-p1"]). None yet — the
        # first real patch is the proposal-010 follow-up.
        patches = [],
    )
