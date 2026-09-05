# The set of Envoy extensions compiled into //:envoy.
#
# Until aether #697 this file was a 659-line VERBATIM copy of Envoy v1.38.0's
# source/extensions/extensions_build_config.bzl — a stale snapshot that
# customised nothing and silently fell two releases behind (it was missing every
# extension v1.39.0 added). It now loads Envoy's default at the pinned version
# and subtracts, so an Envoy bump picks up new upstream extensions for free and
# the _DROPPED list below is the whole of our policy.
#
# The aether_stats filter is NOT an entry here: it is a dep of the binary
# (AETHER_EXTENSIONS in //BUILD.bazel), exactly like the upstream filter-cc
# example's :http_filter_config. This dict is only for extensions that live in
# @envoy.
load(
    "@envoy//source/extensions:extensions_build_config.bzl",
    _CONTRIB_EXTENSION_PACKAGE_VISIBILITY = "CONTRIB_EXTENSION_PACKAGE_VISIBILITY",
    _EXTENSIONS = "EXTENSIONS",
    _EXTENSION_CONFIG_VISIBILITY = "EXTENSION_CONFIG_VISIBILITY",
    _EXTENSION_PACKAGE_VISIBILITY = "EXTENSION_PACKAGE_VISIBILITY",
    _LEGACY_ALWAYSLINK = "LEGACY_ALWAYSLINK",
    _MOBILE_PACKAGE_VISIBILITY = "MOBILE_PACKAGE_VISIBILITY",
)

# Substrings matched against the extension NAME (not the target label). At
# v1.39.0: 330 upstream extensions - 27 dropped = 303 compiled in.
_DROPPED = [
    # wasm: aether emits no wasm filter/runtime/access-logger/stat-sink anywhere
    # (no `wasm` in charts/, agent/, common/ or api/). Drops 6 entries plus the
    # v8/wasmtime/wamr/emsdk build behind them.
    "wasm",
    # dynamic modules: superseded for aether by proposal 012 — aether_stats is a
    # compiled-in C++ filter and the Rust dynamic-module approach was dropped,
    # while proposal 025's extension escape hatch is CRD-based. Keeping them
    # costs the Rust toolchain, crate_universe, bindgen/libclang and
    # llvm-objcopy — which is the entire reason //bazel/patches existed and why
    # CI had to set CARGO_BAZEL_REPIN. Matches envoy.*.dynamic_modules* and
    # envoy.matching.inputs.dynamic_module_*.
    "dynamic_module",
    # envoy.network.dns_resolver.hickory: the ONE extension outside the two
    # groups above that reaches the dynamic-modules machinery. Its name carries
    # no hint of it, so the first attempt at this trim kept it and //:envoy
    # failed to link on both arches with ~every `envoy_dynamic_module_callback_*`
    # undefined: hickory/BUILD deps on
    # //source/extensions/dynamic_modules/builtin_extensions:hickory_dns_static,
    # a Rust static library that CALLS those callbacks, while their definitions
    # live in //source/extensions/dynamic_modules:abi_impl — which only the
    # dropped dynamic-modules extensions pull in. (grep of envoy@v1.39.0: hickory
    # is the only non-test, non-dynamic_modules BUILD referencing either target.)
    #
    # Safe to drop: aether configures no typed_dns_resolver_config anywhere, so
    # Envoy uses its default c-ares resolver; envoy.network.dns_resolver.{cares,
    # getaddrinfo} both stay. Dropping it also makes "no Rust in the proxy build"
    # true rather than nearly true.
    "hickory",
]

EXTENSIONS = {
    name: target
    for name, target in _EXTENSIONS.items()
    if not [drop for drop in _DROPPED if drop in name]
}

CONTRIB_EXTENSION_PACKAGE_VISIBILITY = _CONTRIB_EXTENSION_PACKAGE_VISIBILITY

EXTENSION_CONFIG_VISIBILITY = _EXTENSION_CONFIG_VISIBILITY

EXTENSION_PACKAGE_VISIBILITY = _EXTENSION_PACKAGE_VISIBILITY

LEGACY_ALWAYSLINK = _LEGACY_ALWAYSLINK

MOBILE_PACKAGE_VISIBILITY = _MOBILE_PACKAGE_VISIBILITY
