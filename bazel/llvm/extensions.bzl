"""Hermetic glibc sysroots for the LLVM C/C++ toolchain (and the proxy SDK's
bindgen).

Chromium's prebuilt Debian **bullseye** sysroots (glibc 2.31) — older than the
Envoy distroless proxy base (glibc 2.36), so the dynamic-module `.so` links
against an old-enough glibc to load there (the reason the previous Zig toolchain
targeted an old glibc). Kept out of MODULE.bazel to keep it clean.

The download URL is content-addressed: `<base>/<sha256>` (see chromium's
build/linux/sysroot_scripts/install-sysroot.py). To bump: update the Sha256Sum
from chromium's sysroots.json — the URL follows.
"""

load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")

_SYSROOT_BASE = "https://commondatastorage.googleapis.com/chrome-linux-sysroot/"

# The archive extracts a real sysroot at its root (usr/include, lib/..., with
# only relative symlinks), so the repo root *is* the sysroot: a filegroup whose
# package path toolchains_llvm uses for --sysroot, plus the stdint.h marker the
# bindgen build script three-parents up to recover that root (execpath yields a
# file, not a dir).
_SYSROOT_BUILD = """\
filegroup(
    name = "sysroot",
    # lib/systemd has unit files with backslash-escaped names ('\\x2d') that are
    # invalid Bazel target names; they are irrelevant to a compile/link sysroot.
    srcs = glob(["**"], exclude = ["BUILD.bazel", "lib/systemd/**"]),
    visibility = ["//visibility:public"],
)

exports_files(["usr/include/stdint.h"])
"""

# Debian bullseye sysroots, sha256 == the content-addressed URL path segment.
_SYSROOTS = {
    "sysroot_linux_amd64": "52d61d4446ffebfaa3dda2cd02da4ab4876ff237853f46d273e7f9b666652e1d",
    "sysroot_linux_arm64": "c7176a4c7aacbf46bda58a029f39f79a68008d3dee6518f154dcf5161a5486d8",
}

def _sysroots_impl(_module_ctx):
    for name, sha in _SYSROOTS.items():
        http_archive(
            name = name,
            build_file_content = _SYSROOT_BUILD,
            sha256 = sha,
            # The content-addressed URL has no extension; declare the type.
            type = "tar.xz",
            urls = [_SYSROOT_BASE + sha],
        )

sysroots = module_extension(implementation = _sysroots_impl)
