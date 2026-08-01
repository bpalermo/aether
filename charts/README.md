# Helm charts

Bazel-built Helm charts for Aether, using
[`rules_helm`](https://registry.bazel.build/modules/rules_helm).

| Chart | Path | Deploys |
| --- | --- | --- |
| `crds` | [`charts/crds`](./crds) | Aether CustomResourceDefinitions (`MeshConfig`, `HTTPFilter`, `EdgeConfig`). Install **first**; standalone so CRDs can be upgraded independently. |
| `aether` | [`charts/aether`](./aether) | The whole system: agent DaemonSet (xDS + CNI install) + per-node Envoy proxy, registrar Deployment, and the controller (validating webhooks for `MeshConfig`/`HTTPFilter`/`EdgeConfig`/`HTTPRoute`, a pod-mutating webhook, and the `MeshConfig`→ConfigMap reconciler). Owns the `aether-system` namespace and RBAC. |
| `prober` | [`charts/prober`](./prober) | External mesh-availability prober (proposal 013). |

Agent, registrar and controller deploy together as one system from the single
`aether` chart — system config (OTEL, SPIRE, mesh domain) is set once at the top
level of its `values.yaml` and inherited by every component. The proxy data plane
can override its own observability at runtime via the `MeshConfig` CR. See
[`docs/proposals/015_mesh-config.md`](../docs/proposals/015_mesh-config.md).

Resource names are derived from the **release** name, so install with
`helm install aether …` to get `aether-agent`, `aether-registrar`,
`aether-controller`, etc.

### Images (mirroring)

Each image is configured as `repository` + `digest` (in-repo images, digest-pinned
for immutability) or `repository` + `tag` (the external proxy image). To mirror to
a private registry, override `repository` alone, e.g.:

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
| Chart `version` | e.g. `0.65.0-{GIT_COMMIT}` (aether), `0.6.0-{GIT_COMMIT}` (crds) | SemVer pre-release; commit becomes the OCI tag (dash-separated — `+build` metadata would be rewritten to `_` by helm). Bump the chart's `version:` on any change to its templates/values (enforced in CI). |
| Chart `appVersion` | `{STABLE_GIT_VERSION}` | `git describe` value — matches the binaries' embedded `Version`. |
| Image refs | `repo@sha256:…` | Pinned to the exact built digest (strongest form). |

The `{...}` placeholders in `Chart.yaml` are filled from the workspace status
(`bazel/workspace_status.sh`) **only on `--stamp` builds**:

```bash
bazel build --stamp //charts/aether   # embed git version
bazel run   --stamp //charts/aether:aether.push   # publish a stamped chart
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

From the published OCI registry (use the semver `+`/`-` form for `--version`;
helm maps it to the dash-separated tag):

```bash
helm install aether-crds oci://ghcr.io/bpalermo/aether/charts/crds \
  --version 0.6.0-<git-commit>
helm install aether oci://ghcr.io/bpalermo/aether/charts/aether \
  --version 0.65.0-<git-commit> -n aether-system --create-namespace
```

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

## Publish to GitHub Container Registry (OCI)

Charts and images both publish to GitHub Container Registry under the `aether/`
path namespace:

| Artifact | Reference |
| --- | --- |
| crds chart | `ghcr.io/bpalermo/aether/charts/crds` |
| aether chart | `ghcr.io/bpalermo/aether/charts/aether` |
| agent image | `ghcr.io/bpalermo/aether/agent` |
| registrar image | `ghcr.io/bpalermo/aether/registrar` |
| controller image | `ghcr.io/bpalermo/aether/controller` |
| cni-install image | `ghcr.io/bpalermo/aether/cni-install` |

```bash
export HELM_REGISTRY_USERNAME=<github-user>
export HELM_REGISTRY_PASSWORD=<github-pat>   # PAT with write:packages, or HELM_REGISTRY_PASSWORD_FILE

# Push the chart together with the images it references:
bazel run //charts/aether:aether.push

# Push only the chart (skip images):
bazel run //charts/aether:aether.push_registry
```

The push target performs `helm registry login` automatically when the
`HELM_REGISTRY_USERNAME` / `HELM_REGISTRY_PASSWORD` environment variables are set.
In CI, authenticate to GHCR with the built-in `GITHUB_TOKEN` (`username: ${{ github.actor }}`,
`password: ${{ secrets.GITHUB_TOKEN }}`) and grant the job `packages: write`.
Adjust the `registry_url` / `login_url` in each chart's `BUILD.bazel` to target a
different namespace or registry.
