"""promtool_rules_test: Prometheus rule files checked, and their unit tests run, by the pinned promtool."""

load("@rules_shell//shell:sh_test.bzl", "sh_test")

def promtool_rules_test(name, rules, tests = [], **kwargs):
    """A test that runs the pinned promtool over rule files and their unit tests.

    `promtool check rules` on every file of `rules`, then `promtool test rules`
    on every file of `tests`. It fails when either does, and when `rules` is
    empty: a glob that matches nothing must not be a passing test.

    A rule test names its rule files relative to itself (`rule_files:`), so
    the rule files a test needs must be in `rules`. Every .yml in a directory
    of `rules` or `tests` must be in one of the two: the checker fails on one
    it finds there and was not handed.

    Args:
        name: name of the test
        rules: Prometheus rule files (alerting and recording rules)
        tests: promtool unit test files for them
        **kwargs: passed on to the sh_test
    """
    promtool = Label("//bazel/promtool:promtool_bin")
    sh_test(
        name = name,
        size = kwargs.pop("size", "small"),
        srcs = [Label("//bazel/promtool:check_rules.sh")],
        args = ["--rules"] + ["$(rootpath {})".format(f) for f in rules] +
               ["--tests"] + ["$(rootpath {})".format(f) for f in tests],
        data = rules + tests + [promtool],
        env = {"PROMTOOL_RLOCATIONPATH": "$(rlocationpath {})".format(promtool)},
        deps = ["@bazel_tools//tools/bash/runfiles"],
        **kwargs
    )
