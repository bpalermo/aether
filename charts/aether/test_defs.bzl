"""The template-test rules, as charts/aether/BUILD.bazel uses them.

`clusterName` is a required value of this chart since 2.6.0: a render without
it fails. Every test of the chart therefore has to name a cluster, and none of
them is about which one. The wrappers below put `--set clusterName=<TEST_CLUSTER_NAME>`
in front of each target's `opts`, so a test file of some two hundred targets
does not repeat it two hundred times. A target's own `opts` come after it and
win: `--set clusterName=other` in a target is what helm renders.

The tests of the requirement itself (`:aether_cluster_name_required_test` and
its neighbours) must render WITHOUT the value, and load the rule under another
name straight from //bazel/helm:defs.bzl.

The wrapper keeps the name of the rule it wraps, so what reads the BUILD file
as text (//bazel/helm:upstream_template_test_opts_test.sh) reads the same
calls.
"""

load(
    "@rules_helm//helm:defs.bzl",
    _helm_template_test = "helm_template_test",
)
load(
    "//bazel/helm:defs.bzl",
    _helm_pod_template_file_test = "helm_pod_template_file_test",
    _helm_pod_template_version_test = "helm_pod_template_version_test",
    _helm_template_absent_test = "helm_template_absent_test",
    _helm_template_fail_test = "helm_template_fail_test",
    _helm_template_match_test = "helm_template_match_test",
    _helm_template_reproducible_test = "helm_template_reproducible_test",
    _helm_template_value_changes_test = "helm_template_value_changes_test",
    _helm_template_value_ignored_test = "helm_template_value_ignored_test",
)

# The cluster every test of the chart renders for, unless its own `opts` name
# another. Not a default of the chart: the chart has none.
TEST_CLUSTER_NAME = "test-cluster"

_CLUSTER_OPTS = [
    "--set",
    "clusterName=" + TEST_CLUSTER_NAME,
]

def _with_cluster_name(rule):
    def _wrapped(name, opts = None, **kwargs):
        rule(
            name = name,
            opts = _CLUSTER_OPTS + (opts or []),
            **kwargs
        )

    return _wrapped

helm_template_test = _with_cluster_name(_helm_template_test)
helm_pod_template_file_test = _with_cluster_name(_helm_pod_template_file_test)
helm_pod_template_version_test = _with_cluster_name(_helm_pod_template_version_test)
helm_template_absent_test = _with_cluster_name(_helm_template_absent_test)
helm_template_fail_test = _with_cluster_name(_helm_template_fail_test)
helm_template_match_test = _with_cluster_name(_helm_template_match_test)
helm_template_reproducible_test = _with_cluster_name(_helm_template_reproducible_test)
helm_template_value_changes_test = _with_cluster_name(_helm_template_value_changes_test)
helm_template_value_ignored_test = _with_cluster_name(_helm_template_value_ignored_test)
