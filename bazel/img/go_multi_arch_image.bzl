load("@bazel_skylib//rules:common_settings.bzl", "string_flag")
load("@container_structure_test//:defs.bzl", "container_structure_test")
load("@rules_img//img:image.bzl", "image_index", "image_manifest")
load("@rules_img//img:layer.bzl", "file_metadata", "image_layer")
load("@rules_img//img:load.bzl", "image_load")
load("@rules_img//img:push.bzl", "image_push")
load("//tools/buildid:defs.bzl", "content_build_id")

def go_multi_arch_image(name, binary, repository, registry = "ghcr.io", base = "@distroless_static", container_test_configs = ["testdata/container_test.yaml"], tars_layer = None):
    """
    Creates a containerized binary from Go sources.

    Every ELF that goes into the image passes through `content_build_id` first,
    which rewrites its GNU build-ID to a hash of its own bytes (#651, #653) so
    that no two released binaries share an ID. It is unconditional — a content
    hash needs no workspace status — so dev, PR-CI and release images all carry
    correct, self-verifying IDs and `bazel run`/`bazel test` behaviour is
    unchanged.

    Parameters:
        name:  name of the image
        binary:  go binary
        repository: image repository
        registry: image registry
        base: base image
        tars: additional image layers
    """
    binary_name = binary[1:]
    entrypoint = "/{}".format(binary_name)

    image_binary = "{}_buildid_binary".format(name)
    content_build_id(
        name = image_binary,
        binary = binary,
        # Public so //tools/buildid:release_build_ids can assert on the exact
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

    image_layer(
        name = "additional_layer",
        srcs = image_tars_layer,
        default_metadata = file_metadata(
            mode = "0755",
        ),
        include_runfiles = False,
        compress = "zstd",  # Use zstd compression (optional, uses global default otherwise)
    )

    image_manifest(
        name = "image_manifest",
        base = base,
        layers = [":binary_layer", ":additional_layer"] if image_tars_layer else [":binary_layer"],
        visibility = ["//visibility:private"],
        entrypoint = [entrypoint],
    )

    image_index(
        name = "image_index",
        manifests = [":image_manifest"],
        platforms = [
            "@rules_go//go/toolchain:linux_amd64",
            "@rules_go//go/toolchain:linux_arm64",
        ],
        visibility = ["//visibility:private"],
    )

    # image_load uses image_index so the platform transition builds the Go
    # binary for Linux even when run from a macOS host.
    image_load(
        name = "image_load",
        image = ":image_index",
        # Registry-qualified tag so the locally loaded image matches its pushed
        # reference (e.g. ghcr.io/bpalermo/aether/agent:latest) — the e2e suite
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
        tag_list = [
            "{{if (eq .GIT_BRANCH \"main\")}}dev{{else}}{{.tag}}{{end}}",
            "{{if .GIT_COMMIT}}{{.tag}}-{{.GIT_COMMIT}}{{end}}",
        ],
        build_settings = {
            "tag": ":release_tag",
        },
    )
