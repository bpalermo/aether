"""Stamp the GNU build-ID of a linked binary with the release commit SHA (#651).

rules_go links every Go binary with `-buildid=redacted`, and since Go 1.24 the Go
linker derives `.note.gnu.build-id` from that constant by default — so every
aether Go binary ever produced carries the same GNU build-ID. Profilers that key
symbols purely by build-ID (the Pyroscope eBPF symbolizer does) then attribute a
new release's samples to the previous release's symbol offsets, silently.

rules_go does not stamp-expand `gc_linkopts`, so the linker's `-B 0x<sha>` cannot
be fed from the workspace status. `stamped_build_id` instead rewrites the note
after the link: the descriptor is exactly 20 bytes, the width of a git commit
SHA-1, so the release commit drops in without moving any other byte.

Guarantees:
  * two different commits produce two different build-IDs;
  * rebuilding the same commit reproduces the same build-ID (the id comes from
    the *stable* workspace status, so the cache stays warm — only the copy action
    re-runs when the commit changes, never the link or the compiles);
  * `--nostamp` builds (every dev build, every PR CI build) get a byte-identical
    copy of the plain binary, so nothing about local iteration changes.

`build_id_check` is the guard: it fails the build if a released binary has no
build-ID note, or (under `--stamp`) if the note is not the release commit.
"""

# `--stamp` is a native Bazel option, so a plain `values` config_setting reads it
# — the same trick rules_go uses for its own stamping (@rules_go//go/private:stamp).
STAMP_ENABLED = "//tools/buildid:stamp_enabled"

def _status_file(ctx):
    """The STABLE workspace status file, or None on a --nostamp build.

    `ctx.info_file` is stable-status.txt; `ctx.version_file` is the *volatile*
    one. Only the stable file is cache-key material, which is precisely what is
    wanted here: the same commit rebuilds to the same build-ID and the same
    remote-cache entry, and a new commit re-runs the copy action alone.
    """
    return ctx.info_file if ctx.attr.stamp else None

def _stamped_build_id_impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name)
    binary = ctx.file.binary

    args = ctx.actions.args()
    args.add("stamp")
    args.add("-in", binary)
    args.add("-out", out)

    inputs = [binary]
    status = _status_file(ctx)
    if status:
        args.add("-status-file", status)
        inputs.append(status)

    ctx.actions.run(
        executable = ctx.executable._tool,
        arguments = [args],
        inputs = inputs,
        outputs = [out],
        mnemonic = "StampBuildID",
        progress_message = "Stamping GNU build-ID into %{output}",
    )

    return [DefaultInfo(
        files = depset([out]),
        executable = out,
        runfiles = ctx.runfiles().merge(ctx.attr.binary[DefaultInfo].default_runfiles),
    )]

stamped_build_id = rule(
    implementation = _stamped_build_id_impl,
    executable = True,
    doc = "Copies `binary`, rewriting its GNU build-ID note to the release commit SHA.",
    attrs = {
        "binary": attr.label(
            mandatory = True,
            allow_single_file = True,
            doc = "The binary to stamp. Non-ELF inputs are copied unchanged.",
        ),
        "stamp": attr.bool(
            default = False,
            doc = "Whether to read the commit from the stable workspace status. " +
                  "Set from a select() on //tools/buildid:stamp_enabled.",
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
    if not binaries:
        fail("build_id_check needs at least one binary")

    args = ctx.actions.args()
    args.add("verify")
    args.add("-marker", marker)

    inputs = list(binaries)
    status = _status_file(ctx)
    if status:
        args.add("-status-file", status)
        inputs.append(status)
    args.add_all(binaries)

    ctx.actions.run(
        executable = ctx.executable._tool,
        arguments = [args],
        inputs = inputs,
        outputs = [marker],
        mnemonic = "CheckBuildID",
        progress_message = "Checking GNU build-IDs of the released binaries",
    )
    return [DefaultInfo(files = depset([marker]))]

build_id_check = rule(
    implementation = _build_id_check_impl,
    doc = "Fails the build unless every binary carries the expected GNU build-ID.",
    attrs = {
        "binaries": attr.label_list(
            mandatory = True,
            allow_files = True,
            doc = "Stamped binaries to check.",
        ),
        "stamp": attr.bool(
            default = False,
            doc = "Whether to require the note to equal the release commit.",
        ),
        "_tool": attr.label(
            default = Label("//tools/buildid"),
            executable = True,
            cfg = "exec",
        ),
    },
)

def stamp_select():
    """select() that turns stamping on exactly when `--stamp` is passed."""
    return select({
        STAMP_ENABLED: True,
        "//conditions:default": False,
    })
