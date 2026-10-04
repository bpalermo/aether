"""Where aether's images and charts are published: ONE setting (proposal 040).

Every published coordinate is derived from the constants below: the image_push
targets (//bazel/img:go_multi_arch_image.bzl), the chart pushes (//charts/*,
chart_push in //bazel/helm:defs.bzl), the proxy's oci_push (the //proxy
workspace carries a byte-identical copy of this file at proxy/bazel/registry.bzl,
since it cannot load from this module), the aether-proxy pin parser
(//bazel/proxy_pin), the e2e go_test's default image references, and -- through
scripts/image-registry.sh, which parses THIS file -- every workflow, verifier
and e2e script. Nothing else may spell the registry out:
scripts/check-registry-config.sh fails CI on a literal anywhere outside its
allow-list.

History. Until the phase-2 cut-over (proposal 040) this file said

    IMAGE_REGISTRY = "ghcr.io"
    IMAGE_NAMESPACE = "bpalermo/aether"
    IMAGE_NAME_OVERRIDES = {"proxy": "aether-proxy"}
    CHART_REPOSITORY_PREFIX = "charts/"

and had no SIGNATURE_LAYOUT. Commits published before the cut-over live on
ghcr.io under those names and stay there: the publish-verify sweep reads THIS
FILE AS OF EACH PUSH HEAD (`git show <sha>:bazel/img/registry.bzl`), so an old
head is checked on ghcr.io and a new one on quay.io, with no date or sha typed
anywhere (scripts/verify-published-artifacts.sh).

scripts/image-registry.sh reads the assignments below with a strict,
line-anchored grep: keep each on ONE line, exactly `NAME = "value"` (the
overrides as a one-line dict of string pairs). Anything it cannot parse is a
hard failure, never a fallback.
"""

# Registry host.
IMAGE_REGISTRY = "quay.io"

# Namespace (org / path) under the host that every image and chart lives in.
IMAGE_NAMESPACE = "aethermesh"

# component -> repository basename, where the two differ. Empty on Quay: every
# component, the proxy included, is published under its own name
# (`<registry>/<namespace>/proxy`; on ghcr.io it was `aether-proxy`).
IMAGE_NAME_OVERRIDES = {}

# Prepended to a chart's name to form its repository under IMAGE_NAMESPACE.
# Ending in "/" makes it a path segment (ghcr.io was `.../charts/aether`);
# otherwise it is a flat-name prefix (Quay: `<namespace>/chart-aether`), which
# keeps the chart called `aether` off the org's most natural repository name and
# the prober/udsecho charts off their images' repositories. Quay has no nested
# repositories, so a flat prefix is the only shape it can hold, and `helm push`
# cannot name such a repository: charts are pushed by chart_push
# (//bazel/helm:defs.bzl, oras) to chart_registry_url(<chart>).
CHART_REPOSITORY_PREFIX = "chart-"

# Where cosign's keyless signature lands on IMAGE_REGISTRY, which the
# publish-verify sweep ASSERTS for every commit published under this setting:
# "referrer" (an OCI 1.1 referrer and no tag -- a registry that serves the
# Referrers API; quay.io, measured by the quay-smoke gate, run 36345331611) or
# "tag" (cosign 3's `sha256-<hex>` fallback tag, or cosign 2's `.sig` -- a
# registry without the API, as ghcr.io). A commit whose registry.bzl predates
# this line was published under "tag".
SIGNATURE_LAYOUT = "referrer"

def image_repository(component):
    """Repository path (no host) of a component's image.

    Args:
      component: the component name, e.g. "agent" or "proxy".

    Returns:
      e.g. "aethermesh/agent".
    """
    return "{}/{}".format(IMAGE_NAMESPACE, IMAGE_NAME_OVERRIDES.get(component, component))

def image_reference(component):
    """Host-qualified image repository of a component.

    Args:
      component: the component name, e.g. "agent".

    Returns:
      e.g. "quay.io/aethermesh/agent".
    """
    return "{}/{}".format(IMAGE_REGISTRY, image_repository(component))

def chart_repository(chart):
    """Repository path (no host) a chart is published under.

    Args:
      chart: the chart directory / Chart.yaml name, e.g. "aether".

    Returns:
      e.g. "aethermesh/chart-aether".
    """
    return "{}/{}{}".format(IMAGE_NAMESPACE, CHART_REPOSITORY_PREFIX, chart)

def chart_registry_url(chart):
    """The host-qualified OCI repository a chart is pushed to (no scheme, no tag).

    chart_push (//bazel/helm:defs.bzl) publishes `<this>:<chart version>` with
    oras, naming the repository outright. `helm push` cannot: it appends
    Chart.yaml's `name:` to whatever base it is handed, so a flat prefix
    (`chart-`) is unreachable with it, and Quay has no nested repositories to
    hold `charts/<name>`. Consumers pull the very same coordinate with
    `helm pull oci://<this> --version <X.Y.Z>`.

    Args:
      chart: the chart directory / Chart.yaml name, e.g. "aether".

    Returns:
      e.g. "quay.io/aethermesh/chart-aether".
    """
    return "{}/{}".format(IMAGE_REGISTRY, chart_repository(chart))

def registry_token_url(registry, repository):
    """The anonymous pull-token endpoint for <registry>/<repository>.

    Registry-specific: GHCR (and Docker's distribution) serve `/token`, Quay
    serves `/v2/auth`. Kept beside the constants so a registry flip cannot leave
    a token URL behind. scripts/registry-lib.sh has the shell twin.

    Args:
      registry: registry host, e.g. "quay.io".
      repository: repository path, e.g. "aethermesh/proxy".

    Returns:
      The token URL, scoped to pull on that repository.
    """
    path = "v2/auth" if registry == "quay.io" else "token"
    return "https://{r}/{p}?service={r}&scope=repository:{repo}:pull".format(
        r = registry,
        p = path,
        repo = repository,
    )
