"""Where aether's images and charts are published: ONE setting (proposal 040).

Every published coordinate is derived from the constants below: the image_push
targets (//bazel/img:go_multi_arch_image.bzl), the chart pushes (//charts/*),
the proxy's oci_push (the //proxy workspace carries a byte-identical copy of
this file at proxy/bazel/registry.bzl, since it cannot load from this module),
the aether-proxy pin parser (//bazel/proxy_pin), the e2e go_test's default image
references, and -- through scripts/image-registry.sh, which parses THIS file --
every workflow, verifier and e2e script. Nothing else may spell the registry
out: scripts/check-registry-config.sh fails CI on a literal anywhere outside its
allow-list.

The phase-2 cut-over to Quay (proposal 040) is this edit:

    IMAGE_REGISTRY = "quay.io"
    IMAGE_NAMESPACE = "aethermesh"
    IMAGE_NAME_OVERRIDES = {}
    CHART_REPOSITORY_PREFIX = "chart-"

plus the same text in proxy/bazel/registry.bzl, and the data the check then
names (the aether-proxy pin in charts/aether/values.yaml, a chart major bump).

scripts/image-registry.sh reads the four assignments below with a strict,
line-anchored grep: keep each on ONE line, exactly `NAME = "value"` (the
overrides as a one-line dict of string pairs). Anything it cannot parse is a
hard failure, never a fallback.
"""

# Registry host.
IMAGE_REGISTRY = "ghcr.io"

# Namespace (org / path) under the host that every image and chart lives in.
IMAGE_NAMESPACE = "bpalermo/aether"

# component -> repository basename, where the two differ. GHCR history: the
# proxy image predates the flat names and is `aether-proxy`; on Quay it is plain
# `proxy` like every other component, so phase 2 empties this.
IMAGE_NAME_OVERRIDES = {"proxy": "aether-proxy"}

# Prepended to a chart's name to form its repository under IMAGE_NAMESPACE.
# Ending in "/" makes it a path segment (GHCR: `bpalermo/aether/charts/aether`);
# otherwise it is a flat-name prefix (Quay: `aethermesh/chart-aether`), which
# keeps the chart called `aether` off the org's most natural repository name.
CHART_REPOSITORY_PREFIX = "charts/"

def image_repository(component):
    """Repository path (no host) of a component's image.

    Args:
      component: the component name, e.g. "agent" or "proxy".

    Returns:
      e.g. "bpalermo/aether/agent".
    """
    return "{}/{}".format(IMAGE_NAMESPACE, IMAGE_NAME_OVERRIDES.get(component, component))

def image_reference(component):
    """Host-qualified image repository of a component.

    Args:
      component: the component name, e.g. "agent".

    Returns:
      e.g. "ghcr.io/bpalermo/aether/agent".
    """
    return "{}/{}".format(IMAGE_REGISTRY, image_repository(component))

def chart_repository(chart):
    """Repository path (no host) a chart is published under.

    Args:
      chart: the chart directory / Chart.yaml name, e.g. "aether".

    Returns:
      e.g. "bpalermo/aether/charts/aether".
    """
    return "{}/{}{}".format(IMAGE_NAMESPACE, CHART_REPOSITORY_PREFIX, chart)

def chart_registry_url():
    """The `oci://` base that `helm push` appends a chart's name to.

    helm derives the last path segment from Chart.yaml's `name:` and nothing
    else, so this works only while CHART_REPOSITORY_PREFIX is a path segment. A
    flat prefix (`chart-`) needs a push that can name the repository -- a phase-2
    open question in proposal 040 -- and fails loudly here until then, rather
    than publishing charts under names the verifiers do not expect.

    Returns:
      e.g. "oci://ghcr.io/bpalermo/aether/charts".
    """
    if not CHART_REPOSITORY_PREFIX.endswith("/"):
        fail("CHART_REPOSITORY_PREFIX \"%s\" is a flat-name prefix: `helm push` cannot publish chart <name> as <namespace>/%s<name>. See docs/proposals/040_quay-registry.md (phase 2)." % (CHART_REPOSITORY_PREFIX, CHART_REPOSITORY_PREFIX))
    return "oci://{}/{}/{}".format(IMAGE_REGISTRY, IMAGE_NAMESPACE, CHART_REPOSITORY_PREFIX.rstrip("/"))

def registry_token_url(registry, repository):
    """The anonymous pull-token endpoint for <registry>/<repository>.

    Registry-specific: GHCR (and Docker's distribution) serve `/token`, Quay
    serves `/v2/auth`. Kept beside the constants so a registry flip cannot leave
    a token URL behind. scripts/registry-lib.sh has the shell twin.

    Args:
      registry: registry host, e.g. "ghcr.io".
      repository: repository path, e.g. "bpalermo/aether/aether-proxy".

    Returns:
      The token URL, scoped to pull on that repository.
    """
    path = "v2/auth" if registry == "quay.io" else "token"
    return "https://{r}/{p}?service={r}&scope=repository:{repo}:pull".format(
        r = registry,
        p = path,
        repo = repository,
    )
