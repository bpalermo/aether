load("@bazel_skylib//rules:common_settings.bzl", "string_flag")
load("@container_structure_test//:defs.bzl", "container_structure_test")
load("@rules_img//img:image.bzl", "image_index", "image_manifest")
load("@rules_img//img:layer.bzl", "file_metadata", "image_layer")
load("@rules_img//img:load.bzl", "image_load")
load("@rules_img//img:push.bzl", "image_push")

def go_multi_arch_image(name, binary, repository, registry = "docker.io", base = "@distroless_static", container_test_configs = ["testdata/container_test.yaml"], tars_layer = None):
    """
    Creates a containerized binary from Go sources.
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

    string_flag(
        name = "release_tag",
        build_setting_default = "dev",
    )

    image_layer(
        name = "binary_layer",
        srcs = {
            entrypoint: binary,
        },
        default_metadata = file_metadata(
            mode = "0755",
        ),
        include_runfiles = False,
        compress = "zstd",  # Use zstd compression (optional, uses global default otherwise)
    )

    image_layer(
        name = "additional_layer",
        srcs = tars_layer,
        default_metadata = file_metadata(
            mode = "0755",
        ),
        include_runfiles = False,
        compress = "zstd",  # Use zstd compression (optional, uses global default otherwise)
    )

    image_manifest(
        name = "image_manifest",
        base = base,
        layers = [":binary_layer", ":additional_layer"] if tars_layer and len(tars_layer) > 0 else [":binary_layer"],
        visibility = ["//visibility:private"],
        entrypoint = [entrypoint],
    )

    image_load(
        name = "image_load",
        image = ":image_manifest",
        tag = "{}:{}".format(repository, "latest"),
    )

    native.filegroup(
        name = "image_tarball",
        srcs = [":image_load"],
        output_group = "tarball",
    )

    container_structure_test(
        name = "image_test",
        driver = "tar",
        configs = container_test_configs,
        image = ":image_tarball",
        visibility = ["//visibility:private"],
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

    image_push(
        name = "image_push",
        image = ":image_index",
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
