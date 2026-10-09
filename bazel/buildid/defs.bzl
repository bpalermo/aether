"""Derive each released binary's GNU build-ID from its own content (#651, #653).

rules_go links every Go binary with `-buildid=redacted`, and since Go 1.24 the Go
linker derives `.note.gnu.build-id` from that constant by default — so every
aether Go binary carried one and the same GNU build-ID (#651). Stamping the note
with the release commit (#652) removed that collision but created another: one
commit produces many released ELFs, so all of them shared a single ID (#653).
Profilers that key symbols purely by build-ID (the Pyroscope eBPF symbolizer
does) then resolve every one of them against whichever binary's debuginfo was
uploaded — silently, with plausible-looking frames.

`content_build_id` rewrites the note after the link to
`sha1(image bytes with the 20-byte descriptor zeroed)` — the same thing
`ld --build-id=sha1` computes natively, which is what the custom Envoy now gets
from the linker. The descriptor is exactly a SHA-1 wide, so it is patched in
place and no other byte of the image moves.

Properties:
  * **pairwise distinct** — two ELFs that differ anywhere outside the descriptor
    get different IDs, so no two released binaries can collide;
  * **changes when the code changes** — a commit that alters a binary alters its
    ID; a commit that leaves a binary byte-identical keeps it, which is correct
    (its uploaded symbols are still the right ones, and the upload dedupes);
  * **reproducible** — the ID is a pure function of the input bytes, so the same
    inputs rebuild to the same ID and the remote cache stays warm;
  * **unconditional** — a content hash needs no workspace status, so there is no
    `--stamp` gating and no `ctx.info_file` input. Dev, PR-CI and release builds
    all produce correct, self-verifying IDs, and the action's cache key is just
    (input binary, tool). Which commit built a binary is answered outside
    it, by its image's signature, provenance and `dev-<sha>` tag (#1378).

Since #1378 the note is also the version each component reports about itself
(OTel `service.version`, `--version`, the CSI plugin's `vendor_version`):
`//common/buildinfo` reads it from the running executable.

`build_id_check` is the guard: it fails the build if a released binary has no
build-ID note, if a note does not match the hash recomputed from that binary's
own bytes, or if any two of them share an ID. It is not handed a list of
binaries (#1427): it is handed what the release pushes, and finds every
`content_build_id` output underneath, in the configuration it ships in.
"""

ContentBuildIdInfo = provider(
    doc = "A binary whose GNU build-ID was derived from its content by `content_build_id`.",
    fields = {
        "binary": "File: the rewritten binary, the one that goes into an image.",
        "platform": "string: the platform it was built for, as `<os>/<arch>` (Go's names).",
        "source": "Label: the binary it was made from.",
    },
)

def _target_platform(ctx):
    """The target platform as <os>/<arch>, `unknown` for what no image is built for."""

    def has(attr):
        return ctx.target_platform_has_constraint(attr[platform_common.ConstraintValueInfo])

    os = "unknown"
    if has(ctx.attr._os_linux):
        os = "linux"
    elif has(ctx.attr._os_macos):
        os = "darwin"
    arch = "unknown"
    if has(ctx.attr._cpu_x86_64):
        arch = "amd64"
    elif has(ctx.attr._cpu_arm64):
        arch = "arm64"
    return "{}/{}".format(os, arch)

def _content_build_id_impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name)
    binary = ctx.file.binary

    args = ctx.actions.args()
    args.add("set")
    args.add("-in", binary)
    args.add("-out", out)

    ctx.actions.run(
        executable = ctx.executable._tool,
        arguments = [args],
        inputs = [binary],
        outputs = [out],
        mnemonic = "ContentBuildID",
        progress_message = "Deriving the GNU build-ID of %{output} from its content",
    )

    return [
        DefaultInfo(
            files = depset([out]),
            executable = out,
            runfiles = ctx.runfiles().merge(ctx.attr.binary[DefaultInfo].default_runfiles),
        ),
        ContentBuildIdInfo(
            binary = out,
            platform = _target_platform(ctx),
            source = ctx.attr.binary.label,
        ),
    ]

content_build_id = rule(
    implementation = _content_build_id_impl,
    executable = True,
    doc = "Copies `binary`, rewriting its GNU build-ID note to a hash of its own content.",
    attrs = {
        "binary": attr.label(
            mandatory = True,
            allow_single_file = True,
            doc = "The binary to rewrite. Non-ELF inputs are copied unchanged.",
        ),
        "_cpu_arm64": attr.label(default = Label("@platforms//cpu:arm64")),
        "_cpu_x86_64": attr.label(default = Label("@platforms//cpu:x86_64")),
        "_os_linux": attr.label(default = Label("@platforms//os:linux")),
        "_os_macos": attr.label(default = Label("@platforms//os:macos")),
        "_tool": attr.label(
            default = Label("//bazel/buildid"),
            executable = True,
            cfg = "exec",
        ),
    },
)

