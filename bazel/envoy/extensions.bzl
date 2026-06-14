"""Pinned Envoy artifacts fetched for the proxy build (proposals 007/008).

The single source of truth for the Envoy version (ENVOY_VERSION) — the abi.h
fetch, the aether-proxy image base (@envoy_distroless), and their version-derived
tags/URLs all come from here, keeping the version out of MODULE.bazel. On an
Envoy upgrade bump ENVOY_VERSION + the two digests below, and the SDK git tag in
proxy/filters/telemetry/Cargo.toml (a static manifest that can't read this).
"""

load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_file")
load("@rules_img//img:pull.bzl", "pull")

ENVOY_VERSION = "v1.38.0"

# source/extensions/dynamic_modules/abi/abi.h @ ENVOY_VERSION
_ABI_H_SHA256 = "5c54bfc141a7cc45974781ce9bb3bc291f6911aced7970a96009e19ba95ab445"

# Multi-arch index digest of envoyproxy/envoy:distroless-{ENVOY_VERSION}, the
# base for the custom aether-proxy image (//proxy:image).
_DISTROLESS_DIGEST = "sha256:a7a56545102f7a682e0cafea2c9b8448af1b09ebb710eab688dfb931e3ec7ff6"

def _envoy_impl(_module_ctx):
    # The dynamic-modules ABI header. The SDK's bindgen build script reads it
    # via AETHER_ABI_H (the crate.annotation in MODULE.bazel); re-exported as
    # the main-repo alias //proxy/filters/telemetry:abi_h.
    http_file(
        name = "envoy_abi_h",
        downloaded_file_path = "abi.h",
        sha256 = _ABI_H_SHA256,
        urls = [
            "https://raw.githubusercontent.com/envoyproxy/envoy/{}/source/extensions/dynamic_modules/abi/abi.h".format(ENVOY_VERSION),
        ],
    )

    # Stock upstream Envoy distroless image — the base for //proxy:image (Envoy
    # base + baked stats module). Tag derived from ENVOY_VERSION.
    pull(
        name = "envoy_distroless",
        digest = _DISTROLESS_DIGEST,
        layer_handling = "lazy",
        registry = "index.docker.io",
        repository = "envoyproxy/envoy",
        tag = "distroless-{}".format(ENVOY_VERSION),
    )

envoy = module_extension(implementation = _envoy_impl)
