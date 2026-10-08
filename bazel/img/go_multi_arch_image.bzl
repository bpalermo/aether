"""go_multi_arch_image: a Go binary as a multi-arch (amd64/arm64) distroless image."""

load("@aether_registry//:registry.bzl", "IMAGE_REGISTRY")
load("@bazel_skylib//rules:common_settings.bzl", "string_flag")
load("@container_structure_test//:defs.bzl", "container_structure_test")
load("@rules_img//img:image.bzl", "image_index", "image_manifest")
load("@rules_img//img:layer.bzl", "file_metadata", "image_layer")
load("@rules_img//img:load.bzl", "image_load")
load("@rules_img//img:push.bzl", "image_push")
load("//bazel/buildid:defs.bzl", "content_build_id")

# OCI provenance, on the config (labels) AND on the descriptors (annotations).
#
# #837: nothing we published carried any `org.opencontainers.image.*` of its
# own. rules_img inherits `org.opencontainers.image.base.{name,digest}` from the
# pulled base and nothing else, so a tool walking annotations for provenance
# either found nothing or -- on the rules_oci proxy image, whose distroless/cc
# base does carry one -- found `org.opencontainers.image.source` pointing at
# GoogleContainerTools/distroless. A confidently wrong answer, which is worse
# than an absent one. Setting `source` explicitly OVERRIDES that inherited value.
IMAGE_SOURCE_URL = "https://github.com/bpalermo/aether"

# NOTHING HERE MAY DEPEND ON THE COMMIT (#1378).
#
# #837 also put `org.opencontainers.image.revision` here, as a config label, a
# manifest annotation and an index annotation. Each of the three is part of the
# JSON document it sits in, so each one alone gave every image a new index
# digest with every commit (measured: 0 of 10 digests survived a docs-only
# commit, with any single one of them left in). The charts pin index digests, so
# every deploy rolled every workload, the node proxy included.
#
# An image is now a function of what is in it. Which commit built a digest is
# answered by things that are NOT part of the digest: the cosign signature's
# certificate, the provenance attestation and the per-commit `dev-<sha>` tag
# (docs/verifying-releases.md). There is no annotation placement that is free:
# only tags, referrers, signatures and attestations sit outside the digest.
_PROVENANCE = {
    "org.opencontainers.image.source": IMAGE_SOURCE_URL,
}

