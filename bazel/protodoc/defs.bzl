"""Render a `proto_library` as a markdown API reference, at build time.

Used by //website to publish aethermesh.dev's `/api/` section. The point is that
the pages are an *output of the proto graph*: edit a comment in any `.proto` and
the page rebuilds, so the published reference can never drift from the schema.
No generated markdown is committed.

Include paths, not descriptor sets. The obvious wiring — hand protoc the
`ProtoInfo.transitive_descriptor_sets` the graph already built, with
`--descriptor_set_in` — silently produces a reference with **every description
column empty**: Bazel compiles those descriptor sets without
`--include_source_info`, and a `.proto` comment lives nowhere else. Comments are
the entire product here, so this instead compiles from sources, and takes the
`-I` roots from `ProtoInfo.transitive_proto_path` and the sources from
`ProtoInfo.transitive_sources`. Both are computed by the `proto_library` graph
from `deps`, so `buf/validate`, `google/protobuf/*` and aether's own
cross-package imports resolve exactly as they do for the Go code generators, and
there is no second, hand-maintained include path to drift out of sync.
"""

load("@protobuf//bazel/common:proto_info.bzl", "ProtoInfo")

def _import_name(file):
    """The name protoc knows a source file by, i.e. its import path."""
    return file.path

def _proto_doc_impl(ctx):
    proto_info = ctx.attr.proto[ProtoInfo]
    out = ctx.actions.declare_file(ctx.label.name + ".md")

    args = ctx.actions.args()
    args.add_all(proto_info.transitive_proto_path, format_each = "-I%s")
    args.add(ctx.executable._plugin, format = "--plugin=protoc-gen-doc=%s")

    # protoc-gen-doc writes <doc_out>/<the filename in doc_opt>; pointing both at
    # the declared output keeps the action's output declared and hermetic.
    args.add(out.dirname, format = "--doc_out=%s")
    args.add(out.basename, format = "--doc_opt=markdown,%s")

    # Only the direct sources are rendered. Everything else is on the include
    # path so that imports resolve, not so that it lands on the page.
    args.add_all(proto_info.direct_sources, map_each = _import_name)

    ctx.actions.run(
        outputs = [out],
        inputs = proto_info.transitive_sources,
        executable = ctx.executable._protoc,
        tools = [ctx.executable._plugin],
        arguments = [args],
        mnemonic = "ProtoDoc",
        progress_message = "Generating protobuf reference for %{label}",
        toolchain = None,
    )
    return [DefaultInfo(files = depset([out]))]

proto_doc = rule(
    implementation = _proto_doc_impl,
    doc = "Renders `proto` as a markdown API reference named `<name>.md`.",
    attrs = {
        "proto": attr.label(
            doc = "The proto_library to document. Only its direct sources are rendered.",
            mandatory = True,
            providers = [ProtoInfo],
        ),
        "_plugin": attr.label(
            default = "//bazel/protodoc",
            cfg = "exec",
            executable = True,
        ),
        "_protoc": attr.label(
            default = "@protobuf//:protoc",
            cfg = "exec",
            executable = True,
        ),
    },
)
