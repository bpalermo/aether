load("@envoy//bazel:envoy_build_system.bzl", "envoy_cc_library", "envoy_cc_test")
load("@protobuf//bazel:cc_proto_library.bzl", "cc_proto_library")
load("@protobuf//bazel:proto_library.bzl", "proto_library")

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
        "@abseil-cpp//absl/strings",
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

envoy_cc_test(
    name = "aether_stats_test",
    srcs = ["aether_stats_test.cc"],
    repository = "@envoy",
    deps = [
        ":aether_stats_lib",
        ":pkg_cc_proto",
        "@envoy//source/common/stats:isolated_store_lib",
        "@envoy//test/mocks/server:server_factory_context_mocks",
        "@envoy//test/mocks/ssl:ssl_mocks",
        "@envoy//test/mocks/stream_info:stream_info_mocks",
        "@envoy//test/mocks/upstream:cluster_info_mocks",
    ],
)

# Guards the no-regex invariant behind aether#695: a counter the filter writes
# with programmatic tags keeps those tags when StatMerger merges it across a hot
# restart (envoyproxy/envoy#45674), with NO aether_stats stats_tags regexes in
# charts/aether/templates/agent-proxy-configmap.yaml. Also pins the pre-fix
# behaviour (propagation runtime guard off) and the two chart regexes that stay.
envoy_cc_test(
    name = "tag_extraction_test",
    srcs = ["tag_extraction_test.cc"],
    repository = "@envoy",
    deps = [
        ":aether_stats_lib",
        ":pkg_cc_proto",
        "@abseil-cpp//absl/strings",
        "@envoy//source/common/runtime:runtime_features_lib",
        "@envoy//source/common/stats:allocator_lib",
        "@envoy//source/common/stats:stat_merger_lib",
        "@envoy//source/common/stats:symbol_table_lib",
        "@envoy//source/common/stats:tag_producer_lib",
        "@envoy//source/common/stats:thread_local_store_lib",
        "@envoy//test/mocks/server:server_factory_context_mocks",
        "@envoy//test/mocks/stream_info:stream_info_mocks",
        "@envoy//test/mocks/upstream:cluster_info_mocks",
        "@envoy//test/test_common:utility_lib",
        "@envoy_api//envoy/config/metrics/v3:pkg_cc_proto",
    ],
)

# Boots a real Envoy with the filter wired as the agent emits it (TypedStruct)
# and drives a request through it. Catches a static-init abort (the binary won't
# start) and factory-resolution failures, which the unit test and the tar-driver
# image_test cannot. Links :config so the factory is registered in the test
# binary.
envoy_cc_test(
    name = "aether_stats_integration_test",
    srcs = ["aether_stats_integration_test.cc"],
    repository = "@envoy",
    deps = [
        ":config",
        "@abseil-cpp//absl/strings",
        "@envoy//source/common/common:minimal_logger_lib",
        "@envoy//test/integration:http_integration_lib",
        "@envoy//test/test_common:utility_lib",
        "@envoy_api//envoy/config/bootstrap/v3:pkg_cc_proto",
    ],
)
