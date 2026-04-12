"Define linter aspects"

load("@aspect_rules_lint//lint:buf.bzl", "lint_buf_aspect")
load("@aspect_rules_lint//lint:buildifier.bzl", "lint_buildifier_aspect")
load("@aspect_rules_lint//lint:lint_test.bzl", "lint_test")
load("@aspect_rules_lint//lint:shellcheck.bzl", "lint_shellcheck_aspect")

buf = lint_buf_aspect(
    config = Label("//bazel/lint:buf.yaml"),
)

buf_test = lint_test(aspect = buf)

buildifier = lint_buildifier_aspect(
    binary = Label("@buildifier_prebuilt//:buildifier"),
)

buildifier_test = lint_test(aspect = buildifier)

shellcheck = lint_shellcheck_aspect(
    binary = Label("@aspect_rules_lint//lint:shellcheck_bin"),
    config = Label("@//:.shellcheckrc"),
)

shellcheck_test = lint_test(aspect = shellcheck)
