"""Pinned Envoy artifacts fetched for the proxy build (proposals 007/008).

The single source of truth for the Envoy version (ENVOY_VERSION) — the abi.h
fetch and the official Envoy release binary (baked into the custom aether-proxy
image, //proxy:image) all derive their URLs from here, keeping the version out of
MODULE.bazel. On an Envoy upgrade bump ENVOY_VERSION + the sha256s below, and the
SDK git tag in proxy/filters/http/aether_stats/Cargo.toml (a static manifest that can't
read this).
"""

load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_file")

ENVOY_VERSION = "v1.38.0"

# ENVOY_VERSION without the leading "v" — release-asset filenames use it.
_V = ENVOY_VERSION[1:]

# source/extensions/dynamic_modules/abi/abi.h @ ENVOY_VERSION
_ABI_H_SHA256 = "5c54bfc141a7cc45974781ce9bb3bc291f6911aced7970a96009e19ba95ab445"

# Official Envoy release binaries (github releases), per arch — baked at
# /usr/local/bin/envoy in //proxy:image on a distroless/cc base (which provides
# the libstdc++/libgcc_s/glibc runtime the binary and the Rust module need).
_ENVOY_BIN = {
    "envoy_bin_amd64": ("x86_64", "cca312a7c3f91852f2849995c895130c59842e21ba787dc90bafa4026d6c5ecc"),
    "envoy_bin_arm64": ("aarch_64", "9f00678c0cc433d7a342c422415eb3fdd163e77414aee5c5b57c04bd7d579454"),
}

def _envoy_impl(_module_ctx):
    # The dynamic-modules ABI header. The SDK's bindgen build script reads it
    # via AETHER_ABI_H (the crate.annotation in MODULE.bazel); re-exported as
    # the main-repo alias //proxy/filters/http/aether_stats:abi_h.
    http_file(
        name = "envoy_abi_h",
        downloaded_file_path = "abi.h",
        sha256 = _ABI_H_SHA256,
        urls = [
            "https://raw.githubusercontent.com/envoyproxy/envoy/{}/source/extensions/dynamic_modules/abi/abi.h".format(ENVOY_VERSION),
        ],
    )

    for name, (arch, sha) in _ENVOY_BIN.items():
        http_file(
            name = name,
            downloaded_file_path = "envoy",
            executable = True,
            sha256 = sha,
            urls = [
                "https://github.com/envoyproxy/envoy/releases/download/{ver}/envoy-{v}-linux-{arch}".format(
                    ver = ENVOY_VERSION,
                    v = _V,
                    arch = arch,
                ),
            ],
        )

envoy = module_extension(implementation = _envoy_impl)
