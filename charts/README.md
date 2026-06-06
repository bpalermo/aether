# Helm charts

Bazel-built Helm charts for Aether, using
[`rules_helm`](https://registry.bazel.build/modules/rules_helm).

| Chart | Path | Deploys |
| --- | --- | --- |
| `agent` | [`charts/agent`](./agent) | Agent DaemonSet (xDS + CNI install) and, optionally, the per-node Envoy proxy DaemonSet. Owns the `aether-system` namespace and RBAC. |
| `registrar` | [`charts/registrar`](./registrar) | Registrar Deployment, Service and RBAC. |

The chart `name` (`agent` / `registrar`) is the trailing path segment of the
published OCI artifact (`ghcr.io/bpalermo/aether/charts/<name>`). Resource names
are derived from the **release** name, so install with `helm install aether …`
to get `aether-agent`, `aether-registrar`, etc.

The container image references in each `values.yaml` are written as
`{@//path/to:image_push}` placeholders. At package time `rules_helm` substitutes
them with the digest-pinned reference produced by the corresponding in-repo
image, so packaged charts are always pinned to a concrete image digest.

## Versioning & stamping

| Field | Value | Notes |
| --- | --- | --- |
| Chart `version` | `0.1.0-{GIT_COMMIT}` | SemVer pre-release; commit becomes the OCI tag (dash-separated — `+build` metadata would be rewritten to `_` by helm). |
| Chart `appVersion` | `{STABLE_GIT_VERSION}` | `git describe` value — matches the binaries' embedded `Version`. |
| Image refs | `repo@sha256:…` | Pinned to the exact built digest (strongest form). |

The `{...}` placeholders in `Chart.yaml` are filled from the workspace status
(`bazel/workspace_status.sh`) **only on `--stamp` builds**:

```bash
bazel build --stamp //charts/agent //charts/registrar   # embed git version
bazel run   --stamp //charts/agent:agent.push           # publish a stamped chart
```

Without `--stamp` the braces are stripped (e.g. `0.1.0-GIT_COMMIT`) so the chart
still lints/templates — but release builds should pass `--stamp`. Image digests
are stamped independently of this flag (they always reflect the built image), and
the published image tags themselves already embed the git commit.

## Build & test

```bash
# Package a chart (.tgz under bazel-bin/charts/<name>/)
bazel build //charts/agent //charts/registrar

# Lint + `helm template` smoke tests
bazel test //charts/...
```

## Install

From the local Bazel build:

```bash
bazel run //charts/agent:agent.install
bazel run //charts/registrar:registrar.install
# Counterparts: .upgrade, .uninstall
```

From the published OCI registry (use the semver `+`/`-` form for `--version`;
helm maps it to the dash-separated tag):

```bash
helm install aether oci://ghcr.io/bpalermo/aether/charts/agent \
  --version 0.1.0-<git-commit> -n aether-system
helm install aether-registrar oci://ghcr.io/bpalermo/aether/charts/registrar \
  --version 0.1.0-<git-commit> -n aether-system
```

## Multiple instances & labels

Resource names are release-prefixed (`<release>-agent`, …) and
cluster-scoped resources (ClusterRole/ClusterRoleBinding) additionally include
the namespace, so several releases coexist without collisions. Customize naming
with `nameOverride` / `fullnameOverride`, and target a namespace with
`helm install <release> -n <ns> [--create-namespace]` (or `namespace.create=true`).

> The **agent** is effectively singleton-per-node by design: it owns host paths
> (`/run/aether`, `/opt/cni/bin`, `/etc/cni/net.d`, …) and the CNI plugin, so only
> one agent release should target a given set of nodes. The **registrar** is an
> ordinary Deployment and runs fine as multiple instances.

All objects carry the [recommended `app.kubernetes.io/*` labels](https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/)
(`name`, `instance`, `version`, `component`, `part-of: aether`, `managed-by`) plus
`helm.sh/chart`. Workload selectors use the immutable subset (`name` + `instance`
+ `component`).

## Publish to GitHub Container Registry (OCI)

Charts and images both publish to GitHub Container Registry under the `aether/`
path namespace:

| Artifact | Reference |
| --- | --- |
| agent chart | `ghcr.io/bpalermo/aether/charts/agent` |
| registrar chart | `ghcr.io/bpalermo/aether/charts/registrar` |
| agent image | `ghcr.io/bpalermo/aether/agent` |
| registrar image | `ghcr.io/bpalermo/aether/registrar` |
| cni-install image | `ghcr.io/bpalermo/aether/cni-install` |

```bash
export HELM_REGISTRY_USERNAME=<github-user>
export HELM_REGISTRY_PASSWORD=<github-pat>   # PAT with write:packages, or HELM_REGISTRY_PASSWORD_FILE

# Push the chart together with the images it references:
bazel run //charts/agent:agent.push
bazel run //charts/registrar:registrar.push

# Push only the chart (skip images):
bazel run //charts/agent:agent.push_registry
bazel run //charts/registrar:registrar.push_registry
```

The push target performs `helm registry login` automatically when the
`HELM_REGISTRY_USERNAME` / `HELM_REGISTRY_PASSWORD` environment variables are set.
In CI, authenticate to GHCR with the built-in `GITHUB_TOKEN` (`username: ${{ github.actor }}`,
`password: ${{ secrets.GITHUB_TOKEN }}`) and grant the job `packages: write`.
Adjust the `registry_url` / `login_url` in each chart's `BUILD.bazel` to target a
different namespace or registry.
