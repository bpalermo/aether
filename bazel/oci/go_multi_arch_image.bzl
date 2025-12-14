load("@aspect_bazel_lib//lib:testing.bzl", "assert_archive_contains")
load("@container_structure_test//:defs.bzl", "container_structure_test")
load("@rules_oci//oci:defs.bzl", "oci_image", "oci_image_index", "oci_load", "oci_push")
load("@rules_pkg//:pkg.bzl", "pkg_tar")
load("//bazel/oci:transition.bzl", "multi_arch")

def go_multi_arch_image(name, binary, repository, base = "@distroless_static", container_test_configs = ["testdata/container_test.yaml"], tars = [], **kwargs):
    """
    Creates a containerized binary from Go sources.
    Parameters:
        name:  name of the image
        binary:  go binary
        repository: image repository
        base: base image
        tars: additional image layers
        kwargs: arguments passed to the go_binary target
    """
    binary_name = binary[1:]

    pkg_tar(
        name = "layer",
        srcs = [binary],
        package_dir = "/",
        visibility = ["//visibility:private"],
        mode = "755",
    )

    assert_archive_contains(
        name = "layer_test",
        archive = "layer.tar",
        expected = [binary_name],
        visibility = ["//visibility:private"],
    )

    oci_image(
        name = "image",
        base = base,
        entrypoint = ["/{}".format(binary_name)],
        tars = tars + [":layer"],
        visibility = ["//visibility:private"],
    )

    container_structure_test(
        name = "image_test",
        configs = container_test_configs,
        image = ":image",
        tags = ["requires-docker"],
        visibility = ["//visibility:private"],
    )

    multi_arch(
        name = "images",
        image = ":image",
        platforms = [
            "@rules_go//go/toolchain:linux_amd64",
            "@rules_go//go/toolchain:linux_arm64",
        ],
        visibility = ["//visibility:private"],
    )

    oci_image_index(
        name = "index",
        images = [
            ":images",
        ],
    )

    oci_load(
        name = "load",
        image = ":image",
        repo_tags = ["{}:{}".format(repository, "latest")],
    )

    oci_push(
        name = "push",
        image = ":index",
        repository = repository,
    )
