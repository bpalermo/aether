"""Derive each released binary's GNU build-ID from its own content (#651, #653).

rules_go links every Go binary with `-buildid=redacted`, and since Go 1.24 the Go
linker derives `.note.gnu.build-id` from that constant by default — so every
aether Go binary carried one and the same GNU build-ID (#651). Stamping the note
with the release commit (#652) removed that collision but created another: one
commit produces seven released ELFs, so all seven shared a single ID (#653).
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
    (input binary, tool). Release traceability lives in the `--stamp` x_defs
    version information and the image tags, not in the ELF note.

`build_id_check` is the guard: it fails the build if a released binary has no
build-ID note, if a note does not match the hash recomputed from that binary's
own bytes, or if any two of them share an ID.
"""

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

    return [DefaultInfo(
        files = depset([out]),
        executable = out,
        runfiles = ctx.runfiles().merge(ctx.attr.binary[DefaultInfo].default_runfiles),
    )]

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
        "_tool": attr.label(
            default = Label("//tools/buildid"),
            executable = True,
            cfg = "exec",
        ),
    },
)

def _build_id_check_impl(ctx):
    marker = ctx.actions.declare_file(ctx.label.name + ".txt")
    binaries = ctx.files.binaries
    if len(binaries) < 2:
        fail("build_id_check needs at least two binaries — pairwise distinctness is the point")

    args = ctx.actions.args()
    args.add("verify")
    args.add("-marker", marker)
    args.add_all(binaries)

    ctx.actions.run(
        executable = ctx.executable._tool,
        arguments = [args],
        inputs = binaries,
        outputs = [marker],
        mnemonic = "CheckBuildID",
        progress_message = "Checking the GNU build-IDs of the released binaries are self-verifying and distinct",
    )
    return [DefaultInfo(files = depset([marker]))]

build_id_check = rule(
    implementation = _build_id_check_impl,
    doc = "Fails the build unless every binary's GNU build-ID hashes its own " +
          "content and no two of them are equal.",
    attrs = {
        "binaries": attr.label_list(
            mandatory = True,
            allow_files = True,
            doc = "The released binaries to check.",
        ),
        "_tool": attr.label(
            default = Label("//tools/buildid"),
            executable = True,
            cfg = "exec",
        ),
    },
)