def go_multi_arch_image(name, binary, repository, registry = IMAGE_REGISTRY, base = "@distroless_static", container_test_configs = ["testdata/container_test.yaml"], tars_layer = None, extra_labels = {}):
    """
    Creates a containerized binary from Go sources.

    Every ELF that goes into the image passes through `content_build_id` first,
    which rewrites its GNU build-ID to a hash of its own bytes (#651, #653) so
    that no two released binaries share an ID. It is unconditional — a content
    hash needs no workspace status — so dev, PR-CI and release images all carry
    correct, self-verifying IDs and `bazel run`/`bazel test` behaviour is
    unchanged.

    Args:
        name: name of the image
        binary: go binary
        repository: image repository, from registry.bzl's image_repository()
        registry: image registry host (registry.bzl's IMAGE_REGISTRY; proposal
          040 -- do not pass a literal)
        base: base image
        container_test_configs: container-structure-test configs run against
          the image (`:image_test`)
        tars_layer: optional dict of in-image path -> binary label, shipped as
          one extra layer (each binary is content-build-ID'd like the main one)
        extra_labels: image-specific OCI config labels merged on top of the
          shared provenance set (which callers cannot drop).
    """
    binary_name = binary[1:]
    labels = dict(_PROVENANCE)
    labels.update(extra_labels)
    entrypoint = "/{}".format(binary_name)

    image_binary = "{}_buildid_binary".format(name)
    content_build_id(
        name = image_binary,
        binary = binary,
        # Public so //bazel/buildid:release_build_ids can assert on the exact
        # ELF that ships, not on a rebuild of it.
        visibility = ["//visibility:public"],
    )

    image_tars_layer = None
    if tars_layer:
        image_tars_layer = {}
        for i, path in enumerate(tars_layer.keys()):
            extra = "{}_buildid_extra_{}".format(name, i)
            content_build_id(
                name = extra,
                binary = tars_layer[path],
                visibility = ["//visibility:public"],
            )
            image_tars_layer[path] = ":" + extra

    string_flag(
        name = "release_tag",
        build_setting_default = "dev",
    )

    image_layer(
        name = "binary_layer",
        srcs = {
            entrypoint: ":" + image_binary,
        },
        default_metadata = file_metadata(
            mode = "0755",
        ),
        include_runfiles = False,
        compress = "zstd",  # Use zstd compression (optional, uses global default otherwise)
    )

    # Only declared when there is something to put in it. Declared
    # unconditionally, it left an empty `:additional_layer` in every image
    # package that passes no `tars_layer` — four targets nothing referenced
    # (image_manifest below already gates on `image_tars_layer`), which
    # `bazel build //...` still had to build.
    if image_tars_layer:
        image_layer(
            name = "additional_layer",
            srcs = image_tars_layer,
            default_metadata = file_metadata(
                mode = "0755",
            ),
            include_runfiles = False,
            compress = "zstd",  # Use zstd compression (optional, uses global default otherwise)
        )

    # No `stamp` (#1378). The labels and annotations hold no template, so
    # rules_img gives these actions no workspace-status input at all and the
    # config, the manifest and the index are identical in every build
    # configuration: `--stamp` or not, at this commit or the next. #837 set
    # `stamp = "force"` here for the `revision` label, which is gone.
    image_manifest(
        name = "image_manifest",
        base = base,
        layers = [":binary_layer", ":additional_layer"] if image_tars_layer else [":binary_layer"],
        visibility = ["//visibility:private"],
        entrypoint = [entrypoint],
        labels = labels,
        annotations = _PROVENANCE,
    )

    image_index(
        name = "image_index",
        manifests = [":image_manifest"],
        platforms = [
            "@rules_go//go/toolchain:linux_amd64",
            "@rules_go//go/toolchain:linux_arm64",
        ],
        visibility = ["//visibility:private"],
        # The index is the artifact the chart pins, so it carries the source
        # too: a tool that reads only the top-level descriptor never has to walk
        # into a per-platform manifest to answer "whose image is this?".
        annotations = _PROVENANCE,
    )

    # image_load uses image_index so the platform transition builds the Go
    # binary for Linux even when run from a macOS host.
    image_load(
        name = "image_load",
        image = ":image_index",
        # Registry-qualified tag so the locally loaded image matches its pushed
        # reference (<IMAGE_REGISTRY>/<image_repository(...)>:latest) — the e2e suite
        # kind-loads images by that full ref.
        tag = "{}/{}:{}".format(registry, repository, "latest"),
    )

    # Separate single-manifest load for the container structure test, which
    # requires a single-file tarball output group.
    image_load(
        name = "_image_load_test",
        image = ":image_manifest",
        tag = "{}:{}".format(repository, "test"),
        visibility = ["//visibility:private"],
    )

    native.filegroup(
        name = "image_tarball",
        srcs = [":_image_load_test"],
        output_group = "tarball",
    )

    container_structure_test(
        name = "image_test",
        driver = "tar",
        configs = container_test_configs,
        image = ":image_tarball",
        visibility = ["//visibility:private"],
    )

    image_push(
        name = "image_push",
        image = ":image_index",
        # Public so the Helm charts under //charts can reference the push targets
        # for image substitution and chart-with-images publishing.
        visibility = ["//visibility:public"],
        registry = registry,
        repository = repository,
        # The per-commit tag is where the commit lives now (#1378): a tag is not
        # part of the digest, so `dev-<sha>` can point a new commit at an
        # unchanged image. It reads the STABLE key, so the value it prints is
        # the one that re-runs this expansion. Bazel does not re-run an action
        # for a volatile key: with the volatile `.GIT_COMMIT` here the tag still
        # followed the commit, but only because a STABLE_ key changed in the
        # same status; with no commit-derived STABLE_ key a warm Bazel server
        # kept the PREVIOUS commit's tag (both measured). See the key's comment
        # in bazel/workspace_status.sh.
        tag_list = [
            "{{if (eq .GIT_BRANCH \"main\")}}dev{{else}}{{.tag}}{{end}}",
            "{{if .STABLE_GIT_COMMIT}}{{.tag}}-{{.STABLE_GIT_COMMIT}}{{end}}",
        ],
        build_settings = {
            "tag": ":release_tag",
        },
    )
