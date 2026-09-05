load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")

# Pinned Envoy source — the single source of truth for the custom proxy build.
#
# Pinned to the v1.39.0 release commit. The aether_stats extension is a
# compiled-in C++ filter (//source/extensions/filters/http/aether_stats), so it
# is built against this exact Envoy tree — there is no separate SDK to keep in
# sync. On an Envoy bump:
#   1. ENVOY_SHA = <commit of the new tag>
#      (`gh api repos/envoyproxy/envoy/git/refs/tags/vX.Y.Z`)
#   2. ENVOY_SHA256 = sha256 of the archive
#      (`curl -sSL .../archive/$ENVOY_SHA.tar.gz | sha256sum`)
#   3. Re-check .bazelversion / envoy.bazelrc against the new Envoy release.
#
# Envoy tag: v1.39.0
ENVOY_SHA = "8eea3285d6bdb89f8ea34632cfe7ce1608a8f374"

ENVOY_SHA256 = "aa73fb45f0e6e4fbaf5cb68a81fa898bbd2f9098fc46122962119f5c6c9b7e9f"

ENVOY_ORG = "envoyproxy"

ENVOY_REPO = "envoy"

def envoy_repository():
    # To override with a local Envoy checkout, pass
    # `--override_repository=envoy=/PATH/TO/ENVOY` or persist it in user.bazelrc.
    #
    # No patches: the two aarch64 patches this archive used to carry
    # (Rust-SDK libclang, dynamic-modules llvm-objcopy) existed only to build
    # Envoy's dynamic-modules extensions on the arm64 RBE pool, and those
    # extensions are no longer compiled in — see //bazel/extension_config.
    http_archive(
        name = "envoy",
        sha256 = ENVOY_SHA256,
        strip_prefix = ENVOY_REPO + "-" + ENVOY_SHA,
        url = "https://github.com/" + ENVOY_ORG + "/" + ENVOY_REPO + "/archive/" + ENVOY_SHA + ".tar.gz",
    )
