"""Extracts the Envoy binary from the chart-pinned aether-proxy image.

`//test/envoy_validate` runs `envoy --mode validate` over aether-generated
bootstrap configs. That gate is only meaningful against the binary the mesh
actually runs: the custom proxy (//proxy workspace) compiles a large set of
upstream extensions *out* (`_DROPPED` in
proxy/bazel/build_config/extensions_build_config.bzl) and compiles the C++
`aether_stats` extension *in*, so a stock Envoy release binary is strictly more
permissive and validates configs the real proxy would NACK (aether #709).

There is no stock release asset that matches the proxy while it tracks an Envoy
`main` snapshot, so this extension takes the only binary that is always exactly
in sync with the data plane: the one inside the published aether-proxy image, at
the digest the chart pins.

Single source of truth
----------------------
`charts/aether/values.yaml` (`proxy.image.repository` / `proxy.image.digest`).
MODULE.bazel cannot `load()` a .bzl file, but a module extension can read a
source file, so the pin lives in exactly one place: when proxy-release's
`bump-chart` job rewrites the chart's `tag:` + `digest:`, this gate moves with
it and there is no second pin to keep in sync.

Parsing contract (deliberately strict — it fails the build rather than silently
validating against the wrong binary):

  * Find the first line of the form `repository: <ref>` whose `<ref>` names the
    `aether-proxy` image (last path segment is exactly `aether-proxy`).
  * Starting from the line after it, scan forward while still inside that
    mapping (indentation >= the `repository:` key's indentation; blank lines and
    comment-only lines do not end the block) for a line `digest: "sha256:<64
    hex>"`.
  * Anything else — no such `repository:`, no `digest:` inside its block, or a
    malformed digest — is a hard `fail()` naming what was expected.

Registry access
---------------
`ghcr.io/bpalermo/aether/aether-proxy` is a public package: an anonymous GHCR
bearer token is enough, and neither CI nor a local `bazel test` needs
credentials. If it is ever made private, `docker login ghcr.io` locally and a
`docker/login-action@v3` step in the CI `test` job would be required; the
`_bearer_token` failure path says so.
"""

_ENVOY_PATH_IN_IMAGE = "usr/local/bin/envoy"

_MANIFEST_ACCEPT = ",".join([
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
])

_INDEX_MEDIA_TYPES = [
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
]

_VALUES_LABEL = "//charts/aether:values.yaml"

_HEX = "0123456789abcdef"

def _indent_of(line):
    return len(line) - len(line.lstrip(" "))

def _scalar(raw):
    """Strips an inline comment and surrounding quotes off a YAML scalar."""
    if "#" in raw:
        raw = raw.split("#")[0]
    raw = raw.strip()
    for quote in ["\"", "'"]:
        if len(raw) >= 2 and raw.startswith(quote) and raw.endswith(quote):
            raw = raw[1:-1]
    return raw.strip()

def _is_sha256_digest(digest):
    if not digest.startswith("sha256:"):
        return False
    hexpart = digest[len("sha256:"):]
    if len(hexpart) != 64:
        return False
    for c in hexpart.elems():
        if c not in _HEX:
            return False
    return True

