"""Pinned hermetic LLVM version — the single source of truth for the libclang
file paths below. Matches Envoy's hermetic toolchain pin (bazel/toolchains.bzl
_LLVM_VERSION_HERMETIC) and the abi.h/SDK era.

MODULE.bazel can't load() this, so the llvm.toolchain(llvm_version=...) string
there must be kept in lockstep with LLVM_VERSION.
"""

LLVM_VERSION = "18.1.8"

# Components of LLVM_VERSION used to address files inside @llvm_toolchain_llvm:
# the libclang soname chain (libclang.so -> .so.18.1 -> .so.18.1.8) and the
# clang resource dir (lib/clang/18).
LLVM_MAJOR = "18"

LLVM_MAJOR_MINOR = "18.1"
