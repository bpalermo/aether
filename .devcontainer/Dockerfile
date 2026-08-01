# syntax=docker/dockerfile:1
#
# Aether dev container image.
#
# Matches the CI runner (ubuntu-24.04). The language/build toolchains
# (Bazel, Go, Node, kubectl, helm, kind, Docker, Claude Code) are layered on
# via devcontainer "features" in devcontainer.json — this Dockerfile only
# carries the OS-level bits that have no clean feature equivalent.
FROM mcr.microsoft.com/devcontainers/base:ubuntu-24.04

# Base packages used directly by the repo workflow:
#   - openssl:        e2e cert generation (e2e/Makefile)
#   - make/git/curl:  Makefile targets, dependency fetch, repo ops
#   - build-essential/pkg-config: host C toolchain for any cgo / local tooling
ARG DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        curl \
        git \
        make \
        openssl \
        pkg-config \
        unzip \
    && rm -rf /var/lib/apt/lists/*

# libtinfo5 for the hermetic toolchains_llvm clang.
#
# The pinned LLVM 18.1.8 static build leaks a shared dependency on
# libtinfo.so.5 (llvm/llvm-project#75490), but ubuntu-24.04 ships only
# libtinfo.so.6 (a .6->.5 symlink fails: clang needs the versioned
# NCURSES_TINFO_5.0 symbols). Fetch the real ncurses5 .deb from Debian —
# the same workaround used in CI (.github/actions/setup-llvm-libs) and by
# Envoy (envoyproxy/envoy#42397). See //bazel/llvm and MODULE.bazel.
RUN arch="$(dpkg --print-architecture)" \
    && deb="libtinfo5_6.4-4_${arch}.deb" \
    && curl -fsSLO "http://deb.debian.org/debian/pool/main/n/ncurses/${deb}" \
    && apt-get update \
    && apt-get install -y --no-install-recommends "./${deb}" \
    && rm -f "${deb}" \
    && rm -rf /var/lib/apt/lists/*