_ShippedBinariesInfo = provider(
    doc = "Every `content_build_id` output a target ships.",
    fields = {"binaries": "depset of struct(binary, label, platform, source)"},
)

# The edges from what a release pushes down to the binaries, and no other:
#
#   helm_push_images.package -> helm_package.images -> image_push.image
#     -> image_index.manifests (one configuration per published platform)
#     -> image_manifest.layers -> image_layer.srcs -> content_build_id
#
# Not "*": the walk stops at a `content_build_id` target instead of going on
# into the Go dependency graph and the toolchains of every platform. The price
# is that a rule that renames one of these attributes drops what is under it
# from the walk, silently. //bazel/buildid:release_build_ids_test is what makes
# that loud: it compares what this aspect found with what `bazel query` (a
# genquery) finds under the same targets.
_SHIPPING_EDGES = ["package", "images", "image", "manifests", "layers", "srcs"]

def _targets(value):
    """The Targets in an attribute value: a label, a list of them, or a dict with them."""
    kind = type(value)
    if kind == "Target":
        return [value]
    if kind == "list":
        return [v for v in value if type(v) == "Target"]
    if kind == "dict":
        return [v for pair in value.items() for v in pair if type(v) == "Target"]
    return []

def _shipped_binaries_impl(target, ctx):
    direct = []
    if ContentBuildIdInfo in target:
        info = target[ContentBuildIdInfo]
        direct.append(struct(
            binary = info.binary,
            label = target.label,
            platform = info.platform,
            source = info.source,
        ))
    transitive = []
    for edge in _SHIPPING_EDGES:
        for dep in _targets(getattr(ctx.rule.attr, edge, None)):
            if _ShippedBinariesInfo in dep:
                transitive.append(dep[_ShippedBinariesInfo].binaries)
    return [_ShippedBinariesInfo(binaries = depset(direct, transitive = transitive))]

_shipped_binaries = aspect(
    implementation = _shipped_binaries_impl,
    attr_aspects = _SHIPPING_EDGES,
    doc = "Collects the `content_build_id` outputs under a target, along the edges an image is assembled by.",
)

def _shown(label):
    # //pkg:name for a label of this repository, as `bazel query` prints it.
    if label.repo_name:
        return str(label)
    return "//{}:{}".format(label.package, label.name)

def _named(shipped):
    # What the report shows for one binary. The tool splits at the first `=`.
    return "{} {} ({})={}".format(shipped.platform, _shown(shipped.label), _shown(shipped.source), shipped.binary.path)

def _build_id_check_impl(ctx):
    marker = ctx.actions.declare_file(ctx.label.name + ".txt")
    shipped = depset(transitive = [
        root[_ShippedBinariesInfo].binaries
        for root in ctx.attr.ships
    ]).to_list()
    if len(shipped) < 2:
        fail("build_id_check found {} binaries under `ships`: pairwise distinctness is the point, and none at all means the walk did not reach an image".format(len(shipped)))

    args = ctx.actions.args()
    args.add("verify")
    args.add("-marker", marker)
    args.add_all(shipped, map_each = _named)

    ctx.actions.run(
        executable = ctx.executable._tool,
        arguments = [args],
        inputs = [s.binary for s in shipped],
        outputs = [marker],
        mnemonic = "CheckBuildID",
        progress_message = "Checking the GNU build-IDs of the released binaries are self-verifying and distinct",
    )
    return [DefaultInfo(files = depset([marker]))]

build_id_check = rule(
    implementation = _build_id_check_impl,
    doc = "Fails the build unless the GNU build-ID of every binary shipped by " +
          "`ships` hashes its own content and no two of them are equal. The " +
          "binaries are every `content_build_id` output under those targets, " +
          "for every platform an image index is built for. The same binary " +
          "put into two images is two outputs with one ID, and fails the " +
          "check as any shared ID does.",
    attrs = {
        "ships": attr.label_list(
            mandatory = True,
            aspects = [_shipped_binaries],
            doc = "What the release pushes: targets whose images are walked for binaries.",
        ),
        "_tool": attr.label(
            default = Label("//bazel/buildid"),
            executable = True,
            cfg = "exec",
        ),
    },
)
