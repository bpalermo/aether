load("@rules_oci//oci:pull.bzl", "oci_pull")

# Base for the custom aether-proxy image. distroless/cc ships libstdc++ +
# libgcc_s + glibc, which the Envoy binary and the aether_stats Rust dynamic
# module both need at runtime (the Envoy distroless image is built on
# distroless/base and lacks libgcc_s, so the module's unwinder fails to load
# there). debian12 matches the Envoy release's glibc. Digest carried over from
# the aether root MODULE.bazel @distroless_cc pin.
def oci_images():
    oci_pull(
        name = "distroless_cc",
        digest = "sha256:b0ae8e989418b458e0f25489bc3be523718938a2b70864cc0f6a00af1ddbd985",
        image = "gcr.io/distroless/cc-debian12",
        platforms = [
            "linux/amd64",
            "linux/arm64",
        ],
    )
