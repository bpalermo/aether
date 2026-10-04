# Helm charts

Bazel-built Helm charts for Aether, using
[`rules_helm`](https://registry.bazel.build/modules/rules_helm).

| Chart | Path | Deploys |
| --- | --- | --- |
| `crds` | [`charts/crds`](./crds) | Aether CustomResourceDefinitions (`MeshConfig`, `HTTPFilter`, `EdgeConfig`, `EndpointPolicy`). Install **first**; standalone so CRDs can be upgraded independently. |
| `aether` | [`charts/aether`](./aether) | The whole system: agent DaemonSet (xDS + CNI install) + per-node Envoy proxy + the `mesh-dns` DaemonSet, registrar Deployment, the controller (validating webhooks for `MeshConfig`/`HTTPFilter`/`EdgeConfig`/`EndpointPolicy`/`HTTPRoute`, a pod-mutating webhook, and the `MeshConfig`→ConfigMap reconciler), and the optional north-south edge. Owns the `aether-system` namespace and RBAC. |
| `prober` | [`charts/prober`](./prober) | External mesh-availability prober (proposal 013). |
| `udsecho` | [`charts/udsecho`](./udsecho) | UDS validation workloads (proposal 034): both socket-delivery paths (annotation + `EndpointPolicy`) under continuous mesh traffic. |

Agent, proxy, mesh-dns, registrar and controller deploy together as one system
from the single `aether` chart — system config (OTEL, SPIRE, mesh domain) is set once at the top
level of its `values.yaml` and inherited by every component. The proxy data plane
can override its own observability at runtime via the `MeshConfig` CR. See
[`docs/proposals/015_mesh-config.md`](../docs/proposals/015_mesh-config.md).

Resource names are derived from the **release** name, so install with
`helm install aether …` to get `aether-agent`, `aether-registrar`,
`aether-controller`, etc.

### Images (mirroring)

Each image is configured as `repository` + `digest` (in-repo images, digest-pinned
for immutability). The external proxy image carries `repository` + `tag` + `digest`
— the `aether.image` helper prefers the digest, so the tag is documentation of the
publishing commit, not the reference that is pulled. To mirror to a private
registry, override `repository` alone, e.g.:

```yaml
agent:
  image:
    repository: my-registry.example.com/aether/agent
registrar:
  image:
    repository: my-registry.example.com/aether/registrar
```

For in-repo images the `repository`/`digest` are written as
`{@//path/to:image_push.repository}` / `.digest` placeholders that `rules_helm`
substitutes at package time, so packaged charts are pinned to a concrete digest.

## Versioning & stamping

| Field | Value | Notes |
| --- | --- | --- |
| Chart `version` | e.g. `0.8.0-{GIT_COMMIT}` (crds), `0.3.0-{GIT_COMMIT}` (prober) | SemVer pre-release; commit becomes the OCI tag (dash-separated — `+build` metadata would be rewritten to `_` by helm). The `aether` chart is published **twice**: under its plain `Chart.yaml` version (`<version>`, what Flux and the deploy procedure consume) *and* — via `:aether_commit`, whose `Chart.yaml` is derived from the same file — under `<version>-{GIT_COMMIT}`, so every commit's chart stays addressable after the mutable bare tag has moved on (#692). Bump the chart's `version:` on any change to its templates/values (enforced in CI). These versions move on nearly every release: read the live value from each chart's `Chart.yaml`, or from the version the publish workflow prints. |
| Chart `appVersion` | `{STABLE_GIT_VERSION}` | `git describe` value — matches the binaries' embedded `Version`. |
| Image refs | `repo@sha256:…` | Pinned to the exact built digest (strongest form). |

The `{...}` placeholders in `Chart.yaml` are filled from the workspace status
(`bazel/workspace_status.sh`) **only on `--stamp` builds**:

```bash
bazel build --stamp //charts/aether   # embed git version
bazel run   --stamp //charts/aether:aether.chart_push   # publish a stamped chart
```

Without `--stamp` the braces are stripped (e.g. `0.4.0-GIT_COMMIT`) so the chart
still lints/templates — but release builds should pass `--stamp`. Image digests
are stamped independently of this flag (they always reflect the built image).

## Build & test

```bash
# Package a chart (.tgz under bazel-bin/charts/<name>/)
bazel build //charts/crds //charts/aether

# Lint + `helm template` smoke tests
bazel test //charts/...
```

## Install

CRDs first, then the system:

```bash
bazel run //charts/crds:crds.install
bazel run //charts/aether:aether.install
# Counterparts: .upgrade, .uninstall
```

From the published OCI registry, `quay.io/aethermesh` (use the semver `+`/`-`
form for `--version`; helm maps it to the dash-separated tag). Each chart lives in
its own `chart-<name>` repository.

Do **not** copy a version out of this file — the charts are republished on nearly
every merge. Take `<crds-version>` / `<aether-version>` from the corresponding
`Chart.yaml` at the commit you are installing (or from the versions the publish
workflow prints for that run):

```bash
helm install aether-crds oci://quay.io/aethermesh/chart-crds \
  --version <crds-version>-<git-commit>
# Prefer the commit-pinned tag; the bare `--version <aether-version>` also
# resolves, but that tag is mutable and re-pushed by every release.
helm install aether oci://quay.io/aethermesh/chart-aether \
  --version <aether-version>-<git-commit> -n aether-system --create-namespace
```

> **Moving from ghcr.io (chart 1.0.0, proposal 040).** Until the 1.x charts,
> everything was published to GitHub Container Registry under a different path
> (`charts/<name>` for charts, `aether-proxy` for the proxy image). Every chart
> took a **major** version bump with the move because the default image
> repositories changed: re-point `helm` at the `oci://quay.io/aethermesh/chart-<name>`
> coordinates above, and if you override an image `repository:` by prefix (a
> mirror), mirror from `quay.io/aethermesh/<component>` now — override each
> image's `repository` individually. Nothing publishes to or verifies against
> ghcr.io any more (proposal 040 phase 4); releases from before the move were
> not copied to quay.io.

## Multiple instances & labels

Resource names are release-prefixed (`<release>-agent`, …) and cluster-scoped
resources (ClusterRole/ClusterRoleBinding) additionally include the namespace, so
several releases coexist without collisions. Customize naming with `nameOverride`
/ `fullnameOverride`, and target a namespace with `helm install <release> -n <ns>
[--create-namespace]` (or `namespace.create=true`).

> The **agent** is effectively singleton-per-node by design: it owns host paths
> (`/run/aether`, `/opt/cni/bin`, `/etc/cni/net.d`, …) and the CNI plugin, so only
> one agent release should target a given set of nodes.

All objects carry the [recommended `app.kubernetes.io/*` labels](https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/)
(`name`, `instance`, `version`, `component`, `part-of: aether`, `managed-by`) plus
`helm.sh/chart`. Workload selectors use the immutable subset (`name` + `instance`
+ `component`).

## Publish to Quay (OCI)

Charts and images both publish to the `aethermesh` organisation on quay.io — the
one setting in `bazel/img/registry.bzl` (proposal 040); every coordinate below is
derived from it:

| Artifact | Reference |
| --- | --- |
| crds chart | `quay.io/aethermesh/chart-crds` |
| aether chart | `quay.io/aethermesh/chart-aether` |
| prober chart | `quay.io/aethermesh/chart-prober` |
| udsecho chart | `quay.io/aethermesh/chart-udsecho` |
| agent image | `quay.io/aethermesh/agent` |
| mesh-dns image | `quay.io/aethermesh/mesh-dns` |
| proxy-supervisor image | `quay.io/aethermesh/proxy-supervisor` |
| registrar image | `quay.io/aethermesh/registrar` |
| controller image | `quay.io/aethermesh/controller` |
| cni-install image | `quay.io/aethermesh/cni-install` |
| prober image | `quay.io/aethermesh/prober` |
| udsecho image | `quay.io/aethermesh/udsecho` |
| proxy image | `quay.io/aethermesh/proxy` (built by the `//proxy` workspace, published by `.github/workflows/proxy-release.yml`; the chart's pin moves there with the first proxy release after the cut-over) |

Images and charts are pushed separately, images first. A chart is NOT pushed
with `helm push`: helm appends the chart's `name:` to its base, so it can only
write `<base>/aether`, and Quay has no nested repositories to hold
`charts/aether` (and a bare `prober` / `udsecho` would collide with those
images). `chart_push` (`//bazel/helm:defs.bzl`) writes the identical artifact —
the packaged `.tgz` as the helm chart-content layer, `Chart.yaml` as the helm
config — with the pinned `oras` (`//bazel/oras`) to the repository it names, so
`helm pull oci://quay.io/aethermesh/chart-aether --version <v>` reads it like any
helm-pushed chart.

```bash
docker login quay.io        # image_push and oras both read the Docker config

# The images a chart references (by digest, plus the latest / dev-<sha> tags):
bazel run --stamp //charts/aether:aether.push_images

# The chart, under its packaged version:
bazel run --stamp //charts/aether:aether.chart_push

# The same chart again under `<version>-<git-commit>` (chart only — the images
# are already up as digests). CI does both (#692).
bazel run --stamp //charts/aether:aether_commit.chart_push
```

In CI, `.github/workflows/publish.yaml` does exactly this, logged in with the
Quay robot account (secrets `QUAY_USERNAME` / `QUAY_TOKEN` of the `release`
environment, which only runs on `main`). Never push by hand: the release
workflow is the only publisher. To target a different registry, change
`bazel/img/registry.bzl` — nothing else spells the registry out.