def _parse_proxy_pin(mctx):
    """Reads the aether-proxy repository + digest out of the chart's values.yaml."""
    content = mctx.read(Label(_VALUES_LABEL), watch = "yes")
    lines = content.splitlines()

    repository = None
    digest = None
    for i in range(len(lines)):
        stripped = lines[i].strip()
        if not stripped.startswith("repository:"):
            continue
        candidate = _scalar(stripped[len("repository:"):])
        if candidate.split("/")[-1] != "aether-proxy":
            continue

        repository = candidate
        key_indent = _indent_of(lines[i])
        for j in range(i + 1, len(lines)):
            inner = lines[j].strip()
            if inner == "" or inner.startswith("#"):
                continue
            if _indent_of(lines[j]) < key_indent:
                # Dedented out of the image mapping without finding a digest.
                break
            if inner.startswith("digest:"):
                digest = _scalar(inner[len("digest:"):])
                break
        break

    if repository == None:
        fail((
            "{label}: could not find the aether-proxy image pin. Expected a line " +
            "`repository: <registry>/<path>/aether-proxy`. //test/envoy_validate " +
            "validates against the Envoy inside that image, so the pin cannot be " +
            "guessed (aether #709)."
        ).format(label = _VALUES_LABEL))
    if digest == None:
        fail((
            "{label}: found `repository: {repo}` but no `digest:` key inside the " +
            "same image mapping. //test/envoy_validate pulls the proxy image by " +
            "digest; a tag-only pin is not accepted (aether #709)."
        ).format(label = _VALUES_LABEL, repo = repository))
    if not _is_sha256_digest(digest):
        fail((
            "{label}: the aether-proxy `digest:` is \"{digest}\", which is not a " +
            "`sha256:<64 hex chars>` reference."
        ).format(label = _VALUES_LABEL, digest = digest))

    return struct(
        registry = _registry_of(repository),
        repository = _repository_of(repository),
        digest = digest,
    )

def _registry_of(ref):
    head = ref.split("/")[0]
    if "/" in ref and ("." in head or ":" in head or head == "localhost"):
        return head
    return "index.docker.io"

def _repository_of(ref):
    head = ref.split("/")[0]
    if "/" in ref and ("." in head or ":" in head or head == "localhost"):
        return ref[len(head) + 1:]
    if "/" in ref:
        return ref
    return "library/" + ref

def _bearer_token(rctx):
    """Fetches an anonymous pull token; None when the registry needs no auth."""
    url = "https://{registry}/token?service={registry}&scope=repository:{repo}:pull".format(
        registry = rctx.attr.registry,
        repo = rctx.attr.repository,
    )
    result = rctx.download(url = url, output = "token.json", allow_fail = True)
    if not result.success:
        return None
    parsed = json.decode(rctx.read("token.json"))
    rctx.delete("token.json")
    return parsed.get("token") or parsed.get("access_token")

def _auth_headers(token):
    headers = {"Accept": _MANIFEST_ACCEPT}
    if token:
        headers["Authorization"] = "Bearer " + token
    return headers

def _reference_url(rctx, kind, digest):
    return "https://{registry}/v2/{repo}/{kind}/{digest}".format(
        registry = rctx.attr.registry,
        repo = rctx.attr.repository,
        kind = kind,
        digest = digest,
    )

def _fetch_manifest(rctx, token, digest, what):
    url = _reference_url(rctx, "manifests", digest)
    result = rctx.download(
        url = url,
        output = "manifest.json",
        sha256 = digest[len("sha256:"):],
        headers = _auth_headers(token),
        allow_fail = True,
    )
    if not result.success:
        fail((
            "could not fetch the {what} for {registry}/{repo}@{digest}.\n" +
            "If that package is private, authenticate first:\n" +
            "  locally:  docker login ghcr.io\n" +
            "  in CI:    a docker/login-action@v3 step with `packages: read`\n" +
            "The pin comes from {label} (aether #709)."
        ).format(
            what = what,
            registry = rctx.attr.registry,
            repo = rctx.attr.repository,
            digest = digest,
            label = _VALUES_LABEL,
        ))
    manifest = json.decode(rctx.read("manifest.json"))
    rctx.delete("manifest.json")
    return manifest

def _manifest_for_platform(rctx, token):
    """Resolves the pinned reference down to the single-arch manifest we want."""
    root = _fetch_manifest(rctx, token, rctx.attr.digest, "image index")
    if root.get("mediaType") not in _INDEX_MEDIA_TYPES:
        # Already a single-architecture manifest.
        return root

    wanted = None
    available = []
    for entry in root.get("manifests", []):
        platform = entry.get("platform", {})
        key = "{}/{}".format(platform.get("os", "?"), platform.get("architecture", "?"))
        available.append(key)
        if platform.get("os") == rctx.attr.os and platform.get("architecture") == rctx.attr.architecture:
            wanted = entry
    if wanted == None:
        fail("{registry}/{repo}@{digest} has no {os}/{arch} manifest (index has: {available})".format(
            registry = rctx.attr.registry,
            repo = rctx.attr.repository,
            digest = rctx.attr.digest,
            os = rctx.attr.os,
            arch = rctx.attr.architecture,
            available = ", ".join(available),
        ))
    return _fetch_manifest(rctx, token, wanted["digest"], "{}/{} manifest".format(rctx.attr.os, rctx.attr.architecture))

