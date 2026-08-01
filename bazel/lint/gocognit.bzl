"""API for declaring a gocognit cognitive-complexity lint aspect.

gocognit (https://github.com/uudashr/gocognit) reports Go functions whose
cognitive complexity exceeds a threshold. This aspect visits production Go
rules (go_library / go_binary), filters out generated sources, and runs
gocognit with `-over <threshold>`.

Typical usage in `bazel/lint/linters.bzl`:

```starlark
load("//bazel/lint:gocognit.bzl", "lint_gocognit_aspect")

gocognit = lint_gocognit_aspect(
    binary = Label("@com_github_uudashr_gocognit//cmd/gocognit"),
    threshold = 15,
)

gocognit_test = lint_test(aspect = gocognit)
```

## Threshold / scope policy

The threshold is **15**: the aspect was introduced at a ratchet of 109 (the
then-smallest zero-violation value), and the #556 campaign refactored all 65
production functions over 15, so the gate now sits at the target. Test code
(go_test rules) is deliberately NOT visited: complexity gating on validated
e2e/conformance harnesses adds churn risk for no production value.

## Contract

This file deliberately re-implements (vendors) the small amount of helper logic
it needs from aspect_rules_lint's `lint:private/lint_aspect.bzl` rather than
loading from that private path, so an upgrade of aspect_rules_lint can't break
us. The reproduced contract:

  * OutputGroupInfo(rules_lint_human, rules_lint_machine, _validation)
  * `<name>.<mnemonic>.out` / `<name>.<mnemonic>.report` report files (always
    produced, even on a clean target).
  * When `--@aspect_rules_lint//lint:fail_on_violation` is NOT set, the linter
    exit code is written to a sibling `.exit_code` file and the action itself
    succeeds (downstream tooling / lint_test interprets it). When it IS set,
    the action's own exit code carries the violation so `--config=ci` fails the
    build. We select on the public `//lint:fail_on_violation.enabled`
    config_setting to avoid depending on the private `LintOptionsInfo`
    provider.

gocognit's `-over N` already exits 1 iff its output is non-empty and 0
otherwise, so — unlike the original plan's assumption — no exit-code inversion
is needed; the same success/exit-code-capture shell wrapper the upstream
shellcheck/buf aspects use works verbatim.
"""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")

_MNEMONIC = "AetherGoCognit"

_OUTFILE_FORMAT = "{label}.{mnemonic}.{suffix}"

# The public bool_flag toggled by `--@aspect_rules_lint//lint:fail_on_violation`
# (pulled in by `--config=ci`). It provides bazel_skylib's BuildSettingInfo, so
# we can read its value without touching the private LintOptionsInfo provider.
_FAIL_ON_VIOLATION = "@aspect_rules_lint//lint:fail_on_violation"

# Generated / non-hand-written Go sources we never want to lint.
_GENERATED_SUFFIXES = [
    ".pb.go",
    "_pb.go",
    ".pb.validate.go",
    ".pb.dynamo.go",
]

_GENERATED_PREFIXES = [
    "zz_generated",
]

_GENERATED_SUBSTRINGS = [
    ".pb.",
    "mock_",
    "_mock",
]

def _is_generated(f):
    base = f.basename
    for suffix in _GENERATED_SUFFIXES:
        if base.endswith(suffix):
            return True
    for prefix in _GENERATED_PREFIXES:
        if base.startswith(prefix):
            return True
    for sub in _GENERATED_SUBSTRINGS:
        if sub in base:
            return True
    if "/testdata/" in f.path:
        return True
    return False

def _should_visit(rule, allow_kinds):
    """Vendored from lint_aspect.should_visit: skip `no-lint`-tagged targets."""
    if "no-lint" in rule.attr.tags:
        return False
    return rule.kind in allow_kinds

def _go_srcs_to_lint(rule):
    """Non-generated, first-party Go source files from the rule's srcs."""
    if not hasattr(rule.attr, "srcs"):
        return []
    out = []
    for src in rule.files.srcs:
        if not src.basename.endswith(".go"):
            continue

        # Only lint sources checked into this workspace (skip external + genfiles).
        if not src.is_source or src.owner.workspace_name != "":
            continue
        if _is_generated(src):
            continue
        out.append(src)
    return out

def _declare_report(ctx, target, suffix):
    return ctx.actions.declare_file(_OUTFILE_FORMAT.format(
        label = target.label.name,
        mnemonic = _MNEMONIC,
        suffix = suffix,
    ))

