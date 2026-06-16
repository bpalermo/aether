load("@com_google_protobuf//bazel:cc_proto_library.bzl", "cc_proto_library")
load("@com_google_protobuf//bazel:proto_library.bzl", "proto_library")
load("@envoy//bazel:envoy_build_system.bzl", "envoy_cc_library")

# aether_stats native C++ HTTP filter (proposal 012). Compiled into the custom
# Envoy via //:envoy AETHER_EXTENSIONS.
package(default_visibility = ["//visibility:public"])

proto_library(
    name = "pkg",
    srcs = ["aether_stats.proto"],
)

cc_proto_library(
    name = "pkg_cc_proto",
    deps = [":pkg"],
)

envoy_cc_library(
    name = "aether_stats_lib",
    srcs = ["aether_stats.cc"],
    hdrs = ["aether_stats.h"],
    repository = "@envoy",
    deps = [
        ":pkg_cc_proto",
        "@com_google_absl//absl/strings",
        "@envoy//envoy/server:factory_context_interface",
        "@envoy//envoy/stats:stats_interface",
        "@envoy//envoy/stream_info:stream_info_interface",
        "@envoy//envoy/upstream:upstream_interface",
        "@envoy//source/common/stats:symbol_table_lib",
        "@envoy//source/common/stream_info:utility_lib",
        "@envoy//source/extensions/filters/http/common:pass_through_filter_lib",
    ],
)

envoy_cc_library(
    name = "config",
    srcs = ["config.cc"],
    hdrs = ["config.h"],
    repository = "@envoy",
    deps = [
        ":aether_stats_lib",
        ":pkg_cc_proto",
        "@envoy//envoy/registry",
        "@envoy//envoy/server:filter_config_interface",
    ],
)
