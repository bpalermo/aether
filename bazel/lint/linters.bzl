"Define linter aspects"

load("@aspect_rules_lint//lint:buf.bzl", "lint_buf_aspect")
load("@aspect_rules_lint//lint:lint_test.bzl", "lint_test")
load("@aspect_rules_lint//lint:shellcheck.bzl", "lint_shellcheck_aspect")
load("//bazel/lint:gocognit.bzl", "lint_gocognit_aspect")

buf = lint_buf_aspect(
    config = Label("//bazel/lint:buf.yaml"),
)

buf_test = lint_test(aspect = buf)

# No buildifier aspect: rules_lint's only visits bzl_library targets and
# "starlark"-tagged filegroups, and BUILD files cannot be srcs of a target in
# another package, so here it linted nothing (#1245). buildifier's linter runs
# in the Starlark formatter instead: bazel/format/BUILD.bazel.

shellcheck = lint_shellcheck_aspect(
    binary = Label("@aspect_rules_lint//lint:shellcheck_bin"),
    config = Label("@//:.shellcheckrc"),
)

shellcheck_test = lint_test(aspect = shellcheck)

# gocognit reports Go functions over a cognitive-complexity threshold.
# 15 is the target (reached via the #556 campaign, PRs #558-#563); the
# aspect lints production code only. See bazel/lint/gocognit.bzl.
gocognit = lint_gocognit_aspect(
    binary = Label("@com_github_uudashr_gocognit//cmd/gocognit"),
    threshold = 15,
)

gocognit_test = lint_test(aspect = gocognit)