def _run_gocognit(ctx, srcs, out, exit_code):
    """Run `gocognit -over N` over srcs.

    Mirrors the shellcheck/buf action contract:
      * If `exit_code` is provided, always succeed and capture gocognit's exit
        code to that file (report goes to `out`).
      * Otherwise, propagate gocognit's exit code as the action's result and
        still write the report to `out` (a clean target produces an empty one).
    """
    args = ctx.actions.args()
    args.add("-over", str(ctx.attr._threshold))
    args.add_all(srcs)

    outputs = [out]
    if exit_code:
        command = "{gocognit} $@ >{out}; echo $? >{exit_code}"
        outputs.append(exit_code)
    else:
        command = "{gocognit} $@ >{out}"

    ctx.actions.run_shell(
        inputs = srcs,
        outputs = outputs,
        command = command.format(
            gocognit = ctx.executable._gocognit.path,
            out = out.path,
            exit_code = exit_code.path if exit_code else "",
        ),
        arguments = [args],
        mnemonic = _MNEMONIC,
        progress_message = "Linting %{label} with gocognit",
        tools = [ctx.executable._gocognit],
    )

def _noop(ctx, outputs):
    """Produce the expected (empty) outputs for a target with nothing to lint."""
    outs = [outputs.human_out, outputs.machine_out]
    commands = ["touch {}".format(o.path) for o in outs]
    if outputs.human_exit_code:
        commands.append("echo 0 > {}".format(outputs.human_exit_code.path))
        outs.append(outputs.human_exit_code)
    if outputs.machine_exit_code:
        commands.append("echo 0 > {}".format(outputs.machine_exit_code.path))
        outs.append(outputs.machine_exit_code)
    ctx.actions.run_shell(
        inputs = [],
        outputs = outs,
        command = " && ".join(commands),
        mnemonic = _MNEMONIC,
        progress_message = "Linting %{label} with gocognit (no Go sources)",
    )

def _outputs(ctx, target):
    human_out = _declare_report(ctx, target, "out")
    machine_out = _declare_report(ctx, target, "report")

    if ctx.attr._fail_on_violation[BuildSettingInfo].value:
        # The action's own exit code carries the violation (fails --config=ci).
        human_exit_code = None
        machine_exit_code = None
    else:
        # The exit codes are provided as outputs so the build itself succeeds;
        # lint_test / `aspect lint` interpret them.
        human_exit_code = _declare_report(ctx, target, "out.exit_code")
        machine_exit_code = _declare_report(ctx, target, "report.exit_code")

    human_files = [f for f in [human_out, human_exit_code] if f]
    machine_files = [f for f in [machine_out, machine_exit_code] if f]

    info = OutputGroupInfo(
        rules_lint_human = depset(human_files),
        rules_lint_machine = depset(machine_files),
        # Force the action to run even if the outputs aren't explicitly requested.
        _validation = depset(human_files[0:1]),
    )
    return struct(
        human_out = human_out,
        human_exit_code = human_exit_code,
        machine_out = machine_out,
        machine_exit_code = machine_exit_code,
    ), info

def _gocognit_aspect_impl(target, ctx):
    if not _should_visit(ctx.rule, ctx.attr._rule_kinds):
        return []

    srcs = _go_srcs_to_lint(ctx.rule)
    outputs, info = _outputs(ctx, target)

    if len(srcs) == 0:
        _noop(ctx, outputs)
        return [info]

    _run_gocognit(ctx, srcs, outputs.human_out, outputs.human_exit_code)
    _run_gocognit(ctx, srcs, outputs.machine_out, outputs.machine_exit_code)
    return [info]

def lint_gocognit_aspect(binary, threshold, rule_kinds = ["go_library", "go_binary"]):
    """A factory function to create the gocognit linter aspect.

    Args:
        binary: a gocognit executable (e.g. `@com_github_uudashr_gocognit//cmd/gocognit`).
        threshold: report functions whose cognitive complexity is strictly greater
            than this value (`-over <threshold>`).
        rule_kinds: which rule kinds the aspect visits.
    """
    return aspect(
        implementation = _gocognit_aspect_impl,
        attrs = {
            "_gocognit": attr.label(
                default = binary,
                executable = True,
                cfg = "exec",
            ),
            "_threshold": attr.int(
                default = threshold,
            ),
            "_rule_kinds": attr.string_list(
                default = rule_kinds,
            ),
            # Honor the same flag the upstream factories read via LintOptionsInfo,
            # but via the public bool_flag's BuildSettingInfo so we never touch
            # lint:private.
            "_fail_on_violation": attr.label(
                default = _FAIL_ON_VIOLATION,
                providers = [BuildSettingInfo],
            ),
        },
    )
