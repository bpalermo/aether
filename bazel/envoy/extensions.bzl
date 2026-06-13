"""Pinned Envoy artifacts fetched for the dynamic-module build (proposal 007).

Keeps the Envoy fetch out of MODULE.bazel. Bump these together with the proxy
image (charts/agent/values.yaml) and the SDK git tag in
proxy/filters/telemetry/Cargo.toml on an Envoy upgrade.
"""

load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_file")

ENVOY_VERSION = "v1.38.0"

# source/extensions/dynamic_modules/abi/abi.h @ ENVOY_VERSION
_ABI_H_SHA256 = "5c54bfc141a7cc45974781ce9bb3bc291f6911aced7970a96009e19ba95ab445"

def _envoy_impl(_module_ctx):
    # The dynamic-modules ABI header. The SDK's bindgen build script reads it
    # via AETHER_ABI_H (see //bazel/rust); re-exported as the main-repo alias
    # //proxy/filters/telemetry:abi_h.
    http_file(
        name = "envoy_abi_h",
        downloaded_file_path = "abi.h",
        sha256 = _ABI_H_SHA256,
        urls = [
            "https://raw.githubusercontent.com/envoyproxy/envoy/{}/source/extensions/dynamic_modules/abi/abi.h".format(ENVOY_VERSION),
        ],
    )

envoy = module_extension(implementation = _envoy_impl)