def _extract_envoy(rctx, token, manifest):
    """Walks the layers topmost-first and pulls out /usr/local/bin/envoy."""
    layers = manifest.get("layers", [])
    for index in range(len(layers) - 1, -1, -1):
        layer = layers[index]
        archive = "layer.tar.gz" if layer.get("mediaType", "").endswith("+gzip") else "layer.tar"
        result = rctx.download(
            url = _reference_url(rctx, "blobs", layer["digest"]),
            output = archive,
            sha256 = layer["digest"][len("sha256:"):],
            headers = _auth_headers(token),
            allow_fail = True,
        )
        if not result.success:
            fail("could not fetch layer {} of {}@{}".format(
                layer["digest"],
                rctx.attr.repository,
                rctx.attr.digest,
            ))
        rctx.extract(archive = archive, output = "layer")
        rctx.delete(archive)
        candidate = rctx.path("layer/" + _ENVOY_PATH_IN_IMAGE)
        if candidate.exists:
            rctx.execute(["mv", str(candidate), "envoy"])
            rctx.delete("layer")
            rctx.execute(["chmod", "+x", "envoy"])
            return
        rctx.delete("layer")

    fail("no layer of {registry}/{repo}@{digest} ({os}/{arch}) contains /{path}".format(
        registry = rctx.attr.registry,
        repo = rctx.attr.repository,
        digest = rctx.attr.digest,
        os = rctx.attr.os,
        arch = rctx.attr.architecture,
        path = _ENVOY_PATH_IN_IMAGE,
    ))

def _proxy_envoy_impl(rctx):
    token = _bearer_token(rctx)
    manifest = _manifest_for_platform(rctx, token)
    _extract_envoy(rctx, token, manifest)
    rctx.file(
        "BUILD.bazel",
        content = """\
# Generated by //bazel/proxy_pin:extensions.bzl — the Envoy binary extracted from
# {registry}/{repo}@{digest} ({os}/{arch}), the aether-proxy image pinned by
# //charts/aether:values.yaml.
exports_files(
    ["envoy"],
    visibility = ["//visibility:public"],
)
""".format(
            registry = rctx.attr.registry,
            repo = rctx.attr.repository,
            digest = rctx.attr.digest,
            os = rctx.attr.os,
            arch = rctx.attr.architecture,
        ),
    )

_proxy_envoy = repository_rule(
    implementation = _proxy_envoy_impl,
    doc = "Extracts /usr/local/bin/envoy from a digest-pinned OCI image for one platform.",
    attrs = {
        "architecture": attr.string(mandatory = True, doc = "OCI platform.architecture, e.g. amd64."),
        "digest": attr.string(mandatory = True, doc = "sha256 digest of the image index or manifest."),
        "os": attr.string(default = "linux", doc = "OCI platform.os."),
        "registry": attr.string(mandatory = True, doc = "Registry host, e.g. ghcr.io."),
        "repository": attr.string(mandatory = True, doc = "Repository path within the registry."),
    },
)

def _pinned_proxy_impl(mctx):
    pin = _parse_proxy_pin(mctx)
    for arch in ["amd64", "arm64"]:
        _proxy_envoy(
            name = "pinned_envoy_linux_" + arch,
            architecture = arch,
            digest = pin.digest,
            registry = pin.registry,
            repository = pin.repository,
        )
    return mctx.extension_metadata(reproducible = True)

pinned_proxy = module_extension(
    implementation = _pinned_proxy_impl,
    doc = """Declares @pinned_envoy_linux_{amd64,arm64}.

Each repo exposes `:envoy` — /usr/local/bin/envoy lifted out of the aether-proxy
image at the digest //charts/aether:values.yaml pins. Used by
//test/envoy_validate so the `envoy --mode validate` gate runs the exact binary
the mesh deploys.""",
)
